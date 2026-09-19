package executor

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	codexTurnStateTicketDefaultTargetLength         = 292
	codexTurnStateTicketDefaultTTL                  = time.Hour
	codexTurnStateTicketDefaultRefreshBefore        = 10 * time.Minute
	codexTurnStateTicketDefaultProbeInterval        = 6 * time.Second
	codexTurnStateTicketDefaultAttemptTimeout       = 25 * time.Second
	codexTurnStateTicketDefaultMaxEntries           = 4096
	codexTurnStateTicketDefaultHarvestWorkers       = 16
	codexTurnStateTicketStatePrefix                 = "gAAAAA"
	codexTurnStateTicketDefaultModelAstra           = "gpt-6-astra"
	codexTurnStateTicketDefaultModelSol             = "gpt-5.6-sol"
	codexTurnStateTicketHarvestEndpoint             = "https://chatgpt.com/backend-api/codex/responses"
	codexTurnStateTicketHarvestProxyRequiredMessage = "codex turn-state ticket harvest proxy is not configured"
	codexTurnStateTicketUnavailableMessage          = "codex turn-state ticket unavailable"
)

// codexTurnStateTicketHarvestURL is a variable so protocol tests can point the
// probe at an httptest server without changing the production endpoint.
var codexTurnStateTicketHarvestURL = codexTurnStateTicketHarvestEndpoint

const codexTurnStateTicketDiagnosticBodyLimit = 16 << 10

type codexTurnStateTicketProbeDiagnostic struct {
	RequestMethod  string            `json:"request_method"`
	RequestURL     string            `json:"request_url"`
	Proxy          string            `json:"proxy,omitempty"`
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	// RequestBody is retained for structured server-side logging, but is omitted
	// from the diagnostic string returned to management callers.
	RequestBody           string            `json:"-"`
	ResponseStatus        int               `json:"response_status,omitempty"`
	ResponseHeaders       map[string]string `json:"response_headers,omitempty"`
	ResponseBody          string            `json:"response_body,omitempty"`
	ResponseBodyReadError string            `json:"response_body_read_error,omitempty"`
	TurnStateLength       int               `json:"turn_state_length"`
	TurnStatePrefixValid  bool              `json:"turn_state_prefix_valid"`
}

// errCodexTurnStateTicketUnavailable is returned only when fail-closed mode is
// explicitly enabled. The ticket value is never included in this error.
var errCodexTurnStateTicketUnavailable = errors.New(codexTurnStateTicketUnavailableMessage)

type codexTurnStateTicket struct {
	model      string
	state      string
	capturedAt time.Time
	expiresAt  time.Time
}

func (t *codexTurnStateTicket) valid(now time.Time, targetLength int) bool {
	if t == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	state := strings.TrimSpace(t.state)
	if !codexTurnStateTicketStateValid(state, targetLength) {
		return false
	}
	return !t.expiresAt.IsZero() && now.Before(t.expiresAt)
}

func (t *codexTurnStateTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.expiresAt.IsZero() {
		return true
	}
	return !t.expiresAt.After(now.Add(refreshBefore))
}

type codexTurnStateTicketEntry struct {
	ticket *codexTurnStateTicket
	elem   *list.Element
}

// codexTurnStateTicketStore keeps ephemeral tickets bounded and account-scoped.
// It intentionally has no persistence hook: x-codex-turn-state is a short-lived
// upstream capability and copying it into auth files would widen its exposure.
type codexTurnStateTicketStore struct {
	mu         sync.Mutex
	entries    map[string]*codexTurnStateTicketEntry
	recency    *list.List
	maxEntries int
}

func newCodexTurnStateTicketStore() *codexTurnStateTicketStore {
	return &codexTurnStateTicketStore{
		entries:    make(map[string]*codexTurnStateTicketEntry),
		recency:    list.New(),
		maxEntries: codexTurnStateTicketDefaultMaxEntries,
	}
}

func (s *codexTurnStateTicketStore) get(key string, now time.Time, targetLength int) *codexTurnStateTicket {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	entry := s.entries[key]
	if entry == nil || !entry.ticket.valid(now, targetLength) {
		if entry != nil {
			s.removeLocked(key, entry)
		}
		return nil
	}
	if entry.elem != nil {
		s.recency.MoveToFront(entry.elem)
	}
	return entry.ticket
}

func (s *codexTurnStateTicketStore) put(key string, ticket *codexTurnStateTicket) {
	key = strings.TrimSpace(key)
	if s == nil || key == "" || ticket == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.entries[key]; old != nil {
		s.removeLocked(key, old)
	}
	entry := &codexTurnStateTicketEntry{ticket: ticket}
	entry.elem = s.recency.PushFront(key)
	s.entries[key] = entry
	for len(s.entries) > s.maxEntries {
		oldest := s.recency.Back()
		if oldest == nil {
			break
		}
		oldKey, _ := oldest.Value.(string)
		if oldEntry := s.entries[oldKey]; oldEntry != nil {
			s.removeLocked(oldKey, oldEntry)
		} else {
			s.recency.Remove(oldest)
		}
	}
}

func (s *codexTurnStateTicketStore) delete(key string) {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[key]; entry != nil {
		s.removeLocked(key, entry)
	}
}

func (s *codexTurnStateTicketStore) deleteAuth(authID string) {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return
	}
	prefix := authID + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if strings.HasPrefix(key, prefix) {
			s.removeLocked(key, entry)
		}
	}
}

func (s *codexTurnStateTicketStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries = make(map[string]*codexTurnStateTicketEntry)
	s.recency.Init()
	s.mu.Unlock()
}

func (s *codexTurnStateTicketStore) cleanupLocked(now time.Time) {
	if s == nil || s.recency == nil {
		return
	}
	for elem := s.recency.Back(); elem != nil; {
		previous := elem.Prev()
		key, _ := elem.Value.(string)
		entry := s.entries[key]
		if entry == nil || entry.ticket == nil || entry.ticket.expiresAt.IsZero() || !now.Before(entry.ticket.expiresAt) {
			if entry != nil {
				s.removeLocked(key, entry)
			} else {
				s.recency.Remove(elem)
			}
		}
		elem = previous
	}
}

func (s *codexTurnStateTicketStore) removeLocked(key string, entry *codexTurnStateTicketEntry) {
	if s == nil || entry == nil {
		return
	}
	delete(s.entries, key)
	if entry.elem != nil {
		s.recency.Remove(entry.elem)
		entry.elem = nil
	}
}

type codexTurnStateTicketProvider struct {
	cfg         *config.Config
	tickets     *codexTurnStateTicketStore
	flights     helps.InFlightGroup[*codexTurnStateTicket]
	lifecycleMu sync.Mutex
	manager     *cliproxyauth.Manager
	cancel      context.CancelFunc
	done        chan struct{}
}

func newCodexTurnStateTicketProvider(cfg *config.Config) *codexTurnStateTicketProvider {
	return &codexTurnStateTicketProvider{
		cfg:     cfg,
		tickets: newCodexTurnStateTicketStore(),
	}
}

func codexTurnStateTicketConfigForConfig(cfg *config.Config) config.CodexTurnStateTicketConfig {
	var out config.CodexTurnStateTicketConfig
	if cfg != nil {
		out = cfg.Codex.OpenAICodexTicket
	}
	if out.TargetLength <= 0 {
		out.TargetLength = codexTurnStateTicketDefaultTargetLength
	}
	if out.TTLSeconds <= 0 {
		out.TTLSeconds = int(codexTurnStateTicketDefaultTTL / time.Second)
	}
	if out.RefreshBeforeSeconds <= 0 {
		out.RefreshBeforeSeconds = int(codexTurnStateTicketDefaultRefreshBefore / time.Second)
	}
	if out.HarvestProbeIntervalSeconds <= 0 {
		out.HarvestProbeIntervalSeconds = int(codexTurnStateTicketDefaultProbeInterval / time.Second)
	}
	if out.HarvestAttemptTimeoutSeconds <= 0 {
		out.HarvestAttemptTimeoutSeconds = int(codexTurnStateTicketDefaultAttemptTimeout / time.Second)
	}
	out.HarvestProxyURL = strings.TrimSpace(out.HarvestProxyURL)
	if len(out.Models) == 0 {
		out.Models = []string{codexTurnStateTicketDefaultModelAstra}
	} else {
		models := make([]string, 0, len(out.Models))
		seen := make(map[string]struct{}, len(out.Models))
		for _, model := range out.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			models = append(models, model)
		}
		// The original default pair included Sol, but Ticket harvesting is
		// currently supported only for Astra. Treat that exact legacy default
		// pair as Astra-only while preserving explicitly configured custom models.
		if len(models) == 2 && models[0] == codexTurnStateTicketDefaultModelAstra && models[1] == codexTurnStateTicketDefaultModelSol {
			models = []string{codexTurnStateTicketDefaultModelAstra}
		}
		out.Models = models
	}
	if len(out.AuthFiles) > 0 {
		selectors := make([]string, 0, len(out.AuthFiles))
		seen := make(map[string]struct{}, len(out.AuthFiles))
		for _, selector := range out.AuthFiles {
			selector = strings.TrimSpace(selector)
			if selector == "" {
				continue
			}
			if _, ok := seen[selector]; ok {
				continue
			}
			seen[selector] = struct{}{}
			selectors = append(selectors, selector)
		}
		out.AuthFiles = selectors
	}
	return out
}

func codexTurnStateTicketKey(auth *cliproxyauth.Auth, model string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return ""
	}
	return authID + "\x00" + model
}

func codexTurnStateTicketModelAllowed(cfg config.CodexTurnStateTicketConfig, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, configured := range cfg.Models {
		if strings.TrimSpace(configured) == model {
			return true
		}
	}
	return false
}

func codexTurnStateTicketAuthAllowed(cfg config.CodexTurnStateTicketConfig, auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(cfg.AuthFiles) == 0 {
		return true
	}
	for _, selector := range cfg.AuthFiles {
		if codexTurnStateTicketAuthSelectorMatches(auth, selector) {
			return true
		}
	}
	return false
}

func codexTurnStateTicketAuthSelectorMatches(auth *cliproxyauth.Auth, selector string) bool {
	if auth == nil {
		return false
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	if selector == strings.TrimSpace(auth.ID) {
		return true
	}

	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		return false
	}
	if selector == fileName || selector == filepath.Base(fileName) {
		return true
	}
	cleanSelector := filepath.Clean(filepath.FromSlash(selector))
	cleanFileName := filepath.Clean(filepath.FromSlash(fileName))
	if cleanSelector == cleanFileName || cleanSelector == filepath.Base(cleanFileName) {
		return true
	}
	return false
}

func codexTurnStateTicketOAuthAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || auth.IsDisabled() {
		return false
	}
	if codexIsAPIKeyAuth(auth) || codexIsAgentIdentityAuth(auth) {
		return false
	}
	accessToken := strings.TrimSpace(metadataString(auth.Metadata, "access_token", "accessToken"))
	if accessToken == "" {
		return false
	}
	_, baseURL := codexCreds(auth)
	return codexTurnStateTicketBaseURLAllowed(baseURL)
}

func codexTurnStateTicketBaseURLAllowed(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "chatgpt.com") {
		return false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return path == "/backend-api/codex"
}

func codexTurnStateTicketRequestURLAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "chatgpt.com") {
		return false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return path == "/backend-api/codex/responses"
}

func (p *codexTurnStateTicketProvider) setAuthManager(manager *cliproxyauth.Manager) {
	if p == nil {
		return
	}
	p.lifecycleMu.Lock()
	p.manager = manager
	shouldStart := manager != nil && p.config().Enabled && p.config().HarvestProxyURL != "" && p.cancel == nil
	if !shouldStart {
		p.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	p.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		p.harvestLoop(ctx)
	}()
	log.WithFields(log.Fields{
		"interval_seconds": p.config().HarvestProbeIntervalSeconds,
		"target_length":    p.config().TargetLength,
		"models":           p.config().Models,
	}).Info("codex turn-state ticket harvester started")
}

func (p *codexTurnStateTicketProvider) stop() {
	if p == nil {
		return
	}
	p.lifecycleMu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done, p.manager = nil, nil, nil
	p.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	p.tickets.clear()
}

func (p *codexTurnStateTicketProvider) config() config.CodexTurnStateTicketConfig {
	if p == nil {
		return codexTurnStateTicketConfigForConfig(nil)
	}
	return codexTurnStateTicketConfigForConfig(p.cfg)
}

func (p *codexTurnStateTicketProvider) managerSnapshot() *cliproxyauth.Manager {
	if p == nil {
		return nil
	}
	p.lifecycleMu.Lock()
	manager := p.manager
	p.lifecycleMu.Unlock()
	return manager
}

func (p *codexTurnStateTicketProvider) harvestLoop(ctx context.Context) {
	interval := time.Duration(p.config().HarvestProbeIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = codexTurnStateTicketDefaultProbeInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			p.refresh(ctx)
			timer.Reset(interval)
		}
	}
}

func (p *codexTurnStateTicketProvider) refresh(ctx context.Context) {
	if p == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	cfg := p.config()
	if !cfg.Enabled || cfg.HarvestProxyURL == "" {
		return
	}
	manager := p.managerSnapshot()
	if manager == nil {
		return
	}
	auths := manager.ListByProvider("codex")
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	type harvestJob struct {
		auth  *cliproxyauth.Auth
		model string
	}
	jobs := make([]harvestJob, 0, len(auths)*len(cfg.Models))
	for _, auth := range auths {
		if !codexTurnStateTicketOAuthAuth(auth) || !codexTurnStateTicketAuthAllowed(cfg, auth) {
			continue
		}
		for _, model := range cfg.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			key := codexTurnStateTicketKey(auth, model)
			if key == "" {
				continue
			}
			if ticket := p.tickets.get(key, now, cfg.TargetLength); ticket != nil && !ticket.needsRefresh(now, refreshBefore) {
				continue
			}
			jobs = append(jobs, harvestJob{auth: auth, model: model})
		}
	}
	if len(jobs) == 0 {
		return
	}
	workerCount := codexTurnStateTicketDefaultHarvestWorkers
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	jobCh := make(chan harvestJob)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobCh:
					if !ok {
						return
					}
					p.probeOnce(ctx, job.auth, job.model)
				}
			}
		}()
	}
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			break
		case jobCh <- job:
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobCh)
	wg.Wait()
}

func (p *codexTurnStateTicketProvider) keyFor(auth *cliproxyauth.Auth, model string) string {
	return codexTurnStateTicketKey(auth, model)
}

func (p *codexTurnStateTicketProvider) refreshModel(ctx context.Context, auth *cliproxyauth.Auth, model string) error {
	if p == nil || ctx == nil || !codexTurnStateTicketOAuthAuth(auth) {
		return errors.New("codex turn-state ticket refresh is unavailable")
	}
	cfg := p.config()
	model = strings.TrimSpace(model)
	if !cfg.Enabled || strings.TrimSpace(cfg.HarvestProxyURL) == "" || !codexTurnStateTicketAuthAllowed(cfg, auth) || !codexTurnStateTicketModelAllowed(cfg, model) {
		return errors.New("codex turn-state ticket refresh is not allowed")
	}
	key := codexTurnStateTicketKey(auth, model)
	p.tickets.delete(key)
	token, _ := codexCreds(auth)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("model %s: OAuth access token is missing", model)
	}
	state, status, err := p.fireProbe(ctx, auth, token, model)
	if err == nil && status == http.StatusUnauthorized {
		if manager := p.managerSnapshot(); manager != nil {
			refreshCtx := helps.WithProxyOverride(ctx, p.config().HarvestProxyURL)
			if refreshed, refreshErr := manager.RefreshAuth(refreshCtx, auth); refreshErr != nil {
				return fmt.Errorf("model %s: OAuth refresh failed: %w", model, refreshErr)
			} else if refreshed != nil {
				auth = refreshed
				token, _ = codexCreds(auth)
				state, status, err = p.fireProbe(ctx, auth, token, model)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("model %s: harvest request failed: %w", model, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("model %s: harvest upstream returned HTTP %d", model, status)
	}
	trimmedState := strings.TrimSpace(state)
	if trimmedState == "" {
		return fmt.Errorf("model %s: harvest response is HTTP 200 but missing x-codex-turn-state", model)
	}
	if !codexTurnStateTicketStateValid(trimmedState, cfg.TargetLength) {
		return fmt.Errorf("model %s: x-codex-turn-state length is %d, accepted lengths %s", model, len(trimmedState), codexTurnStateTicketAcceptedLengths(cfg.TargetLength))
	}
	if !strings.HasPrefix(trimmedState, codexTurnStateTicketStatePrefix) {
		return fmt.Errorf("model %s: x-codex-turn-state has unexpected prefix", model)
	}
	now := time.Now()
	p.tickets.put(key, &codexTurnStateTicket{model: model, state: state, capturedAt: now, expiresAt: now.Add(time.Duration(cfg.TTLSeconds) * time.Second)})
	return nil
}

func (p *codexTurnStateTicketProvider) refreshAuth(ctx context.Context, auth *cliproxyauth.Auth) error {
	if p == nil || ctx == nil || !codexTurnStateTicketOAuthAuth(auth) {
		return errors.New("codex turn-state ticket refresh is unavailable")
	}
	cfg := p.config()
	if !cfg.Enabled || strings.TrimSpace(cfg.HarvestProxyURL) == "" {
		return errors.New("codex turn-state ticket harvest is disabled or proxy is not configured")
	}
	if !codexTurnStateTicketAuthAllowed(cfg, auth) {
		return errors.New("auth file is not enabled for codex turn-state ticket harvest")
	}
	var refreshed int
	var lastErr error
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model == "" || !codexTurnStateTicketModelAllowed(cfg, model) {
			continue
		}
		token, _ := codexCreds(auth)
		state, status, err := p.fireProbe(ctx, auth, token, model)
		if err == nil && status == http.StatusUnauthorized {
			if manager := p.managerSnapshot(); manager != nil {
				refreshCtx := helps.WithProxyOverride(ctx, p.config().HarvestProxyURL)
				if updated, refreshErr := manager.RefreshAuth(refreshCtx, auth); refreshErr == nil && updated != nil {
					auth = updated
					token, _ = codexCreds(auth)
					state, status, err = p.fireProbe(ctx, auth, token, model)
				}
			}
		}
		if err != nil {
			lastErr = fmt.Errorf("model %s: harvest request failed: %w", model, err)
			continue
		}
		if status != http.StatusOK {
			lastErr = fmt.Errorf("model %s: harvest upstream returned HTTP %d", model, status)
			continue
		}
		if !codexTurnStateTicketStateValid(state, cfg.TargetLength) {
			lastErr = fmt.Errorf("model %s: harvest response did not contain a valid x-codex-turn-state", model)
			continue
		}
		now := time.Now()
		p.tickets.put(codexTurnStateTicketKey(auth, model), &codexTurnStateTicket{model: model, state: state, capturedAt: now, expiresAt: now.Add(time.Duration(cfg.TTLSeconds) * time.Second)})
		refreshed++
	}
	if refreshed == 0 {
		if lastErr != nil {
			return lastErr
		}
		return errors.New("no turn-state ticket was harvested")
	}
	return nil
}

func (p *codexTurnStateTicketProvider) probeOnce(ctx context.Context, auth *cliproxyauth.Auth, model string) {
	cfg := p.config()
	if p == nil || ctx == nil || ctx.Err() != nil || !cfg.Enabled || !codexTurnStateTicketOAuthAuth(auth) || !codexTurnStateTicketAuthAllowed(cfg, auth) {
		return
	}
	key := codexTurnStateTicketKey(auth, model)
	if key == "" {
		return
	}
	_, _, _, _ = p.flights.Do(ctx, key, func() (*codexTurnStateTicket, error) {
		currentAuth := auth
		token, _ := codexCreds(currentAuth)
		if strings.TrimSpace(token) == "" {
			return nil, nil
		}
		state, status, err := p.fireProbe(ctx, currentAuth, token, model)
		if err != nil {
			log.Debugf("codex turn-state ticket probe failed for auth %s model %s via %s", auth.ID, model, proxyutil.Redact(p.config().HarvestProxyURL))
			return nil, nil
		}
		if status == http.StatusUnauthorized {
			if manager := p.managerSnapshot(); manager != nil {
				refreshCtx := helps.WithProxyOverride(ctx, p.config().HarvestProxyURL)
				if refreshed, refreshErr := manager.RefreshAuth(refreshCtx, currentAuth); refreshErr == nil && refreshed != nil {
					currentAuth = refreshed
					token, _ = codexCreds(currentAuth)
					if token != "" {
						state, status, err = p.fireProbe(ctx, currentAuth, token, model)
					}
				}
			}
		}
		if err != nil || status != http.StatusOK || !codexTurnStateTicketStateValid(state, cfg.TargetLength) {
			return nil, nil
		}
		now := time.Now()
		ticket := &codexTurnStateTicket{
			model:      strings.TrimSpace(model),
			state:      state,
			capturedAt: now,
			expiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
		}
		p.tickets.put(key, ticket)
		log.Debugf("codex turn-state ticket harvested for auth %s model %s", auth.ID, model)
		return ticket, nil
	})
}

func codexTurnStateTicketStateValid(state string, targetLength int) bool {
	state = strings.TrimSpace(state)
	if targetLength <= 0 || !strings.HasPrefix(state, codexTurnStateTicketStatePrefix) {
		return false
	}
	if len(state) == targetLength {
		return true
	}
	return false
}

func codexTurnStateTicketAcceptedLengths(targetLength int) string {
	return fmt.Sprintf("%d", targetLength)
}

func codexTurnStateTicketResponseModel(body []byte) string {
	const marker = `"model":"`
	text := string(body)
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	value := text[index+len(marker):]
	if end := strings.IndexByte(value, '"'); end >= 0 {
		return strings.TrimSpace(value[:end])
	}
	return ""
}

func (p *codexTurnStateTicketProvider) fireProbe(ctx context.Context, auth *cliproxyauth.Auth, token, model string) (string, int, error) {
	if p == nil {
		return "", 0, errors.New("nil ticket provider")
	}
	cfg := p.config()
	proxyURL := strings.TrimSpace(cfg.HarvestProxyURL)
	if proxyURL == "" {
		return "", 0, errors.New(codexTurnStateTicketHarvestProxyRequiredMessage)
	}
	transport, mode, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return "", 0, fmt.Errorf("invalid harvest proxy: %w", err)
	}
	if mode == proxyutil.ModeInherit || mode == proxyutil.ModeInvalid {
		return "", 0, errors.New("harvest proxy must be explicit; refusing inherited proxy settings")
	}
	if transport == nil {
		return "", 0, errors.New("ticket harvest proxy transport is unavailable")
	}
	transport.DisableKeepAlives = true
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 0
	defer transport.CloseIdleConnections()

	attemptTimeout := time.Duration(cfg.HarvestAttemptTimeoutSeconds) * time.Second
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":        strings.TrimSpace(model),
		"store":        false,
		"stream":       true,
		"instructions": "Reply with exactly: pong",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "ping",
			}},
		}},
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, codexTurnStateTicketHarvestURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Close = true
	req.Host = "chatgpt.com"
	applyCodexHeadersForRequestKind(req, auth, token, true, p.cfg, codexFinalUpstreamResponses)
	req.Header.Del(codexHeaderTurnState)
	req.Header.Set(codexHeaderOpenAIBeta, "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	diagnostic := codexTurnStateTicketProbeDiagnostic{
		RequestMethod:  req.Method,
		RequestURL:     req.URL.String(),
		Proxy:          proxyutil.Redact(proxyURL),
		RequestHeaders: redactCodexTurnStateTicketHeaders(req.Header),
		RequestBody:    string(body),
	}

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		log.WithFields(log.Fields{
			"model": model, "method": req.Method, "url": req.URL.String(),
			"proxy": proxyutil.Redact(proxyURL), "request_headers": diagnostic.RequestHeaders,
			"request_body": diagnostic.RequestBody,
		}).Warnf("codex turn-state ticket harvest request failed: %v", err)
		return "", 0, errors.New("harvest upstream request failed")
	}
	if resp == nil {
		return "", 0, errors.New("nil ticket harvest response")
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, codexTurnStateTicketDiagnosticBodyLimit))
	state := strings.TrimSpace(resp.Header.Get(codexHeaderTurnState))
	diagnostic.ResponseStatus = resp.StatusCode
	diagnostic.ResponseHeaders = redactCodexTurnStateTicketHeaders(resp.Header)
	diagnostic.ResponseBody = string(responseBody)
	diagnostic.TurnStateLength = len(state)
	diagnostic.TurnStatePrefixValid = strings.HasPrefix(state, codexTurnStateTicketStatePrefix)
	if readErr != nil {
		diagnostic.ResponseBodyReadError = readErr.Error()
	}
	fields := log.Fields{
		"model": model, "method": req.Method, "url": req.URL.String(),
		"proxy": proxyutil.Redact(proxyURL), "request_headers": diagnostic.RequestHeaders,
		"request_body": diagnostic.RequestBody, "response_status": diagnostic.ResponseStatus,
		"response_headers": diagnostic.ResponseHeaders, "turn_state_length": diagnostic.TurnStateLength,
		"turn_state_prefix_valid": diagnostic.TurnStatePrefixValid, "response_body": diagnostic.ResponseBody,
	}
	if readErr != nil {
		fields["response_body_read_error"] = readErr.Error()
	}
	log.WithFields(fields).Error("codex turn-state ticket harvest exchange")
	if readErr != nil && state == "" {
		return state, resp.StatusCode, fmt.Errorf("read harvest response body: %w", readErr)
	}
	if resp.StatusCode == http.StatusOK && state == "" {
		return state, resp.StatusCode, fmt.Errorf("harvest response is HTTP 200 but missing %s", codexHeaderTurnState)
	}
	if resp.StatusCode == http.StatusOK && !codexTurnStateTicketStateValid(state, cfg.TargetLength) {
		return state, resp.StatusCode, fmt.Errorf("harvest response contains invalid %s: length=%d accepted_lengths=%s prefix_valid=%t", codexHeaderTurnState, len(state), codexTurnStateTicketAcceptedLengths(cfg.TargetLength), diagnostic.TurnStatePrefixValid)
	}
	if resp.StatusCode == http.StatusOK {
		if responseModel := codexTurnStateTicketResponseModel(responseBody); responseModel != "" && !strings.EqualFold(responseModel, strings.TrimSpace(model)) {
			return state, resp.StatusCode, fmt.Errorf("harvest response model mismatch: requested=%q returned=%q; refusing to cache Ticket", strings.TrimSpace(model), responseModel)
		}
	}
	return state, resp.StatusCode, nil
}

func redactCodexTurnStateTicketHeaders(headers http.Header) map[string]string {
	result := make(map[string]string, len(headers))
	for name, values := range headers {
		value := strings.Join(values, ", ")
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "api-key", "x-api-key", "openai-api-key", "x-goog-api-key", strings.ToLower(codexHeaderTurnState):
			value = "[REDACTED]"
		}
		if len(value) > 2048 {
			value = value[:2048] + "...[truncated]"
		}
		result[name] = value
	}
	return result
}

func (p *codexTurnStateTicketProvider) apply(ctx context.Context, auth *cliproxyauth.Auth, model string, headers http.Header, overwriteExisting bool) error {
	if p == nil || headers == nil {
		return nil
	}
	cfg := p.config()
	if !cfg.Enabled || !codexTurnStateTicketOAuthAuth(auth) || !codexTurnStateTicketAuthAllowed(cfg, auth) || !codexTurnStateTicketModelAllowed(cfg, model) {
		return nil
	}
	if strings.TrimSpace(headers.Get(codexHeaderTurnState)) != "" && !overwriteExisting {
		return nil
	}
	key := codexTurnStateTicketKey(auth, model)
	ticket := p.tickets.get(key, time.Now(), cfg.TargetLength)
	if ticket == nil && cfg.FailClosed {
		// A request can arrive before the background harvester's first tick or
		// immediately after a credential refresh. Make one bounded, single-flight
		// probe here so fail-closed mode does not reject a recoverable cold cache.
		attemptTimeout := time.Duration(cfg.HarvestAttemptTimeoutSeconds) * time.Second
		if attemptTimeout <= 0 {
			attemptTimeout = codexTurnStateTicketDefaultAttemptTimeout
		}
		probeCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		p.probeOnce(probeCtx, auth, model)
		cancel()
		ticket = p.tickets.get(key, time.Now(), cfg.TargetLength)
	}
	if ticket != nil {
		headers.Set(codexHeaderTurnState, ticket.state)
		return nil
	}
	if !cfg.FailClosed {
		return nil
	}
	return fmt.Errorf("%w: auth %q model %q", errCodexTurnStateTicketUnavailable, strings.TrimSpace(auth.ID), strings.TrimSpace(model))
}

func (p *codexTurnStateTicketProvider) deleteAuth(authID string) {
	if p != nil && p.tickets != nil {
		p.tickets.deleteAuth(authID)
	}
}

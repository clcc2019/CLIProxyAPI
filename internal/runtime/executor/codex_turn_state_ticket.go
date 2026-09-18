package executor

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if targetLength <= 0 || len(state) != targetLength || !strings.HasPrefix(state, codexTurnStateTicketStatePrefix) {
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
		out.Models = []string{codexTurnStateTicketDefaultModelAstra, codexTurnStateTicketDefaultModelSol}
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
	var wg sync.WaitGroup
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
			wg.Add(1)
			go func(auth *cliproxyauth.Auth, model string) {
				defer wg.Done()
				p.probeOnce(ctx, auth, model)
			}(auth, model)
		}
	}
	wg.Wait()
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
				if refreshed, refreshErr := manager.RefreshAuth(ctx, currentAuth); refreshErr == nil && refreshed != nil {
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
	return targetLength > 0 && len(state) == targetLength && strings.HasPrefix(state, codexTurnStateTicketStatePrefix)
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
	transport, _, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return "", 0, err
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

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, errors.New("nil ticket harvest response")
	}
	defer resp.Body.Close()
	return strings.TrimSpace(resp.Header.Get(codexHeaderTurnState)), resp.StatusCode, nil
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

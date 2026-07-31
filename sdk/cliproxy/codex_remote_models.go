package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexRemoteCatalogTTL        = 5 * time.Minute
	codexRemoteCatalogTimeout    = 15 * time.Second
	codexRemoteCatalogMaxBytes   = 8 << 20
	codexRemoteCatalogETagHeader = "ETag"
)

type codexRemoteCatalogCacheEntry struct {
	payload   []byte
	fetchedAt time.Time
	sourceKey string
	etag      string
}

func (entry codexRemoteCatalogCacheEntry) fresh(now time.Time, sourceKey string) bool {
	return len(entry.payload) > 0 && entry.sourceKey == sourceKey && now.Sub(entry.fetchedAt) < codexRemoteCatalogTTL
}

func (s *Service) refreshCodexRemoteCatalog(ctx context.Context, auth *coreauth.Auth) error {
	_, err := s.refreshCodexRemoteCatalogWithETag(ctx, auth, "")
	return err
}

// refreshCodexRemoteCatalogWithETag mirrors the official Codex client: a
// response ETag that differs from the cached /models ETag forces one online
// refresh, while a matching ETag only renews the five-minute cache lifetime.
func (s *Service) refreshCodexRemoteCatalogWithETag(ctx context.Context, auth *coreauth.Auth, responseETag string) (bool, error) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" || auth.IsDisabled() {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false, nil
	}
	if !codexServiceHasAccessToken(auth) && !codexServiceAuthIsAgentIdentity(auth) && !codexServiceAuthIsAPIKey(auth) {
		return false, nil
	}
	responseETag = strings.TrimSpace(responseETag)
	baseURL := codexServiceBaseURL(auth)
	sourceKey := codexRemoteCatalogSourceKey(auth, baseURL)
	now := time.Now()
	if cached, ok := s.codexRemoteCatalogs.Load(auth.ID); ok {
		if entry, okEntry := cached.(codexRemoteCatalogCacheEntry); okEntry && entry.fresh(now, sourceKey) && (responseETag == "" || entry.etag == responseETag) {
			if responseETag != "" {
				entry.fetchedAt = now
				s.codexRemoteCatalogs.Store(auth.ID, entry)
			}
			return false, nil
		}
	}

	flightKey := auth.ID + "\x00" + sourceKey
	value, err, _ := s.codexRemoteCatalogRefreshes.Do(flightKey, func() (any, error) {
		now = time.Now()
		if cached, ok := s.codexRemoteCatalogs.Load(auth.ID); ok {
			if entry, okEntry := cached.(codexRemoteCatalogCacheEntry); okEntry && entry.fresh(now, sourceKey) && (responseETag == "" || entry.etag == responseETag) {
				if responseETag != "" {
					entry.fetchedAt = now
					s.codexRemoteCatalogs.Store(auth.ID, entry)
				}
				return false, nil
			}
		}

		targetURL := strings.TrimSuffix(baseURL, "/") + "/models?client_version=" + url.QueryEscape(misc.CodexCLIVersion)
		requestCtx := ctx
		if requestCtx == nil {
			requestCtx = context.Background()
		}
		requestCtx, cancel := context.WithTimeout(requestCtx, codexRemoteCatalogTimeout)
		defer cancel()
		req, errRequest := http.NewRequestWithContext(requestCtx, http.MethodGet, targetURL, nil)
		if errRequest != nil {
			return nil, errRequest
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", misc.CodexCLIUserAgent)
		req.Header.Set("Version", misc.CodexCLIVersion)
		req.Header.Set("Originator", misc.CodexCLIOriginator)
		if accountID := codexServiceAccountID(auth); accountID != "" {
			req.Header.Set("ChatGPT-Account-ID", accountID)
		}
		resp, errRequest := s.coreManager.HttpRequest(requestCtx, auth, req)
		if errRequest != nil {
			return nil, errRequest
		}
		if resp == nil {
			return nil, fmt.Errorf("Codex model catalog returned no response")
		}
		defer resp.Body.Close()
		body, errRead := io.ReadAll(io.LimitReader(resp.Body, codexRemoteCatalogMaxBytes+1))
		if errRead != nil {
			return nil, errRead
		}
		if len(body) > codexRemoteCatalogMaxBytes {
			return nil, fmt.Errorf("Codex model catalog exceeds %d bytes", codexRemoteCatalogMaxBytes)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("Codex model catalog returned HTTP %d", resp.StatusCode)
		}
		models, errParse := registry.ParseCodexClientModelCatalog(body, nil)
		if errParse != nil {
			return nil, errParse
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("Codex model catalog is empty")
		}
		s.codexRemoteCatalogs.Store(auth.ID, codexRemoteCatalogCacheEntry{
			payload:   append([]byte(nil), body...),
			fetchedAt: time.Now(),
			sourceKey: sourceKey,
			etag:      strings.TrimSpace(resp.Header.Get(codexRemoteCatalogETagHeader)),
		})
		return true, nil
	})
	if err != nil {
		return false, err
	}
	refreshed, _ := value.(bool)
	return refreshed, nil
}

// observeCodexResponseMetadata schedules model catalog refreshes after the
// upstream response headers are available. It deliberately does not hold up
// body streaming; the response itself remains valid with the old catalog.
func (s *Service) observeCodexResponseMetadata(_ context.Context, auth *coreauth.Auth, metadata executor.CodexResponseMetadata) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" || auth.IsDisabled() {
		return
	}
	etag := strings.TrimSpace(metadata.ModelsETag)
	if etag == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}

	baseURL := codexServiceBaseURL(auth)
	sourceKey := codexRemoteCatalogSourceKey(auth, baseURL)
	if cached, ok := s.codexRemoteCatalogs.Load(auth.ID); ok {
		if entry, okEntry := cached.(codexRemoteCatalogCacheEntry); okEntry && entry.fresh(time.Now(), sourceKey) && entry.etag == etag {
			entry.fetchedAt = time.Now()
			s.codexRemoteCatalogs.Store(auth.ID, entry)
			return
		}
	}

	observation := sourceKey + "\x00" + etag
	if previous, loaded := s.codexRemoteCatalogObservedETags.Load(auth.ID); loaded {
		if previous == observation {
			return
		}
		s.codexRemoteCatalogObservedETags.Store(auth.ID, observation)
	} else {
		s.codexRemoteCatalogObservedETags.Store(auth.ID, observation)
	}
	authID := auth.ID
	go func() {
		current, ok := s.latestAuthForModelRegistration(authID)
		if !ok || current.IsDisabled() || !strings.EqualFold(strings.TrimSpace(current.Provider), "codex") {
			return
		}
		refreshed, err := s.refreshCodexRemoteCatalogWithETag(context.Background(), current, etag)
		if err != nil {
			if previous, loaded := s.codexRemoteCatalogObservedETags.Load(authID); loaded && previous == observation {
				s.codexRemoteCatalogObservedETags.Delete(authID)
			}
			log.Debugf("failed to refresh Codex model catalog after response ETag for %s: %v", authID, err)
			return
		}
		if refreshed {
			s.refreshModelRegistrationForAuth(current)
		}
		if previous, loaded := s.codexRemoteCatalogObservedETags.Load(authID); loaded && previous == observation {
			s.codexRemoteCatalogObservedETags.Delete(authID)
		}
	}()
}

func (s *Service) codexModelsFromRemoteCatalog(auth *coreauth.Auth, fallback []*ModelInfo) []*ModelInfo {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return fallback
	}
	cached, ok := s.codexRemoteCatalogs.Load(auth.ID)
	if !ok {
		return fallback
	}
	entry, ok := cached.(codexRemoteCatalogCacheEntry)
	sourceKey := codexRemoteCatalogSourceKey(auth, codexServiceBaseURL(auth))
	if !ok || !entry.fresh(time.Now(), sourceKey) {
		return fallback
	}
	models, err := registry.ParseCodexClientModelCatalog(entry.payload, fallback)
	if err != nil || len(models) == 0 {
		return fallback
	}
	models = appendCodexLocalAliases(models, fallback)
	return registry.WithCodexBuiltins(models)
}

func codexServiceBaseURL(auth *coreauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if baseURL := strings.TrimSpace(auth.Attributes["base_url"]); baseURL != "" {
			return strings.TrimSuffix(baseURL, "/")
		}
	}
	return "https://chatgpt.com/backend-api/codex"
}

func codexRemoteCatalogSourceKey(auth *coreauth.Auth, baseURL string) string {
	accountID := codexServiceAccountID(auth)
	if accountID != "" {
		return strings.TrimSpace(baseURL) + "\x00account:" + accountID
	}
	if codexServiceAuthIsAgentIdentity(auth) {
		runtimeID := codexServiceMetadataString(auth, "agent_runtime_id", "agentRuntimeId", "agentRuntimeID")
		taskID := codexServiceMetadataString(auth, "task_id", "taskId")
		digest := sha256.Sum256([]byte(runtimeID + "\x00" + taskID))
		return strings.TrimSpace(baseURL) + "\x00agent:" + fmt.Sprintf("%x", digest)
	}
	token := codexServiceAccessToken(auth)
	if token == "" {
		return strings.TrimSpace(baseURL)
	}
	digest := sha256.Sum256([]byte(token))
	return strings.TrimSpace(baseURL) + "\x00token:" + fmt.Sprintf("%x", digest)
}

func appendCodexLocalAliases(models, fallback []*ModelInfo) []*ModelInfo {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			seen[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
		}
	}
	for _, model := range fallback {
		if model == nil || !strings.HasPrefix(strings.TrimSpace(model.Description), "Alias for ") {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[strings.ToLower(id)]; exists {
			continue
		}
		clone := *model
		models = append(models, &clone)
		seen[strings.ToLower(id)] = struct{}{}
	}
	return models
}

func codexServiceAuthIsAPIKey(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if codexServiceAuthIsAgentIdentity(auth) {
		return false
	}
	kind := strings.TrimSpace(auth.Attributes["auth_kind"])
	if strings.EqualFold(kind, "apikey") || strings.EqualFold(kind, "api_key") {
		return true
	}
	if strings.EqualFold(kind, "oauth") || strings.EqualFold(kind, "chatgpt") || strings.EqualFold(kind, "chatgpt_auth_tokens") || strings.EqualFold(kind, "agent_identity") || strings.EqualFold(kind, "agentIdentity") {
		return false
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != "" && !codexServiceHasAccessToken(auth)
}

func codexServiceAuthIsAgentIdentity(auth *coreauth.Auth) bool {
	return coreauth.CodexAuthUsesAgentIdentity(auth)
}

func codexServiceMetadataString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func codexServiceHasAccessToken(auth *coreauth.Auth) bool {
	return codexServiceAccessToken(auth) != ""
}

func codexServiceAccessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func codexServiceAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if accountID := strings.TrimSpace(auth.Attributes["account_id"]); accountID != "" {
			return accountID
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"} {
			if value, ok := auth.Metadata[key].(string); ok {
				if accountID := strings.TrimSpace(value); accountID != "" {
					return accountID
				}
			}
		}
	}
	return ""
}

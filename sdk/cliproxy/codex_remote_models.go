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
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexRemoteCatalogTTL      = 5 * time.Minute
	codexRemoteCatalogTimeout  = 15 * time.Second
	codexRemoteCatalogMaxBytes = 8 << 20
)

type codexRemoteCatalogCacheEntry struct {
	payload   []byte
	fetchedAt time.Time
	sourceKey string
}

func (entry codexRemoteCatalogCacheEntry) fresh(now time.Time, sourceKey string) bool {
	return len(entry.payload) > 0 && entry.sourceKey == sourceKey && now.Sub(entry.fetchedAt) < codexRemoteCatalogTTL
}

func (s *Service) refreshCodexRemoteCatalog(ctx context.Context, auth *coreauth.Auth) error {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" || auth.Disabled {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || codexServiceAuthIsAPIKey(auth) {
		return nil
	}
	if !codexServiceHasAccessToken(auth) {
		return nil
	}
	baseURL := codexServiceBaseURL(auth)
	sourceKey := codexRemoteCatalogSourceKey(auth, baseURL)
	now := time.Now()
	if cached, ok := s.codexRemoteCatalogs.Load(auth.ID); ok {
		if entry, okEntry := cached.(codexRemoteCatalogCacheEntry); okEntry && entry.fresh(now, sourceKey) {
			return nil
		}
	}

	flightKey := auth.ID + "\x00" + sourceKey
	_, err, _ := s.codexRemoteCatalogRefreshes.Do(flightKey, func() (any, error) {
		now = time.Now()
		if cached, ok := s.codexRemoteCatalogs.Load(auth.ID); ok {
			if entry, okEntry := cached.(codexRemoteCatalogCacheEntry); okEntry && entry.fresh(now, sourceKey) {
				return nil, nil
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
		})
		return nil, nil
	})
	return err
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
	kind := strings.TrimSpace(auth.Attributes["auth_kind"])
	if strings.EqualFold(kind, "apikey") || strings.EqualFold(kind, "api_key") {
		return true
	}
	if strings.EqualFold(kind, "oauth") || strings.EqualFold(kind, "chatgpt") || strings.EqualFold(kind, "chatgpt_auth_tokens") || strings.EqualFold(kind, "agent_identity") {
		return false
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != "" && !codexServiceHasAccessToken(auth)
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

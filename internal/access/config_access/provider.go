package configaccess

import (
	"context"
	"net/http"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	entries := normalizeEntries(cfg.APIKeys)
	if len(entries) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, entries),
	)
}

type provider struct {
	name string
	keys map[string]internalconfig.ClientAPIKeyEntry
}

func newProvider(name string, keys internalconfig.ClientAPIKeys) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]internalconfig.ClientAPIKeyEntry, len(keys))
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key.APIKey); trimmed != "" {
			keySet[trimmed] = key
		}
	}
	return &provider{name: providerName, keys: keySet}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")

	hasCredential := false
	if authHeader != "" {
		hasCredential = true
		if result, authErr, ok := p.authenticateValue(extractBearerToken(authHeader), "authorization"); ok {
			return result, authErr
		}
	}
	if authHeaderGoogle != "" {
		hasCredential = true
		if result, authErr, ok := p.authenticateValue(authHeaderGoogle, "x-goog-api-key"); ok {
			return result, authErr
		}
	}
	if authHeaderAnthropic != "" {
		hasCredential = true
		if result, authErr, ok := p.authenticateValue(authHeaderAnthropic, "x-api-key"); ok {
			return result, authErr
		}
	}

	if r.URL != nil && r.URL.RawQuery != "" {
		query := r.URL.Query()
		queryKey := query.Get("key")
		queryAuthToken := query.Get("auth_token")
		if queryKey != "" {
			hasCredential = true
			if result, authErr, ok := p.authenticateValue(queryKey, "query-key"); ok {
				return result, authErr
			}
		}
		if queryAuthToken != "" {
			hasCredential = true
			if result, authErr, ok := p.authenticateValue(queryAuthToken, "query-auth-token"); ok {
				return result, authErr
			}
		}
	}
	if !hasCredential {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func (p *provider) authenticateValue(value, source string) (*sdkaccess.Result, *sdkaccess.AuthError, bool) {
	if value == "" {
		return nil, nil, false
	}
	entry, ok := p.keys[value]
	if !ok {
		return nil, nil, false
	}
	if entry.Disabled {
		return nil, sdkaccess.NewDisabledCredentialError(), true
	}
	meta := map[string]string{
		"source": source,
	}
	if len(entry.AllowedModels) > 0 {
		meta["allowed_models"] = strings.Join(entry.AllowedModels, ",")
	}
	if len(entry.ExcludedModels) > 0 {
		meta["excluded_models"] = strings.Join(entry.ExcludedModels, ",")
	}
	internalconfig.AddClientAPIKeyQuotaMetadata(meta, entry.Quota)
	return &sdkaccess.Result{
		Provider:  p.Identifier(),
		Principal: value,
		Metadata:  meta,
	}, nil, true
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok {
		return header
	}
	if !strings.EqualFold(scheme, "bearer") {
		return header
	}
	return strings.TrimSpace(token)
}

func normalizeEntries(keys internalconfig.ClientAPIKeys) internalconfig.ClientAPIKeys {
	if len(keys) == 0 {
		return nil
	}
	return internalconfig.NormalizeClientAPIKeys(keys)
}

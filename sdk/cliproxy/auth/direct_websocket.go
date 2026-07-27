package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// SelectDirectWebsocketAuth selects an OpenAI-compatible credential for a
// bidirectional WebSocket endpoint. Unlike Execute, it deliberately does not
// require a ProviderExecutor: the caller owns the WebSocket connection and
// frame relay after credential selection.
//
// providers normally comes from the model registry. When a Realtime model has
// not yet been added to that registry, the method falls back to any configured
// OpenAI-compatible credential so new upstream Realtime models remain usable.
// The returned model is the selected credential's mapped upstream model.
func (m *Manager) SelectDirectWebsocketAuth(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options) (*Auth, string, error) {
	if m == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth manager configured"}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	auths := m.List()
	if len(auths) == 0 {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}

	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		providerKey := strings.ToLower(provider)
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		if auth, upstreamModel, err, attempted := m.selectDirectWebsocketAuthForProvider(ctx, auths, provider, model, opts); attempted {
			return auth, upstreamModel, err
		}
	}

	// Registry entries are intentionally optional for Realtime because upstream
	// models change independently of the text-model catalog. Keep the fallback
	// deterministic so it remains friendly to the regular auth selector.
	fallbackProviders := make(map[string]struct{})
	for _, candidate := range auths {
		if !isOpenAICompatibleDirectWebsocketAuth(candidate) {
			continue
		}
		provider := strings.TrimSpace(candidate.Provider)
		if provider != "" {
			fallbackProviders[strings.ToLower(provider)] = struct{}{}
		}
	}
	keys := make([]string, 0, len(fallbackProviders))
	for provider := range fallbackProviders {
		if _, alreadyTried := seenProviders[provider]; !alreadyTried {
			keys = append(keys, provider)
		}
	}
	sort.Strings(keys)
	for _, provider := range keys {
		if auth, upstreamModel, err, attempted := m.selectDirectWebsocketAuthForProvider(ctx, auths, provider, model, opts); attempted {
			return auth, upstreamModel, err
		}
	}

	return nil, "", &Error{Code: "auth_not_found", Message: "no OpenAI-compatible auth available"}
}

func (m *Manager) selectDirectWebsocketAuthForProvider(ctx context.Context, auths []*Auth, provider, model string, opts cliproxyexecutor.Options) (*Auth, string, error, bool) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, "", nil, false
	}

	candidates := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || !strings.EqualFold(strings.TrimSpace(candidate.Provider), provider) || !isOpenAICompatibleDirectWebsocketAuth(candidate) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, "", nil, false
	}

	available, err := m.availableAuthsForRouteModel(candidates, provider, model, time.Now())
	if err != nil {
		return nil, "", err, true
	}
	selector := m.selectorForProvider(provider)
	if selector == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth selector configured"}, true
	}
	selected, err := selector.Pick(ctx, provider, selectionArgForSelector(selector, model), opts, available)
	if err != nil {
		return nil, "", err, true
	}
	if selected == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}, true
	}

	auth, stale := m.clonePickedAuthForExecution(provider, selected)
	if stale || auth == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "selected auth is no longer available"}, true
	}
	models := m.prepareExecutionModels(auth, model)
	if len(models) == 0 || strings.TrimSpace(models[0]) == "" {
		return nil, "", &Error{Code: "model_not_found", Message: "selected auth has no upstream model"}, true
	}
	return auth, strings.TrimSpace(models[0]), nil, true
}

func isOpenAICompatibleDirectWebsocketAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	return strings.TrimSpace(auth.Attributes["provider_key"]) != "" || strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

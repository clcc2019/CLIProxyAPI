package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Load the config after taking m.mu. A concurrent SetConfig can otherwise
	// publish a newer config while an older pre-lock load is waiting, allowing
	// the older alias table to overwrite the newer one after the reload.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if auth.IsDisabled() {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		// Keep an empty entry as well. It distinguishes a registered API-key
		// auth with no configured aliases from an auth that is absent from the
		// snapshot, allowing request-time misses to avoid a full Config scan.
		out[auth.ID] = byAlias
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	addLookup := func(key, resolved string, allowBaseFallback bool) {
		key = strings.TrimSpace(key)
		resolved = strings.TrimSpace(resolved)
		if key == "" || resolved == "" {
			return
		}
		exactKey := strings.ToLower(key)
		if _, exists := out[exactKey]; !exists {
			out[exactKey] = resolved
		}

		// A suffix-qualified alias is intentionally not collapsed into its base
		// name. That would make foo(high) and foo(low) depend on config order.
		parsed := thinking.ParseSuffix(key)
		if !allowBaseFallback || !parsed.HasSuffix {
			return
		}
		baseKey := strings.ToLower(strings.TrimSpace(parsed.ModelName))
		if baseKey != "" {
			if _, exists := out[baseKey]; !exists {
				out[baseKey] = resolved
			}
		}
	}

	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		// Config priority: first exact alias wins. Base fallback is only
		// synthesized for an alias without a suffix.
		addLookup(alias, name, false)
		// Direct upstream names remain cheap no-ops. Keep suffix-qualified names
		// exact so an upstream high-effort entry cannot satisfy a low-effort
		// request through the shared base key.
		addLookup(name, name, false)
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.

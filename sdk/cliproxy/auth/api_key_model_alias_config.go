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
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
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
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.

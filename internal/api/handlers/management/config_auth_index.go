package management

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
)

type claudeKeyWithAuthIndex struct {
	config.ClaudeKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type codexKeyWithAuthIndex struct {
	config.CodexKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityAPIKeyWithAuthIndex struct {
	config.OpenAICompatibilityAPIKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityWithAuthIndex struct {
	Name          string                                   `json:"name"`
	Priority      int                                      `json:"priority,omitempty"`
	Disabled      bool                                     `json:"disabled"`
	PoolMode      bool                                     `json:"pool-mode,omitempty"`
	Prefix        string                                   `json:"prefix,omitempty"`
	BaseURL       string                                   `json:"base-url"`
	APIKeyEntries []openAICompatibilityAPIKeyWithAuthIndex `json:"api-key-entries,omitempty"`
	Models        []config.OpenAICompatibilityModel        `json:"models,omitempty"`
	Headers       map[string]string                        `json:"headers,omitempty"`
	AuthIndex     string                                   `json:"auth-index,omitempty"`
}

func (h *Handler) liveAuthIndexByID() map[string]string {
	if h == nil {
		return map[string]string{}
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return map[string]string{}
	}
	return manager.AuthIndexesByID()
}

func (h *Handler) claudeKeysWithAuthIndex() []claudeKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]claudeKeyWithAuthIndex, len(h.cfg.ClaudeKey))
	for i := range h.cfg.ClaudeKey {
		entry := h.cfg.ClaudeKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("claude:apikey", key, entry.BaseURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = claudeKeyWithAuthIndex{
			ClaudeKey: entry,
			AuthIndex: authIndex,
		}
	}
	return out
}

func (h *Handler) codexKeysWithAuthIndex() []codexKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]codexKeyWithAuthIndex, len(h.cfg.CodexKey))
	for i := range h.cfg.CodexKey {
		entry := h.cfg.CodexKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("codex:apikey", key, entry.BaseURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = codexKeyWithAuthIndex{
			CodexKey:  entry,
			AuthIndex: authIndex,
		}
	}
	return out
}

func (h *Handler) openAICompatibilityWithAuthIndex() []openAICompatibilityWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cfg == nil {
		return nil
	}

	normalized := normalizedOpenAICompatibilityEntries(h.cfg.OpenAICompatibility)
	out := make([]openAICompatibilityWithAuthIndex, len(normalized))
	idGen := synthesizer.NewStableIDGenerator()
	for i := range normalized {
		entry := normalized[i]
		providerName := strings.ToLower(strings.TrimSpace(entry.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		idKind := fmt.Sprintf("openai-compatibility:%s", providerName)

		response := openAICompatibilityWithAuthIndex{
			Name:      entry.Name,
			Priority:  entry.Priority,
			Disabled:  entry.Disabled,
			PoolMode:  entry.PoolMode,
			Prefix:    entry.Prefix,
			BaseURL:   entry.BaseURL,
			Models:    entry.Models,
			Headers:   entry.Headers,
			AuthIndex: "",
		}
		if len(entry.APIKeyEntries) == 0 {
			id, _ := idGen.Next(idKind, entry.BaseURL)
			response.AuthIndex = liveIndexByID[id]
		} else {
			response.APIKeyEntries = make([]openAICompatibilityAPIKeyWithAuthIndex, len(entry.APIKeyEntries))
			for j := range entry.APIKeyEntries {
				apiKeyEntry := entry.APIKeyEntries[j]
				id, _ := idGen.Next(idKind, apiKeyEntry.APIKey, entry.BaseURL, apiKeyEntry.ProxyURL)
				response.APIKeyEntries[j] = openAICompatibilityAPIKeyWithAuthIndex{
					OpenAICompatibilityAPIKey: apiKeyEntry,
					AuthIndex:                 liveIndexByID[id],
				}
			}
		}
		out[i] = response
	}
	return out
}

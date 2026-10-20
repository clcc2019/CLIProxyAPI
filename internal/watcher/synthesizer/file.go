package synthesizer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// FileSynthesizer generates Auth entries from OAuth JSON files.
// It handles file-based authentication.
type FileSynthesizer struct{}

// NewFileSynthesizer creates a new FileSynthesizer instance.
func NewFileSynthesizer() *FileSynthesizer {
	return &FileSynthesizer{}
}

// Synthesize generates Auth entries from auth files in the auth directory.
func (s *FileSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 16)
	if ctx == nil || ctx.AuthDir == "" {
		return out, nil
	}

	entries, err := os.ReadDir(ctx.AuthDir)
	if err != nil {
		// Not an error if directory doesn't exist
		return out, nil
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !util.HasJSONFileName(name) {
			continue
		}
		full := filepath.Join(ctx.AuthDir, name)
		data, errRead := os.ReadFile(full)
		if errRead != nil || len(data) == 0 {
			continue
		}
		auths := synthesizeFileAuths(ctx, full, data)
		if len(auths) == 0 {
			continue
		}
		out = append(out, auths...)
	}
	return out, nil
}

// SynthesizeAuthFile generates Auth entries for one auth JSON file payload.
// It shares exactly the same mapping behavior as FileSynthesizer.Synthesize.
func SynthesizeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) []*coreauth.Auth {
	return synthesizeFileAuths(ctx, fullPath, data)
}

func synthesizeFileAuths(ctx *SynthesisContext, fullPath string, data []byte) []*coreauth.Auth {
	if ctx == nil || len(data) == 0 {
		return nil
	}
	now := ctx.Now
	cfg := ctx.Config
	metadata, errDecode := coreauth.DecodeAuthFileMetadata(data)
	if errDecode != nil {
		return nil
	}
	if errWeight := coreauth.ValidateAuthWeight(&coreauth.Auth{Metadata: metadata}); errWeight != nil {
		log.WithError(errWeight).WithField("path", fullPath).Warn("skipping auth file with invalid credential weight")
		return nil
	}
	t, _ := metadata["type"].(string)
	if t == "" {
		return nil
	}
	providerMapper := func(provider string) string {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "gemini" {
			return "gemini-cli"
		}
		return provider
	}

	// Read per-account excluded models from the OAuth JSON file.
	perAccountExcluded := extractExcludedModelsFromMetadata(metadata)

	a := coreauth.NewAuthFromAuthFileMetadata(metadata, coreauth.AuthFileProjectionOptions{
		Path:                   fullPath,
		BaseDir:                ctx.AuthDir,
		IncludeSourceAttribute: true,
		CreatedAt:              now,
		UpdatedAt:              now,
		ProviderMapper:         providerMapper,
	})
	if a.Label == "" {
		a.Label = a.Provider
	}
	if !isSupportedSynthesizedAuthProvider(a.Provider) {
		return nil
	}
	if errWeight := coreauth.ApplyAuthWeightMetadata(a, metadata); errWeight != nil {
		log.WithError(errWeight).WithField("path", fullPath).Warn("skipping auth file with invalid credential weight")
		return nil
	}
	authKind := fileAuthKind(a)
	ApplyAuthExcludedModelsMeta(a, cfg, perAccountExcluded, authKind)
	applyCodexPlanType(a)
	if strings.EqualFold(strings.TrimSpace(a.Provider), "gemini-cli") {
		if virtuals := SynthesizeGeminiVirtualAuths(a, metadata, now); len(virtuals) > 0 {
			for _, virtual := range virtuals {
				ApplyAuthExcludedModelsMeta(virtual, cfg, perAccountExcluded, "oauth")
			}
			out := make([]*coreauth.Auth, 0, 1+len(virtuals))
			out = append(out, a)
			out = append(out, virtuals...)
			return out
		}
	}
	return []*coreauth.Auth{a}
}

func applyCodexPlanType(auth *coreauth.Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	coreauth.ApplyCodexMetadataFromMetadata(auth)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if strings.TrimSpace(auth.Attributes["plan_type"]) == "" {
		auth.Attributes["plan_type"] = codexauth.DefaultPlanType
	}
}

func isSupportedSynthesizedAuthProvider(provider string) bool {
	provider = strings.TrimSpace(provider)
	switch {
	case strings.EqualFold(provider, "claude"):
		return true
	case strings.EqualFold(provider, "codex"):
		return true
	case strings.EqualFold(provider, "kimi"):
		return true
	case strings.EqualFold(provider, "xai"):
		return true
	case strings.EqualFold(provider, "gemini"), strings.EqualFold(provider, "gemini-cli"):
		return true
	case strings.EqualFold(provider, "aistudio"):
		return true
	case strings.EqualFold(provider, "vertex"):
		return true
	case strings.EqualFold(provider, "antigravity"):
		return true
	case strings.EqualFold(provider, "openai-compatibility"):
		return true
	default:
		return false
	}
}

// SynthesizeGeminiVirtualAuths creates one active runtime credential per
// project when a Gemini CLI auth file contains a comma-separated project list.
// The persisted primary credential remains as a disabled owner for the shared
// token state; virtual entries never persist independently.
func SynthesizeGeminiVirtualAuths(primary *coreauth.Auth, metadata map[string]any, now time.Time) []*coreauth.Auth {
	if primary == nil || metadata == nil {
		return nil
	}
	projects := splitGeminiProjectIDs(metadata)
	if len(projects) <= 1 {
		return nil
	}
	email, _ := metadata["email"].(string)
	shared := geminicli.NewSharedCredential(primary.ID, email, metadata, projects)
	primary.Disabled = true
	primary.Status = coreauth.StatusDisabled
	primary.Runtime = shared
	if primary.Attributes == nil {
		primary.Attributes = make(map[string]string)
	}
	primary.Attributes["gemini_virtual_primary"] = "true"
	primary.Attributes["virtual_children"] = strings.Join(projects, ",")

	label := primary.Label
	if label == "" {
		label = primary.Provider
	}
	virtuals := make([]*coreauth.Auth, 0, len(projects))
	for _, projectID := range projects {
		attrs := map[string]string{
			"runtime_only":           "true",
			"gemini_virtual_parent":  primary.ID,
			"gemini_virtual_project": projectID,
		}
		for key, value := range primary.Attributes {
			if strings.HasPrefix(key, "header:") && strings.TrimSpace(value) != "" {
				attrs[key] = value
			}
		}
		if value := primary.Attributes["source"]; value != "" {
			attrs["source"] = value
		}
		if value := primary.Attributes["path"]; value != "" {
			attrs["path"] = value
		}
		if value := primary.Attributes["priority"]; value != "" {
			attrs["priority"] = value
		}
		if value := primary.Attributes["note"]; value != "" {
			attrs["note"] = value
		}

		metadataCopy := map[string]any{
			"email":             email,
			"project_id":        projectID,
			"virtual":           true,
			"virtual_parent_id": primary.ID,
			"type":              metadata["type"],
		}
		for _, key := range []string{"disable_cooling", "disable-cooling", "request_retry", "request-retry"} {
			if value, ok := metadata[key]; ok {
				metadataCopy[key] = value
			}
		}
		if proxy := strings.TrimSpace(primary.ProxyURL); proxy != "" {
			metadataCopy["proxy_url"] = proxy
		}
		virtuals = append(virtuals, &coreauth.Auth{
			ID:         buildGeminiVirtualID(primary.ID, projectID),
			Provider:   "gemini-cli",
			Label:      fmt.Sprintf("%s [%s]", label, projectID),
			Status:     coreauth.StatusActive,
			Attributes: attrs,
			Metadata:   metadataCopy,
			ProxyURL:   primary.ProxyURL,
			Prefix:     primary.Prefix,
			CreatedAt:  primary.CreatedAt,
			UpdatedAt:  primary.UpdatedAt,
			Runtime:    geminicli.NewVirtualCredential(projectID, shared),
		})
	}
	return virtuals
}

func splitGeminiProjectIDs(metadata map[string]any) []string {
	raw, _ := metadata["project_id"].(string)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	projects := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		projectID := strings.TrimSpace(part)
		if projectID == "" {
			continue
		}
		if _, exists := seen[projectID]; exists {
			continue
		}
		seen[projectID] = struct{}{}
		projects = append(projects, projectID)
	}
	return projects
}

func buildGeminiVirtualID(baseID, projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "project"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_")
	return fmt.Sprintf("%s::%s", baseID, replacer.Replace(projectID))
}

// extractExcludedModelsFromMetadata reads per-account excluded models from the OAuth JSON metadata.
// Supports both "excluded_models" and "excluded-models" keys, and accepts both []string and []interface{}.
func extractExcludedModelsFromMetadata(metadata map[string]any) []string {
	if metadata == nil {
		return nil
	}
	// Try both key formats
	raw, ok := metadata["excluded_models"]
	if !ok {
		raw, ok = metadata["excluded-models"]
	}
	if !ok || raw == nil {
		return nil
	}
	var stringSlice []string
	switch v := raw.(type) {
	case []string:
		stringSlice = v
	case []interface{}:
		stringSlice = make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				stringSlice = append(stringSlice, s)
			}
		}
	default:
		return nil
	}
	result := make([]string, 0, len(stringSlice))
	for _, s := range stringSlice {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

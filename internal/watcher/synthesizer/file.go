package synthesizer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// FileSynthesizer generates Auth entries from OAuth JSON files.
// It handles file-based authentication and Gemini virtual auth generation.
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
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
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
	if a.Provider == "kiro" {
		applyKiroMachineIDAttribute(a.Attributes, metadata, a.ID)
	}
	ApplyAuthExcludedModelsMeta(a, cfg, perAccountExcluded, "oauth")
	if a.Provider == "gemini-cli" {
		if virtuals := SynthesizeGeminiVirtualAuths(a, metadata, now); len(virtuals) > 0 {
			for _, v := range virtuals {
				ApplyAuthExcludedModelsMeta(v, cfg, perAccountExcluded, "oauth")
			}
			out := make([]*coreauth.Auth, 0, 1+len(virtuals))
			out = append(out, a)
			out = append(out, virtuals...)
			return out
		}
	}
	return []*coreauth.Auth{a}
}

func applyKiroMachineIDAttribute(attrs map[string]string, metadata map[string]any, fallbackSeed string) {
	if attrs == nil {
		return
	}
	for _, key := range []string{"machine_id", "machineId", "device_id", "deviceId"} {
		if raw, ok := metadata[key].(string); ok && strings.TrimSpace(raw) != "" {
			attrs["machine_id"] = strings.TrimSpace(raw)
			return
		}
	}
	if strings.TrimSpace(fallbackSeed) != "" {
		attrs["machine_id_seed"] = fallbackSeed
	}
}

// SynthesizeGeminiVirtualAuths creates virtual Auth entries for multi-project Gemini credentials.
// It disables the primary auth and creates one virtual auth per project.
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
	source := primary.Attributes["source"]
	authPath := primary.Attributes["path"]
	originalProvider := primary.Provider
	if originalProvider == "" {
		originalProvider = "gemini-cli"
	}
	label := primary.Label
	if label == "" {
		label = originalProvider
	}
	virtuals := make([]*coreauth.Auth, 0, len(projects))
	for _, projectID := range projects {
		attrs := map[string]string{
			"runtime_only":           "true",
			"gemini_virtual_parent":  primary.ID,
			"gemini_virtual_project": projectID,
		}
		if source != "" {
			attrs["source"] = source
		}
		if authPath != "" {
			attrs["path"] = authPath
		}
		// Propagate priority from primary auth to virtual auths
		if priorityVal, hasPriority := primary.Attributes["priority"]; hasPriority && priorityVal != "" {
			attrs["priority"] = priorityVal
		}
		// Propagate note from primary auth to virtual auths
		if noteVal, hasNote := primary.Attributes["note"]; hasNote && noteVal != "" {
			attrs["note"] = noteVal
		}
		for k, v := range primary.Attributes {
			if strings.HasPrefix(k, "header:") && strings.TrimSpace(v) != "" {
				attrs[k] = v
			}
		}
		metadataCopy := map[string]any{
			"email":             email,
			"project_id":        projectID,
			"virtual":           true,
			"virtual_parent_id": primary.ID,
			"type":              metadata["type"],
		}
		if v, ok := metadata["disable_cooling"]; ok {
			metadataCopy["disable_cooling"] = v
		} else if v, ok := metadata["disable-cooling"]; ok {
			metadataCopy["disable_cooling"] = v
		}
		if v, ok := metadata["request_retry"]; ok {
			metadataCopy["request_retry"] = v
		} else if v, ok := metadata["request-retry"]; ok {
			metadataCopy["request_retry"] = v
		}
		proxy := strings.TrimSpace(primary.ProxyURL)
		if proxy != "" {
			metadataCopy["proxy_url"] = proxy
		}
		virtual := &coreauth.Auth{
			ID:         buildGeminiVirtualID(primary.ID, projectID),
			Provider:   originalProvider,
			Label:      fmt.Sprintf("%s [%s]", label, projectID),
			Status:     coreauth.StatusActive,
			Attributes: attrs,
			Metadata:   metadataCopy,
			ProxyURL:   primary.ProxyURL,
			Prefix:     primary.Prefix,
			CreatedAt:  primary.CreatedAt,
			UpdatedAt:  primary.UpdatedAt,
			Runtime:    geminicli.NewVirtualCredential(projectID, shared),
		}
		virtuals = append(virtuals, virtual)
	}
	return virtuals
}

// splitGeminiProjectIDs extracts and deduplicates project IDs from metadata.
func splitGeminiProjectIDs(metadata map[string]any) []string {
	raw, _ := metadata["project_id"].(string)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

// buildGeminiVirtualID constructs a virtual auth ID from base ID and project ID.
func buildGeminiVirtualID(baseID, projectID string) string {
	project := strings.TrimSpace(projectID)
	if project == "" {
		project = "project"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_")
	return fmt.Sprintf("%s::%s", baseID, replacer.Replace(project))
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

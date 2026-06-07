package synthesizer

import (
	"os"
	"path/filepath"
	"strings"

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
	t, _ := metadata["type"].(string)
	if t == "" {
		return nil
	}
	providerMapper := func(provider string) string {
		return strings.ToLower(strings.TrimSpace(provider))
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
	ApplyAuthExcludedModelsMeta(a, cfg, perAccountExcluded, "oauth")
	return []*coreauth.Auth{a}
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
	case strings.EqualFold(provider, "openai-compatibility"):
		return true
	default:
		return false
	}
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

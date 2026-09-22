package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed models/codex_client_models.json
var codexClientModelsJSON []byte

var codexClientModelsStore struct {
	mu       sync.RWMutex
	data     []byte
	revision uint64
}

func init() {
	_, _ = loadCodexClientModelsFromBytes(codexClientModelsJSON, "embed")
}

// GetCodexClientModelsJSON returns the embedded Codex client model catalog.
func GetCodexClientModelsJSON() []byte {
	codexClientModelsStore.mu.RLock()
	defer codexClientModelsStore.mu.RUnlock()
	if len(codexClientModelsStore.data) == 0 {
		return append([]byte(nil), codexClientModelsJSON...)
	}
	return append([]byte(nil), codexClientModelsStore.data...)
}

// GetCodexClientModelsRevision changes only after a validated remote catalog
// replaces the active snapshot.
func GetCodexClientModelsRevision() uint64 {
	codexClientModelsStore.mu.RLock()
	defer codexClientModelsStore.mu.RUnlock()
	return codexClientModelsStore.revision
}

func GetCodexClientModelsSnapshot() ([]byte, uint64) {
	codexClientModelsStore.mu.RLock()
	defer codexClientModelsStore.mu.RUnlock()
	return append([]byte(nil), codexClientModelsStore.data...), codexClientModelsStore.revision
}

func loadCodexClientModelsFromBytes(data []byte, source string) (bool, error) {
	if err := ValidateCodexClientModelsJSON(data); err != nil {
		return false, fmt.Errorf("%s: %w", source, err)
	}
	data = append([]byte(nil), data...)
	codexClientModelsStore.mu.Lock()
	defer codexClientModelsStore.mu.Unlock()
	if bytes.Equal(codexClientModelsStore.data, data) {
		return false, nil
	}
	codexClientModelsStore.data = data
	codexClientModelsStore.revision++
	return true, nil
}

// ValidateCodexClientModelsJSON accepts both the compact capability catalog
// used by this project and the richer community catalog. Unknown fields are
// intentionally preserved for forward compatibility; every model must have a
// stable non-empty slug (or id) and IDs must be unique.
func ValidateCodexClientModelsJSON(data []byte) error {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode Codex client model catalog: %w", err)
	}
	if len(payload.Models) == 0 {
		return fmt.Errorf("Codex client model catalog has no models")
	}
	seen := make(map[string]struct{}, len(payload.Models))
	for i, model := range payload.Models {
		raw, _ := model["slug"].(string)
		if strings.TrimSpace(raw) == "" {
			raw, _ = model["id"].(string)
		}
		slug := strings.TrimSpace(raw)
		if slug == "" {
			return fmt.Errorf("Codex client model catalog models[%d] has empty slug", i)
		}
		if _, exists := seen[slug]; exists {
			return fmt.Errorf("Codex client model catalog contains duplicate slug %q", slug)
		}
		seen[slug] = struct{}{}
	}
	return nil
}

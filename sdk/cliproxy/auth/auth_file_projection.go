package auth

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AuthFileProjectionOptions controls how an auth-file JSON document is projected
// into a runtime Auth record.
type AuthFileProjectionOptions struct {
	ID                     string
	Path                   string
	BaseDir                string
	FileName               string
	UseBaseNameAsFileName  bool
	IncludeSourceAttribute bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Now                    time.Time
	ExtraAttributes        map[string]string
	ProviderMapper         func(string) string
}

// DecodeAuthFileMetadata unmarshals and normalizes an auth-file JSON document.
func DecodeAuthFileMetadata(data []byte) (map[string]any, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	if normalized, changed := NormalizeImportedAuthMetadata(metadata); changed {
		metadata = normalized
	}
	return metadata, nil
}

// NewAuthFromAuthFileData projects raw auth-file JSON into a runtime Auth record.
func NewAuthFromAuthFileData(data []byte, opts AuthFileProjectionOptions) (*Auth, error) {
	metadata, err := DecodeAuthFileMetadata(data)
	if err != nil {
		return nil, err
	}
	return NewAuthFromAuthFileMetadata(metadata, opts), nil
}

// NewAuthFromAuthFileMetadata projects auth-file metadata into a runtime Auth record.
func NewAuthFromAuthFileMetadata(metadata map[string]any, opts AuthFileProjectionOptions) *Auth {
	if metadata == nil {
		metadata = make(map[string]any)
	}
	if normalized, changed := NormalizeImportedAuthMetadata(metadata); changed {
		metadata = normalized
	}

	provider := strings.TrimSpace(authFileProjectionString(metadata, "type"))
	if provider == "" {
		provider = "unknown"
	}
	if opts.ProviderMapper != nil {
		if mapped := strings.TrimSpace(opts.ProviderMapper(provider)); mapped != "" {
			provider = mapped
		}
	}

	path := strings.TrimSpace(opts.Path)
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		id = AuthFileIDForPath(path, opts.BaseDir)
	}
	if id == "" {
		id = strings.TrimSpace(opts.FileName)
	}
	if id == "" {
		id = provider
	}

	fileName := strings.TrimSpace(opts.FileName)
	if fileName == "" {
		if opts.UseBaseNameAsFileName && path != "" {
			fileName = filepath.Base(path)
		} else {
			fileName = id
		}
	}

	attrs := make(map[string]string, len(opts.ExtraAttributes)+3)
	if path != "" {
		attrs["path"] = path
		if opts.IncludeSourceAttribute {
			attrs["source"] = path
		}
	}
	if email := strings.TrimSpace(authFileProjectionString(metadata, "email")); email != "" {
		attrs["email"] = email
	}
	for key, value := range opts.ExtraAttributes {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			delete(attrs, key)
			continue
		}
		attrs[key] = value
	}

	disabled, _ := metadata["disabled"].(bool)
	status := StatusActive
	if disabled {
		status = StatusDisabled
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := opts.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	auth := &Auth{
		ID:               id,
		Provider:         provider,
		FileName:         fileName,
		Label:            AuthFileLabelFromMetadata(metadata),
		Status:           status,
		Disabled:         disabled,
		Attributes:       attrs,
		Metadata:         metadata,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}

	ApplyAuthFileOptionsFromMetadata(auth)
	ApplyCodexMetadataFromMetadata(auth)
	ApplyCustomHeadersFromMetadata(auth)
	return auth
}

// AuthFileIDForPath returns the stable auth ID derived from a file path and its auth directory.
func AuthFileIDForPath(path, baseDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	id := path
	baseDir = strings.TrimSpace(baseDir)
	if baseDir != "" {
		if rel, err := filepath.Rel(baseDir, path); err == nil && rel != "" {
			id = rel
		}
	}
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

// AuthFileLabelFromMetadata picks the most descriptive operator-facing label.
func AuthFileLabelFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"label", "email", "project_id"} {
		if value := strings.TrimSpace(authFileProjectionString(metadata, key)); value != "" {
			return value
		}
	}
	return ""
}

func authFileProjectionString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

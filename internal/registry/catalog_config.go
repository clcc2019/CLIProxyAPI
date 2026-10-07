package registry

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// CatalogSources overrides the source of each model catalog independently.
// Empty values retain the process default: the official remote catalog in
// normal mode or the embedded catalog when --local-model is enabled.
type CatalogSources struct {
	Catalog      string `yaml:"catalog" json:"catalog,omitempty"`
	CodexCatalog string `yaml:"codex-catalog" json:"codex-catalog,omitempty"`
	DevinCatalog string `yaml:"devin-catalog" json:"devin-catalog,omitempty"`
}

// Validate accepts HTTPS/HTTP URLs and absolute local paths only. Relative
// paths are rejected so a config file cannot silently change meaning when the
// process working directory changes.
func (m CatalogSources) Validate() error {
	for name, rawSource := range map[string]string{
		"catalog":       m.Catalog,
		"codex-catalog": m.CodexCatalog,
		"devin-catalog": m.DevinCatalog,
	} {
		source := strings.TrimSpace(rawSource)
		if source == "" || filepath.IsAbs(source) {
			continue
		}
		parsed, errParse := url.Parse(source)
		if errParse != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return fmt.Errorf("models.%s must be an http(s) URL or an absolute local path", name)
		}
	}
	return nil
}

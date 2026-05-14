package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestNewUtlsHTTPClientReusesCachedClientAndTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.ProxyURL = "http://proxy-utls.example.com:8080"

	first := NewUtlsHTTPClient(cfg, nil, 0)
	second := NewUtlsHTTPClient(cfg, nil, 0)

	if first != second {
		t.Fatal("expected claude utls client cache reuse")
	}
	if first.Transport != second.Transport {
		t.Fatal("expected claude utls transport reuse")
	}
}

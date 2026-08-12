package helps

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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

func TestNewFingerprintHTTPClientDoesNotCacheFunctionFallbacks(t *testing.T) {
	t.Parallel()

	newFallback := func(label string) http.RoundTripper {
		return contextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"X-Fallback": []string{label}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})
	}

	first := newFingerprintHTTPClient("", nil, newFallback("first"), 0, nil, defaultTLSClientHelloID())
	second := newFingerprintHTTPClient("", nil, newFallback("second"), 0, nil, defaultTLSClientHelloID())
	if first == second || first.Transport == second.Transport {
		t.Fatal("expected function-valued fallbacks to bypass global fingerprint caches")
	}

	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{name: "first", client: first, want: "first"},
		{name: "second", client: second, want: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://fallback.test", nil)
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			resp, err := tc.client.Do(req)
			if err != nil {
				t.Fatalf("client.Do() error = %v", err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("X-Fallback"); got != tc.want {
				t.Fatalf("fallback label = %q, want %q", got, tc.want)
			}
		})
	}
}

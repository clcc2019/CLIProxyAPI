package helps

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientReusesDefaultClient(t *testing.T) {
	t.Parallel()

	first := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)
	second := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)

	if first != second {
		t.Fatal("expected default client to be reused")
	}
}

func TestNewProxyAwareHTTPClientUsesSharedDefaultTransportSettings(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.MaxIdleConns != pooledTransportMaxIdleConns {
		t.Fatalf("MaxIdleConns = %d, want %d", transport.MaxIdleConns, pooledTransportMaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != pooledTransportMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, pooledTransportMaxIdleConnsPerHost)
	}
	if transport.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost = %d, want 0", transport.MaxConnsPerHost)
	}
	if transport.IdleConnTimeout != pooledTransportIdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %s, want %s", transport.IdleConnTimeout, pooledTransportIdleConnTimeout)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("expected ForceAttemptHTTP2 to be enabled")
	}
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientPrefersContextRoundTripperForAuthProxy(t *testing.T) {
	t.Parallel()

	expected := &roundTripperSpy{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(expected))

	client := NewProxyAwareHTTPClient(
		ctx,
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example.com:8080"},
		0,
	)
	second := NewProxyAwareHTTPClient(
		ctx,
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example.com:8080"},
		0,
	)

	if client.Transport != expected {
		t.Fatalf("transport = %T %v, want context round tripper", client.Transport, client.Transport)
	}
	if client == second {
		t.Fatal("expected a fresh client for a context round tripper")
	}
	if second.Transport != expected {
		t.Fatalf("second transport = %T %v, want context round tripper", second.Transport, second.Transport)
	}
}

func TestNewProxyAwareHTTPClientKeepsContextRoundTripperClosuresIsolated(t *testing.T) {
	t.Parallel()

	newRoundTripper := func(label string) http.RoundTripper {
		return contextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"X-Transport": []string{label}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})
	}

	firstRT := newRoundTripper("first")
	secondRT := newRoundTripper("second")
	firstCtx := context.WithValue(context.Background(), "cliproxy.roundtripper", firstRT)
	secondCtx := context.WithValue(context.Background(), "cliproxy.roundtripper", secondRT)

	first := NewProxyAwareHTTPClient(firstCtx, &config.Config{}, nil, 0)
	second := NewProxyAwareHTTPClient(secondCtx, &config.Config{}, nil, 0)

	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{name: "first", client: first, want: "first"},
		{name: "second", client: second, want: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://upstream.test/v1/responses", nil)
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			resp, err := tc.client.Do(req)
			if err != nil {
				t.Fatalf("client.Do() error = %v", err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("X-Transport"); got != tc.want {
				t.Fatalf("transport label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewProxyAwareHTTPClientCachesGlobalProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}}

	first := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	second := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)

	if first != second {
		t.Fatal("expected proxy-aware client to be reused")
	}
	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("expected transports to be configured, got %T and %T", first.Transport, second.Transport)
	}
	if first.Transport != second.Transport {
		t.Fatal("expected global proxy transport to be reused")
	}
}

func TestNewProxyAwareHTTPClientSeparatesClientCacheByTimeout(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}}

	first := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	second := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 5*time.Second)

	if first == second {
		t.Fatal("expected timeout-specific clients to use separate cache entries")
	}
	if first.Transport != second.Transport {
		t.Fatal("expected timeout-specific clients to share transport")
	}
	if second.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s, want %s", second.Timeout, 5*time.Second)
	}
}

func TestNewProxyAwareHTTPClientReusesCustomCATransport(t *testing.T) {
	t.Setenv("CODEX_CA_CERTIFICATE", mustCreateProxyHelperTestCertificatePEM(t))
	t.Setenv("SSL_CERT_FILE", "")

	first := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)
	second := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)

	if first != second {
		t.Fatal("expected custom-CA client to be reused")
	}
	if first.Transport == nil || first.Transport != second.Transport {
		t.Fatal("expected custom-CA transport to be reused")
	}
}

func TestNewProxyAwareHTTPClientDoesNotCacheContextCustomCATransport(t *testing.T) {
	t.Setenv("CODEX_CA_CERTIFICATE", mustCreateProxyHelperTestCertificatePEM(t))
	t.Setenv("SSL_CERT_FILE", "")

	base := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(base))
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example.com:8080"}

	first := NewProxyAwareHTTPClient(ctx, &config.Config{}, auth, 0)
	second := NewProxyAwareHTTPClient(ctx, &config.Config{}, auth, 0)

	if first == second {
		t.Fatal("expected a fresh client for a context custom-CA transport")
	}
	if first.Transport == nil || second.Transport == nil {
		t.Fatal("expected context custom-CA transports to be configured")
	}
	if first.Transport == second.Transport {
		t.Fatal("expected context custom-CA wrappers to remain request-scoped")
	}
	if first.Transport == base {
		t.Fatal("expected context transport to be cloned with custom root CAs")
	}
}

func TestNewCodexHTTPClientUsesNativeProxyAwareClient(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://codex-native-proxy.example.com:8080"}}

	client := NewCodexHTTPClient(context.Background(), cfg, nil, 5*time.Second)
	proxyAware := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 5*time.Second)

	if client != proxyAware {
		t.Fatal("expected Codex client to reuse proxy-aware client cache")
	}
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if client.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s, want %s", client.Timeout, 5*time.Second)
	}
}

type roundTripperSpy struct{}

func (spy *roundTripperSpy) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

type contextRoundTripFunc func(*http.Request) (*http.Response, error)

func (f contextRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mustCreateProxyHelperTestCertificatePEM(t *testing.T) string {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "CLIProxyAPI Proxy Helper Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

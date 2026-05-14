package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type httpRequestContextCaptureExecutor struct {
	rt http.RoundTripper
}

func (e *httpRequestContextCaptureExecutor) Identifier() string { return "codex" }

func (e *httpRequestContextCaptureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *httpRequestContextCaptureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *httpRequestContextCaptureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, nil
}

func (e *httpRequestContextCaptureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *httpRequestContextCaptureExecutor) HttpRequest(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	e.rt, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	return nil, nil
}

type staticRoundTripperProvider struct {
	rt http.RoundTripper
}

func (p staticRoundTripperProvider) RoundTripperFor(*Auth) http.RoundTripper {
	return p.rt
}

type noopRoundTripper struct{}

func (noopRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerHttpRequestInjectsRoundTripperProvider(t *testing.T) {
	executor := &httpRequestContextCaptureExecutor{}
	expected := noopRoundTripper{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetRoundTripperProvider(staticRoundTripperProvider{rt: expected})

	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	_, err = manager.HttpRequest(context.Background(), &Auth{Provider: "codex"}, req)
	if err != nil {
		t.Fatalf("HttpRequest() error = %v", err)
	}
	if executor.rt != expected {
		t.Fatalf("round tripper = %T %v, want provider round tripper", executor.rt, executor.rt)
	}
}

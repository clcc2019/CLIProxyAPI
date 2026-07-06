package access

import (
	"context"
	"net/http"
	"testing"
)

type managerTestProvider struct {
	id        string
	result    *Result
	authError *AuthError
}

func (p managerTestProvider) Identifier() string {
	return p.id
}

func (p managerTestProvider) Authenticate(context.Context, *http.Request) (*Result, *AuthError) {
	if p.authError != nil {
		return nil, p.authError
	}
	return p.result, nil
}

func TestManagerProvidersReturnsDefensiveCopy(t *testing.T) {
	manager := NewManager()
	first := managerTestProvider{id: "first", result: &Result{Provider: "first", Principal: "p1"}}
	second := managerTestProvider{id: "second", result: &Result{Provider: "second", Principal: "p2"}}
	manager.SetProviders([]Provider{first})

	providers := manager.Providers()
	if len(providers) != 1 {
		t.Fatalf("providers length = %d, want 1", len(providers))
	}
	providers[0] = second

	req := httptestRequest()
	result, authErr := manager.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("Authenticate returned error: %v", authErr)
	}
	if result == nil || result.Provider != "first" {
		t.Fatalf("Authenticate result = %+v, want provider first", result)
	}
}

func TestManagerAuthenticateUsesLatestProviderSnapshot(t *testing.T) {
	manager := NewManager()
	manager.SetProviders([]Provider{
		managerTestProvider{id: "first", authError: NewNotHandledError()},
		managerTestProvider{id: "second", result: &Result{Provider: "second", Principal: "p2"}},
	})

	result, authErr := manager.Authenticate(context.Background(), httptestRequest())
	if authErr != nil {
		t.Fatalf("Authenticate returned error: %v", authErr)
	}
	if result == nil || result.Provider != "second" {
		t.Fatalf("Authenticate result = %+v, want provider second", result)
	}
}

func BenchmarkManagerAuthenticate(b *testing.B) {
	manager := NewManager()
	manager.SetProviders([]Provider{
		managerTestProvider{id: "first", authError: NewNotHandledError()},
		managerTestProvider{id: "second", result: &Result{Provider: "second", Principal: "p2"}},
	})
	req := httptestRequest()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, authErr := manager.Authenticate(ctx, req)
		if authErr != nil || result == nil {
			b.Fatalf("Authenticate failed: result=%+v err=%v", result, authErr)
		}
	}
}

func httptestRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	return req
}

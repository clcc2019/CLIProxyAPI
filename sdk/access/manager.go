package access

import (
	"context"
	"net/http"
	"sync/atomic"
)

// Manager coordinates authentication providers.
type Manager struct {
	providers atomic.Value // stores []Provider
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	m := &Manager{}
	m.providers.Store([]Provider{})
	return m
}

// SetProviders replaces the active provider list.
func (m *Manager) SetProviders(providers []Provider) {
	if m == nil {
		return
	}
	cloned := make([]Provider, len(providers))
	copy(cloned, providers)
	m.providers.Store(cloned)
}

// Providers returns a snapshot of the active providers.
func (m *Manager) Providers() []Provider {
	if m == nil {
		return nil
	}
	providers := m.providersSnapshot()
	snapshot := make([]Provider, len(providers))
	copy(snapshot, providers)
	return snapshot
}

func (m *Manager) providersSnapshot() []Provider {
	if m == nil {
		return nil
	}
	raw := m.providers.Load()
	if providers, ok := raw.([]Provider); ok {
		return providers
	}
	return nil
}

// Authenticate evaluates providers until one succeeds.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	if m == nil {
		return nil, nil
	}
	providers := m.providersSnapshot()
	if len(providers) == 0 {
		return nil, nil
	}

	var (
		missing bool
		invalid bool
	)

	for _, provider := range providers {
		if provider == nil {
			continue
		}
		res, authErr := provider.Authenticate(ctx, r)
		if authErr == nil {
			return res, nil
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNotHandled) {
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
			missing = true
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
			invalid = true
			continue
		}
		return nil, authErr
	}

	if invalid {
		return nil, NewInvalidCredentialError()
	}
	if missing {
		return nil, NewNoCredentialsError()
	}
	return nil, NewNoCredentialsError()
}

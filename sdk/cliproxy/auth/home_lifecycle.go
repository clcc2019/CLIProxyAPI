package auth

import (
	"context"
	"strings"
	"time"

	internalhome "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
)

const homeExecutionScopeMetadataKey = "__cliproxy_home_execution_scope"

func homeScopeFromAuth(auth *Auth) *executionregistry.Scope {
	if auth == nil || len(auth.Metadata) == 0 {
		return nil
	}
	if scope, ok := auth.Metadata[homeExecutionScopeMetadataKey].(*executionregistry.Scope); ok {
		return scope
	}
	return nil
}

func setHomeScopeOnAuth(auth *Auth, scope *executionregistry.Scope) {
	if auth == nil || scope == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[homeExecutionScopeMetadataKey] = scope
}

func homeScopeRequestID(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

// HomeDispatchBundle binds a dispatcher to the execution registry for one
// Home subscriber lifetime. Replacing the bundle fences new dispatches while
// existing scopes remain owned by their original registry.
type HomeDispatchBundle struct {
	client     homeAuthDispatcher
	registry   *executionregistry.Registry
	generation uint64
}

func (m *Manager) PublishHomeDispatch(client homeAuthDispatcher, registry *executionregistry.Registry, generation uint64) *HomeDispatchBundle {
	if m == nil || client == nil || registry == nil {
		return nil
	}
	bundle := &HomeDispatchBundle{client: client, registry: registry, generation: generation}
	if releaser, ok := client.(interface {
		PushConcurrencyRelease(context.Context, internalhome.ConcurrencyReleaseFrame) error
	}); ok {
		registry.SetReleaseSink(func(group executionregistry.ReleaseGroup, sequence int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaser.PushConcurrencyRelease(ctx, internalhome.ConcurrencyReleaseFrame{CredentialID: group.CredentialID, Model: group.Model, ReleaseSeq: sequence})
		})
	}
	m.homeDispatchBundle.Store(bundle)
	return bundle
}

func (m *Manager) ClearHomeDispatchBundle(bundle *HomeDispatchBundle) bool {
	if m == nil || bundle == nil {
		return false
	}
	return m.homeDispatchBundle.CompareAndSwap(bundle, nil)
}

func (m *Manager) HomeDispatchBundle() *HomeDispatchBundle {
	if m == nil {
		return nil
	}
	return m.homeDispatchBundle.Load()
}

func (m *Manager) SetHomeExecutionRegistry(registry *executionregistry.Registry) {
	if m == nil || registry == nil {
		return
	}
	if client := currentHomeDispatcher(); client != nil {
		m.PublishHomeDispatch(client, registry, 0)
	}
}

func (m *Manager) ClearHomeExecutionRegistry(registry *executionregistry.Registry) bool {
	bundle := m.HomeDispatchBundle()
	if bundle == nil || bundle.registry != registry {
		return false
	}
	return m.ClearHomeDispatchBundle(bundle)
}

func (m *Manager) HomeExecutionRegistry() *executionregistry.Registry {
	if bundle := m.HomeDispatchBundle(); bundle != nil {
		return bundle.registry
	}
	return nil
}

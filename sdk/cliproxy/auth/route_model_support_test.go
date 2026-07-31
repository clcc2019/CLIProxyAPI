package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestAuthSupportsRouteModelHonorsUpstreamNameAndExactSuffix(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "route-support-" + t.Name(), Provider: "compat"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{
		ID:   "PUBLIC(high)",
		Name: "Upstream-High",
	}})
	t.Cleanup(func() { registryRef.UnregisterClient(auth.ID) })
	if _, ok := supportedModelSetForAuth(auth.ID)["upstream-high"]; !ok {
		t.Fatal("scheduler supported-model snapshot should include provider-facing name")
	}

	for _, routeModel := range []string{"public(high)", "upstream-high"} {
		if !manager.authSupportsRouteModel(registryRef, auth, routeModel) {
			t.Errorf("authSupportsRouteModel(%q) = false, want true", routeModel)
		}
	}
	if _, ok := supportedModelSetForAuth(auth.ID)["upstream-high"]; !ok {
		t.Fatal("scheduler model key should be case-normalized")
	}
	for _, routeModel := range []string{"public(low)", "public"} {
		if manager.authSupportsRouteModel(registryRef, auth, routeModel) {
			t.Errorf("authSupportsRouteModel(%q) = true, want false", routeModel)
		}
	}
}

func TestSingleLegacySelectionRequiredSeparatesSuffixSpecificModels(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "suffix-route-" + t.Name(), Provider: "compat"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "public(high)"}})
	t.Cleanup(func() { registryRef.UnregisterClient(auth.ID) })

	if manager.singleLegacySelectionRequired(auth.Provider, "public(high)", nil) {
		t.Fatal("high suffix should use the scheduler-compatible exact registration")
	}
	if !manager.singleLegacySelectionRequired(auth.Provider, "public(low)", nil) {
		t.Fatal("low suffix must not reuse the high-only registration")
	}
	if manager.singleLegacySelectionRequired(auth.Provider, "public(high)", nil) {
		t.Fatal("high suffix cache key should not be polluted by the low-suffix decision")
	}
}

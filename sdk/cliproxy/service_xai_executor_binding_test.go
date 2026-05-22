package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureExecutorsForAuth_XAIBindsIndependentExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "xai-auth-1",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	service.ensureExecutorsForAuth(auth)
	resolved, ok := service.coreManager.Executor("xai")
	if !ok || resolved == nil {
		t.Fatal("expected xai executor after bind")
	}
	if _, isXAI := resolved.(*executor.XAIExecutor); !isXAI {
		t.Fatalf("executor type = %T, want *executor.XAIExecutor", resolved)
	}
	if _, isCodex := resolved.(*executor.CodexAutoExecutor); isCodex {
		t.Fatal("xai must not bind the codex auto executor")
	}
}

func TestRegisterHomeExecutorsIncludesXAI(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	service.registerHomeExecutors()
	resolved, ok := service.coreManager.Executor("xai")
	if !ok || resolved == nil {
		t.Fatal("expected xai executor after registering home executors")
	}
	if _, isXAI := resolved.(*executor.XAIExecutor); !isXAI {
		t.Fatalf("executor type = %T, want *executor.XAIExecutor", resolved)
	}
}

func TestRegisterModelsForAuth_XAIRegistersCatalog(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "xai-model-auth-test",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	models := reg.GetModelsForClient(auth.ID)
	if len(models) == 0 {
		t.Fatal("expected xai models to be registered")
	}
	for _, model := range models {
		if model != nil && model.ID == "grok-imagine-image" {
			return
		}
	}
	t.Fatalf("expected grok-imagine-image in registered xai models, got %d models", len(models))
}

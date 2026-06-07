package handlers

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestGetRequestDetails_PreservesSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient("test-request-details-openai-new", "openai", []*registry.ModelInfo{
		{ID: "gpt-5", Created: now + 30},
		{ID: "gpt-5-mini", Created: now + 25},
	})
	modelRegistry.RegisterClient("test-request-details-openai", "openai", []*registry.ModelInfo{
		{ID: "gpt-5.2", Created: now + 20},
	})
	modelRegistry.RegisterClient("test-request-details-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Created: now + 5},
	})

	// Ensure cleanup of all test registrations.
	clientIDs := []string{
		"test-request-details-openai-new",
		"test-request-details-openai",
		"test-request-details-claude",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(func() {
			modelRegistry.UnregisterClient(id)
		})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantProviders []string
		wantModel     string
		wantErr       bool
	}{
		{
			name:          "numeric suffix preserved",
			inputModel:    "gpt-5(8192)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5(8192)",
			wantErr:       false,
		},
		{
			name:          "level suffix preserved",
			inputModel:    "gpt-5.2(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.2(high)",
			wantErr:       false,
		},
		{
			name:          "no suffix unchanged",
			inputModel:    "claude-sonnet-4-5",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5",
			wantErr:       false,
		},
		{
			name:          "unknown model with suffix",
			inputModel:    "unknown-model(8192)",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "auto suffix resolved",
			inputModel:    "auto(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5(high)",
			wantErr:       false,
		},
		{
			name:          "special suffix none preserved",
			inputModel:    "gpt-5-mini(none)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5-mini(none)",
			wantErr:       false,
		},
		{
			name:          "special suffix auto preserved",
			inputModel:    "claude-sonnet-4-5(auto)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5(auto)",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, model, errMsg := handler.getRequestDetails(tt.inputModel)
			if (errMsg != nil) != tt.wantErr {
				t.Fatalf("getRequestDetails() error = %v, wantErr %v", errMsg, tt.wantErr)
			}
			if errMsg != nil {
				return
			}
			if !reflect.DeepEqual(providers, tt.wantProviders) {
				t.Fatalf("getRequestDetails() providers = %v, want %v", providers, tt.wantProviders)
			}
			if model != tt.wantModel {
				t.Fatalf("getRequestDetails() model = %v, want %v", model, tt.wantModel)
			}
		})
	}
}

func TestGetRequestDetails_ImageModelReturns503(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	_, _, errMsg := handler.getRequestDetails("gpt-image-1.5")
	if errMsg == nil {
		t.Fatalf("expected error for gpt-image-1.5, got nil")
	}
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status code: got %d want %d", errMsg.StatusCode, http.StatusServiceUnavailable)
	}
	if errMsg.Error == nil {
		t.Fatalf("expected error message, got nil")
	}
	msg := errMsg.Error.Error()
	if !strings.Contains(msg, "/v1/images/generations") ||
		!strings.Contains(msg, "/v1/images/edits") ||
		!strings.Contains(msg, "/v1/images/variations") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestIsOpenAIImageOnlyModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{name: "bare image model", model: "gpt-image-1.5", want: true},
		{name: "mixed case image model", model: " GPT-Image-1.5 ", want: true},
		{name: "provider prefixed image model", model: "openai/gpt-image-1.5", want: false},
		{name: "short model", model: "gpt", want: false},
		{name: "non image model", model: "gpt-5", want: false},
	}

	for i := range tests {
		if got := isOpenAIImageOnlyModel(tests[i].model); got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func BenchmarkIsOpenAIImageOnlyModel(b *testing.B) {
	for b.Loop() {
		if !isOpenAIImageOnlyModel(" GPT-Image-1.5 ") {
			b.Fatal("expected image-only model")
		}
	}
}

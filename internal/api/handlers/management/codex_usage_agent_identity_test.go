package management

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestFetchCodexUsageUsesAgentIdentityWithoutAccessToken(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}

	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-usage" {
			t.Errorf("ChatGPT-Account-ID = %q, want account-usage", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":12}}}`)
	}))
	defer server.Close()
	previousUsageURL := codexUsageURL
	codexUsageURL = server.URL
	defer func() { codexUsageURL = previousUsageURL }()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewCodexExecutor(nil))
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	auth := &coreauth.Auth{
		ID:         "agent-usage.json",
		Provider:   "codex",
		Attributes: map[string]string{},
		Metadata: map[string]any{
			"type":              "codex",
			"agent_runtime_id":  "runtime-usage",
			"agent_private_key": base64.StdEncoding.EncodeToString(der),
			"task_id":           "task-usage",
			"account_id":        "account-usage",
		},
	}

	payload, status, err := h.fetchCodexUsage(context.Background(), auth)
	if err != nil {
		t.Fatalf("fetchCodexUsage: %v", err)
	}
	if status != http.StatusOK || payload == nil {
		t.Fatalf("status/payload = %d/%#v", status, payload)
	}
	if !strings.HasPrefix(authorization, "AgentAssertion ") {
		t.Fatalf("Authorization = %q, want AgentAssertion", authorization)
	}
	if strings.HasPrefix(authorization, "Bearer ") {
		t.Fatalf("Authorization unexpectedly used Bearer: %q", authorization)
	}
}

func TestCodexRateLimitResetRequestsUseAgentIdentityWithoutAccessToken(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}

	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-reset" {
			t.Errorf("ChatGPT-Account-ID = %q, want account-reset", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"available_count":2}`)
		case http.MethodPost:
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			_, _ = io.WriteString(w, `{"code":"reset","windows_reset":1}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	previousDetailsURL := codexRateLimitResetCreditsURL
	previousConsumeURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsURL = server.URL + "/details"
	codexRateLimitResetCreditsConsumeURL = server.URL + "/consume"
	defer func() {
		codexRateLimitResetCreditsURL = previousDetailsURL
		codexRateLimitResetCreditsConsumeURL = previousConsumeURL
	}()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewCodexExecutor(nil))
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	auth := &coreauth.Auth{
		ID:       "agent-reset.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":              "codex",
			"auth_kind":         "agent_identity",
			"agent_runtime_id":  "runtime-reset",
			"agent_private_key": base64.StdEncoding.EncodeToString(der),
			"task_id":           "task-reset",
			"account_id":        "account-reset",
		},
	}

	details, status, err := h.fetchCodexRateLimitResetCreditDetails(context.Background(), auth)
	if err != nil || status != http.StatusOK || details == nil {
		t.Fatalf("details status/payload/err = %d/%#v/%v", status, details, err)
	}
	consume, status, err := h.consumeCodexRateLimitResetCredit(context.Background(), auth, "redeem-test")
	if err != nil || status != http.StatusOK || consume.Code != "reset" {
		t.Fatalf("consume status/payload/err = %d/%#v/%v", status, consume, err)
	}
	if len(authorizations) != 2 {
		t.Fatalf("authorization request count = %d, want 2", len(authorizations))
	}
	for i, authorization := range authorizations {
		if !strings.HasPrefix(authorization, "AgentAssertion ") {
			t.Fatalf("Authorization[%d] = %q, want AgentAssertion", i, authorization)
		}
		if strings.HasPrefix(authorization, "Bearer ") {
			t.Fatalf("Authorization[%d] unexpectedly used Bearer: %q", i, authorization)
		}
	}
}

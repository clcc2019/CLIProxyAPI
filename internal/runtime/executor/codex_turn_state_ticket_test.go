package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const testCodexTurnStateTicket = "gAAAAA12"

func testCodexTurnStateTicketConfig() config.CodexTurnStateTicketConfig {
	return config.CodexTurnStateTicketConfig{
		Enabled:         true,
		TargetLength:    len(testCodexTurnStateTicket),
		TTLSeconds:      3600,
		FailClosed:      true,
		Models:          []string{"gpt-ticket"},
		HarvestProxyURL: "direct",
	}
}

func testCodexOAuthAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	}
}

func TestCodexTurnStateTicketProviderScopesAndGatesTickets(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: testCodexTurnStateTicketConfig()}}
	provider := newCodexTurnStateTicketProvider(cfg)
	auth := testCodexOAuthAuth("auth-a")
	provider.tickets.put(codexTurnStateTicketKey(auth, "gpt-ticket"), &codexTurnStateTicket{
		model:     "gpt-ticket",
		state:     testCodexTurnStateTicket,
		expiresAt: time.Now().Add(time.Hour),
	})

	headers := make(http.Header)
	if err := provider.apply(context.Background(), auth, "gpt-ticket", headers, true); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if got := headers.Get(codexHeaderTurnState); got != testCodexTurnStateTicket {
		t.Fatalf("turn state = %q, want %q", got, testCodexTurnStateTicket)
	}

	otherAuth := testCodexOAuthAuth("auth-b")
	if err := provider.apply(context.Background(), otherAuth, "gpt-ticket", make(http.Header), true); !errors.Is(err, errCodexTurnStateTicketUnavailable) {
		t.Fatalf("other auth apply() error = %v, want ticket unavailable", err)
	}
	if err := provider.apply(context.Background(), auth, "other-model", make(http.Header), true); err != nil {
		t.Fatalf("ungated model apply() error = %v", err)
	}

	expired := make(http.Header)
	provider.tickets.put(codexTurnStateTicketKey(auth, "gpt-ticket"), &codexTurnStateTicket{
		model:     "gpt-ticket",
		state:     testCodexTurnStateTicket,
		expiresAt: time.Now().Add(-time.Second),
	})
	if err := provider.apply(context.Background(), auth, "gpt-ticket", expired, true); !errors.Is(err, errCodexTurnStateTicketUnavailable) {
		t.Fatalf("expired ticket apply() error = %v, want ticket unavailable", err)
	}
}

func TestCodexTurnStateTicketProviderFiltersAuthFiles(t *testing.T) {
	ticketCfg := testCodexTurnStateTicketConfig()
	ticketCfg.AuthFiles = []string{"codex-primary.json"}
	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: ticketCfg}}
	provider := newCodexTurnStateTicketProvider(cfg)
	selected := testCodexOAuthAuth("selected-auth")
	selected.FileName = "/var/lib/cliproxy/codex-primary.json"
	unselected := testCodexOAuthAuth("unselected-auth")
	unselected.FileName = "/var/lib/cliproxy/codex-secondary.json"
	for _, auth := range []*cliproxyauth.Auth{selected, unselected} {
		provider.tickets.put(codexTurnStateTicketKey(auth, "gpt-ticket"), &codexTurnStateTicket{
			model:     "gpt-ticket",
			state:     testCodexTurnStateTicket,
			expiresAt: time.Now().Add(time.Hour),
		})
	}

	selectedHeaders := make(http.Header)
	if err := provider.apply(context.Background(), selected, "gpt-ticket", selectedHeaders, true); err != nil {
		t.Fatalf("selected auth apply() error = %v", err)
	}
	if got := selectedHeaders.Get(codexHeaderTurnState); got != testCodexTurnStateTicket {
		t.Fatalf("selected auth turn state = %q, want %q", got, testCodexTurnStateTicket)
	}

	unselectedHeaders := make(http.Header)
	if err := provider.apply(context.Background(), unselected, "gpt-ticket", unselectedHeaders, true); err != nil {
		t.Fatalf("unselected auth apply() error = %v, want no-op", err)
	}
	if got := unselectedHeaders.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("unselected auth received turn state %q", got)
	}

	idCfg := ticketCfg
	idCfg.AuthFiles = []string{"id-only"}
	idAuth := testCodexOAuthAuth("id-only")
	if !codexTurnStateTicketAuthAllowed(idCfg, idAuth) {
		t.Fatal("auth ID selector did not match")
	}
}

func TestCodexTurnStateTicketProviderHarvestsAndInjects(t *testing.T) {
	const model = "gpt-ticket"
	const state = testCodexTurnStateTicket
	var seenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Host != "chatgpt.com" {
			t.Errorf("harvest request = %s host=%q, want POST host=chatgpt.com", r.Method, r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(codexHeaderOpenAIBeta); got != "responses=experimental" {
			t.Errorf("OpenAI-Beta = %q", got)
		}
		if strings.TrimSpace(r.Header.Get("session_id")) == "" {
			t.Error("session_id is empty")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read probe body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &seenBody); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		w.Header().Set(codexHeaderTurnState, state)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousURL := codexTurnStateTicketHarvestURL
	codexTurnStateTicketHarvestURL = server.URL
	t.Cleanup(func() { codexTurnStateTicketHarvestURL = previousURL })

	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: testCodexTurnStateTicketConfig()}}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	auth := testCodexOAuthAuth("harvest-auth")
	if _, err := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	provider := newCodexTurnStateTicketProvider(cfg)
	provider.lifecycleMu.Lock()
	provider.manager = manager
	provider.lifecycleMu.Unlock()

	provider.refresh(context.Background())
	if seenBody["model"] != model {
		t.Fatalf("probe model = %#v, want %q", seenBody["model"], model)
	}
	if seenBody["store"] != false || seenBody["stream"] != true {
		t.Fatalf("probe body flags = %#v", seenBody)
	}
	input, ok := seenBody["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("probe input = %#v", seenBody["input"])
	}

	headers := make(http.Header)
	if err := provider.apply(context.Background(), auth, model, headers, true); err != nil {
		t.Fatalf("apply harvested ticket: %v", err)
	}
	if got := headers.Get(codexHeaderTurnState); got != state {
		t.Fatalf("injected ticket = %q, want %q", got, state)
	}
}

func TestCodexExecutorInjectsHarvestedTicketIntoOfficialHTTPResponses(t *testing.T) {
	ticketCfg := testCodexTurnStateTicketConfig()
	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: ticketCfg}}
	executor := NewCodexExecutor(cfg)
	auth := testCodexOAuthAuth("http-auth")
	executor.turnStateTickets.tickets.put(codexTurnStateTicketKey(auth, "gpt-ticket"), &codexTurnStateTicket{
		model:     "gpt-ticket",
		state:     testCodexTurnStateTicket,
		expiresAt: time.Now().Add(time.Hour),
	})

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		auth,
		sdktranslator.FromString("openai-response"),
		"",
		"https://chatgpt.com/backend-api/codex/responses",
		cliproxyexecutor.Request{Model: "gpt-ticket"},
		[]byte(`{"model":"gpt-ticket","input":[]}`),
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := call.prepared.httpReq.Header.Get(codexHeaderTurnState); got != testCodexTurnStateTicket {
		t.Fatalf("prepared ticket = %q, want %q", got, testCodexTurnStateTicket)
	}
}

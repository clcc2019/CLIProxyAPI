package executor

import (
	"context"
	"encoding/base64"
	"encoding/binary"
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

var testCodexTurnStateTicket = makeTestCodexTurnStateTicket(codexTurnStateTicketDefaultTargetLength)

func makeTestCodexTurnStateTicket(targetLength int) string {
	blocks := 10
	if targetLength == codexTurnStateTicketSolStateLength {
		blocks = 11
	}
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Add(-time.Minute).Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func testCodexProbeCompletedSSE(model string) string {
	return "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"" + model + "\"}}\n\n"
}

func TestCodexTurnStateTicketProbeCompleted(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "completed",
			body: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			want: true,
		},
		{
			name: "completed with multiline data",
			body: "data: {\"type\":\"response.\n" + "data: completed\"}\n\n",
			want: false,
		},
		{
			name: "incomplete",
			body: "data: {\"type\":\"response.incomplete\"}\n\n",
			want: false,
		},
		{
			name: "missing terminal event",
			body: "data: {\"type\":\"response.output_text.delta\"}\n\n",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexTurnStateTicketProbeCompleted([]byte(tc.body)); got != tc.want {
				t.Fatalf("probe completion = %t, want %t", got, tc.want)
			}
		})
	}
}

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

func TestCodexTurnStateTicketConfigDefaultsToAstra(t *testing.T) {
	got := codexTurnStateTicketConfigForConfig(nil)
	if len(got.Models) != 1 || got.Models[0] != codexTurnStateTicketDefaultModelAstra {
		t.Fatalf("default ticket models = %#v, want [%q]", got.Models, codexTurnStateTicketDefaultModelAstra)
	}

	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: config.CodexTurnStateTicketConfig{
		Models: []string{codexTurnStateTicketDefaultModelAstra, codexTurnStateTicketDefaultModelSol},
	}}}
	got = codexTurnStateTicketConfigForConfig(cfg)
	if len(got.Models) != 2 || got.Models[0] != codexTurnStateTicketDefaultModelAstra || got.Models[1] != codexTurnStateTicketDefaultModelSol {
		t.Fatalf("configured ticket target models = %#v, want [%q, %q]", got.Models, codexTurnStateTicketDefaultModelAstra, codexTurnStateTicketDefaultModelSol)
	}

	cfg.Codex.OpenAICodexTicket.Models = []string{codexTurnStateTicketDefaultModelAstra}
	got = codexTurnStateTicketConfigForConfig(cfg)
	if len(got.Models) != 1 || got.Models[0] != codexTurnStateTicketDefaultModelAstra {
		t.Fatalf("configured ticket target models = %#v, want [%q]", got.Models, codexTurnStateTicketDefaultModelAstra)
	}
}

func TestCodexTurnStateTicketSolLengthAcceptedWithLegacyDefaultTarget(t *testing.T) {
	state := makeTestCodexTurnStateTicket(codexTurnStateTicketSolStateLength)
	if !codexTurnStateTicketStateValid(state, codexTurnStateTicketDefaultTargetLength) {
		t.Fatalf("Sol ticket length %d was rejected with legacy target length %d", len(state), codexTurnStateTicketDefaultTargetLength)
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
	otherAuth.Attributes = map[string]string{"base_url": "https://example.com/backend-api/codex"}
	otherHeaders := make(http.Header)
	if err := provider.apply(context.Background(), otherAuth, "gpt-ticket", otherHeaders, true); err != nil {
		t.Fatalf("ineligible auth apply() error = %v, want no-op", err)
	}
	if got := otherHeaders.Get(codexHeaderTurnState); got != "" {
		t.Fatalf("ineligible auth received turn state %q", got)
	}
	otherModelHeaders := make(http.Header)
	if err := provider.apply(context.Background(), auth, "other-model", otherModelHeaders, true); err != nil {
		t.Fatalf("other model apply() error = %v", err)
	}
	if got := otherModelHeaders.Get(codexHeaderTurnState); got != testCodexTurnStateTicket {
		t.Fatalf("other model turn state = %q, want %q", got, testCodexTurnStateTicket)
	}

	expired := make(http.Header)
	provider.tickets.put(codexTurnStateTicketKey(auth, "gpt-ticket"), &codexTurnStateTicket{
		model:     "gpt-ticket",
		state:     testCodexTurnStateTicket,
		expiresAt: time.Now().Add(-time.Second),
	})
	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer probeServer.Close()
	previousURL := codexTurnStateTicketHarvestURL
	codexTurnStateTicketHarvestURL = probeServer.URL
	t.Cleanup(func() { codexTurnStateTicketHarvestURL = previousURL })
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
	state := testCodexTurnStateTicket
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testCodexProbeCompletedSSE(model))
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

func TestCodexTurnStateTicketRefreshErrorOmitsProbeDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"error":"turn state unavailable"}`)
	}))
	defer server.Close()

	previousURL := codexTurnStateTicketHarvestURL
	codexTurnStateTicketHarvestURL = server.URL
	t.Cleanup(func() { codexTurnStateTicketHarvestURL = previousURL })

	cfg := &config.Config{Codex: config.CodexConfig{OpenAICodexTicket: testCodexTurnStateTicketConfig()}}
	provider := newCodexTurnStateTicketProvider(cfg)
	err := provider.refreshAuth(context.Background(), testCodexOAuthAuth("diagnostic-auth"))
	if err == nil {
		t.Fatal("refreshAuth() error = nil, want missing turn-state error")
	}

	message := err.Error()
	want := "model gpt-ticket: harvest request failed: harvest response is HTTP 200 but missing " + codexHeaderTurnState
	if message != want {
		t.Fatalf("refreshAuth() error = %q, want %q", message, want)
	}
	for _, forbidden := range []string{
		"diagnostic",
		"request_method",
		"request_url",
		"request_headers",
		"response_headers",
		"response_body",
		server.URL,
		"Reply with exactly: pong",
	} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("refreshAuth() error exposed probe details %q: %q", forbidden, message)
		}
	}
	if strings.Contains(message, "oauth-token") {
		t.Fatalf("refreshAuth() error exposed OAuth token: %q", message)
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

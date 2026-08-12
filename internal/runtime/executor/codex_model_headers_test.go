package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRemoteCompactionV2TriggerFeatureGate(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"compaction_trigger","reason":"token_limit"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}
	executor := NewCodexExecutor(&config.Config{})

	for _, test := range []struct {
		name string
		beta string
		want bool
	}{
		{name: "enabled", beta: "other,remote_compaction_v2", want: true},
		{name: "disabled", beta: "other", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contextWithGinHeaders(map[string]string{"X-Codex-Beta-Features": test.beta})
			call, err := executor.prepareCodexHTTPCall(ctx, nil, sdktranslator.FromString("openai-response"), "", "https://chatgpt.com/backend-api/codex/responses", req, body, "token", true)
			if err != nil {
				t.Fatalf("prepareCodexHTTPCall: %v", err)
			}
			got := gjson.GetBytes(call.prepared.body, "input.0.type").String() == "compaction_trigger"
			if got != test.want {
				t.Fatalf("HTTP trigger preserved = %v, want %v; body=%s", got, test.want, call.prepared.body)
			}

			ws, err := NewCodexWebsocketsExecutor(&config.Config{}).prepareCodexWebsocketRequest(ctx, nil, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Headers: http.Header{"X-Codex-Beta-Features": []string{test.beta}}}, body, "token", "https://chatgpt.com/backend-api/codex/responses")
			if err != nil {
				t.Fatalf("prepareCodexWebsocketRequest: %v", err)
			}
			defer ws.unlockSession()
			got = gjson.GetBytes(ws.body, "input.0.type").String() == "compaction_trigger"
			if got != test.want {
				t.Fatalf("WebSocket trigger preserved = %v, want %v; body=%s", got, test.want, ws.body)
			}
		})
	}
}

const (
	codexLunaUserAgentForTest  = "codex-tui/0.144.1 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.1)"
	codexLunaOriginatorForTest = "codex-tui"
)

func codexLunaHeaderOverrideAuthForTest() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "codex-luna-header-override",
		Provider: "codex",
		Metadata: map[string]any{"email": "luna@example.com"},
		Attributes: map[string]string{
			"header:User-Agent": "client-user-agent/1.0",
			"header:Originator": "client-originator",
		},
	}
}

func TestPrepareCodexHTTPCallAppliesLunaModelHeaderOverrides(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	body := []byte(`{"model":"gpt-5.6-luna","input":[]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-luna", Payload: body}

	call, err := executor.prepareCodexHTTPCall(
		context.Background(),
		codexLunaHeaderOverrideAuthForTest(),
		sdktranslator.FromString("openai-response"),
		"",
		"https://chatgpt.com/backend-api/codex/responses",
		req,
		body,
		"oauth-token",
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexHTTPCall() error = %v", err)
	}
	if got := call.prepared.httpReq.Header.Get("User-Agent"); got != codexLunaUserAgentForTest {
		t.Fatalf("User-Agent = %q, want %q", got, codexLunaUserAgentForTest)
	}
	if got := call.prepared.httpReq.Header.Get("Originator"); got != codexLunaOriginatorForTest {
		t.Fatalf("Originator = %q, want %q", got, codexLunaOriginatorForTest)
	}
}

func TestPrepareCodexWebsocketRequestAppliesLunaModelHeaderOverrides(t *testing.T) {
	executor := NewCodexWebsocketsExecutor(&config.Config{})
	body := []byte(`{"model":"gpt-5.6-luna","input":[]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-luna", Payload: body}

	prepared, err := executor.prepareCodexWebsocketRequest(
		context.Background(),
		codexLunaHeaderOverrideAuthForTest(),
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
		body,
		"oauth-token",
		"https://chatgpt.com/backend-api/codex/responses",
	)
	if err != nil {
		t.Fatalf("prepareCodexWebsocketRequest() error = %v", err)
	}
	defer prepared.unlockSession()

	if got := prepared.wsHeaders.Get("User-Agent"); got != codexLunaUserAgentForTest {
		t.Fatalf("User-Agent = %q, want %q", got, codexLunaUserAgentForTest)
	}
	if got := prepared.wsHeaders.Get("Originator"); got != codexLunaOriginatorForTest {
		t.Fatalf("Originator = %q, want %q", got, codexLunaOriginatorForTest)
	}
}

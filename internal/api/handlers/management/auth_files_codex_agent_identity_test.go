package management

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/ssh"
)

func TestPatchCodexAuthModeCreatesAndReusesAgentIdentity(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	var registrations atomic.Int32
	token := codexAgentModeTestJWT(t)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registrations.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Host != "agent-registration.invalid" || r.URL.Path != "/api/accounts/v1/agent/register" {
			t.Errorf("proxied URL = %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		for header, want := range map[string]string{
			"User-Agent":                            "codex_vscode/0.155.0 (Linux; x86_64)",
			"Originator":                            "codex_vscode",
			"Version":                               "0.155.0",
			"X-Codex-Beta-Features":                 "feature-a",
			"X-Codex-Installation-Id":               "installation-test",
			"x-responsesapi-include-timing-metrics": "true",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		var payload codexAgentRegistrationRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode registration: %v", err)
		}
		if payload.ABOM.AgentVersion != "0.155.0" ||
			payload.ABOM.AgentHarnessID != codexAgentRegistrationHarnessID ||
			payload.ABOM.RunningLocation != codexAgentRunningLocation {
			t.Errorf("ABOM = %#v", payload.ABOM)
		}
		publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(payload.Key))
		if err != nil {
			t.Errorf("agent_public_key is invalid: %v", err)
		} else if publicKey.Type() != ssh.KeyAlgoED25519 {
			t.Errorf("public key type = %q", publicKey.Type())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agent_runtime_id":"runtime-created"}`)
	}))
	defer proxyServer.Close()

	previousURL := codexAgentIdentityRegisterURL
	codexAgentIdentityRegisterURL = "http://agent-registration.invalid/api/accounts/v1/agent/register"
	t.Cleanup(func() { codexAgentIdentityRegisterURL = previousURL })

	doc := map[string]any{
		"type":         "codex",
		"access_token": token,
		"proxy_url":    proxyServer.URL,
		"custom_field": "preserved",
		"client_features": map[string]any{
			"user_agent":      "codex_vscode/0.155.0 (Linux; x86_64)",
			"originator":      "codex_vscode",
			"installation_id": "installation-test",
			"headers": map[string]any{
				"Version":                               "0.155.0",
				"X-Codex-Beta-Features":                 "feature-a",
				"x-responsesapi-include-timing-metrics": "true",
			},
		},
	}
	h, manager, authPath := newCodexAuthModeTestHandler(t, doc)

	rec := performCodexAuthModeRequest(t, h, "codex.json", codexAuthModeAgentIdentity)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if registrations.Load() != 1 {
		t.Fatalf("registrations = %d, want 1", registrations.Load())
	}
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "agent_private_key") {
		t.Fatalf("response leaked credential material: %s", rec.Body.String())
	}
	var response struct {
		Mode string         `json:"mode"`
		File map[string]any `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Mode != codexAuthModeAgentIdentity ||
		response.File["auth_mode"] != codexAuthModeAgentIdentity ||
		response.File["has_access_token"] != true ||
		response.File["has_agent_identity"] != true {
		t.Fatalf("mode response = %#v", response)
	}

	stored := readCodexAuthModeTestDocument(t, authPath)
	if stored["auth_kind"] != coreauth.CodexAuthKindAgentIdentity {
		t.Fatalf("auth_kind = %#v", stored["auth_kind"])
	}
	if stored["access_token"] != token || stored["custom_field"] != "preserved" {
		t.Fatalf("existing fields were not preserved: %#v", stored)
	}
	if stored["agent_runtime_id"] != "runtime-created" {
		t.Fatalf("agent_runtime_id = %#v", stored["agent_runtime_id"])
	}
	if pinned, ok := stored[coreauth.AuthFileCodexClientProfilePinnedKey].(bool); !ok || !pinned {
		t.Fatalf("client profile pinned = %#v", stored[coreauth.AuthFileCodexClientProfilePinnedKey])
	}
	privateKeyRaw, _ := stored["agent_private_key"].(string)
	privateDER, err := base64.StdEncoding.DecodeString(privateKeyRaw)
	if err != nil {
		t.Fatalf("decode agent private key: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(privateDER)
	if err != nil {
		t.Fatalf("parse agent private key: %v", err)
	}
	if privateKey, ok := parsed.(ed25519.PrivateKey); !ok || len(privateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key = %T, want Ed25519", parsed)
	}
	for key, want := range map[string]string{
		"account_id":      "account-test",
		"chatgpt_user_id": "user-test",
		"email":           "agent@example.com",
		"plan_type":       "plus",
	} {
		if got := stored[key]; got != want {
			t.Fatalf("%s = %#v, want %q", key, got, want)
		}
	}

	rec = performCodexAuthModeRequest(t, h, "codex.json", codexAuthModeAccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("token switch status = %d, body=%s", rec.Code, rec.Body.String())
	}
	stored = readCodexAuthModeTestDocument(t, authPath)
	if stored["auth_kind"] != coreauth.CodexAuthKindOAuth || stored["agent_runtime_id"] != "runtime-created" {
		t.Fatalf("token mode did not retain Agent Identity: %#v", stored)
	}
	if auth, ok := manager.GetByFileName("codex.json"); !ok || coreauth.CodexAuthUsesAgentIdentity(auth) {
		t.Fatalf("manager auth did not switch to access token: %#v", auth)
	}

	rec = performCodexAuthModeRequest(t, h, "codex.json", codexAuthModeAgentIdentity)
	if rec.Code != http.StatusOK {
		t.Fatalf("reuse status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if registrations.Load() != 1 {
		t.Fatalf("registrations = %d after reuse, want 1", registrations.Load())
	}
	if auth, ok := manager.GetByFileName("codex.json"); !ok || !coreauth.CodexAuthUsesAgentIdentity(auth) {
		t.Fatalf("manager auth did not switch back to Agent Identity: %#v", auth)
	}
}

func TestPatchCodexAuthModeRegistrationFailureDoesNotWriteFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"contains-sensitive-upstream-details"}`)
	}))
	defer server.Close()
	previousURL := codexAgentIdentityRegisterURL
	codexAgentIdentityRegisterURL = server.URL
	t.Cleanup(func() { codexAgentIdentityRegisterURL = previousURL })

	h, _, authPath := newCodexAuthModeTestHandler(t, map[string]any{
		"type":         "codex",
		"access_token": codexAgentModeTestJWT(t),
		"note":         "must-stay",
	})
	before, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	rec := performCodexAuthModeRequest(t, h, "codex.json", codexAuthModeAgentIdentity)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "contains-sensitive-upstream-details") {
		t.Fatalf("response exposed upstream body: %s", rec.Body.String())
	}
	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("auth file changed after failed registration\nbefore=%s\nafter=%s", before, after)
	}
}

func TestPatchCodexAuthModeRejectsMissingAccessToken(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h, _, authPath := newCodexAuthModeTestHandler(t, map[string]any{
		"type": "codex",
		"note": "no token",
	})
	before, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	rec := performCodexAuthModeRequest(t, h, "codex.json", codexAuthModeAgentIdentity)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("status/body = %d/%s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("auth file changed after missing-token rejection")
	}
}

func newCodexAuthModeTestHandler(t *testing.T, doc map[string]any) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex.json")
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	encoded = append(encoded, '\n')
	if err = os.WriteFile(authPath, encoded, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	auth, err := coreauth.NewAuthFromAuthFileData(encoded, coreauth.AuthFileProjectionOptions{
		ID:                    "codex.json",
		Path:                  authPath,
		BaseDir:               authDir,
		FileName:              "codex.json",
		UseBaseNameAsFileName: true,
	})
	if err != nil {
		t.Fatalf("project auth file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err = manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	t.Cleanup(h.Close)
	return h, manager, authPath
}

func performCodexAuthModeRequest(t *testing.T, h *Handler, name, mode string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(codexAuthModeRequest{Name: name, Mode: mode})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/codex-auth-mode", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchCodexAuthMode(ctx)
	return rec
}

func readCodexAuthModeTestDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode auth file: %v", err)
	}
	return doc
}

func codexAgentModeTestJWT(t *testing.T) string {
	t.Helper()
	return testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-test",
			"chatgpt_user_id":    "user-test",
			"chatgpt_plan_type":  "plus",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "agent@example.com",
		},
	})
}

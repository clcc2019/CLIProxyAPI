package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewFileSynthesizer(t *testing.T) {
	synth := NewFileSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestFileSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewFileSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_EmptyAuthDir(t *testing.T) {
	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     "",
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestSupportedSynthesizedAuthProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "claude", provider: " Claude ", want: true},
		{name: "codex", provider: "\tCodex\r\n", want: true},
		{name: "kimi", provider: "Kimi", want: true},
		{name: "xai", provider: "XAI", want: true},
		{name: "openai compatibility", provider: "OpenAI-Compatibility", want: true},
		{name: "unsupported", provider: "anthropic", want: false},
		{name: "empty", provider: " ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSupportedSynthesizedAuthProvider(tt.provider); got != tt.want {
				t.Fatalf("isSupportedSynthesizedAuthProvider(%q) = %t, want %t", tt.provider, got, tt.want)
			}
		})
	}
}

func BenchmarkSupportedSynthesizedAuthProvider(b *testing.B) {
	for b.Loop() {
		if !isSupportedSynthesizedAuthProvider(" OpenAI-Compatibility ") {
			b.Fatal("expected supported provider")
		}
	}
}

func TestFileSynthesizer_Synthesize_NonExistentDir(t *testing.T) {
	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     "/non/existent/path",
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_ValidAuthFile(t *testing.T) {
	tempDir := t.TempDir()

	// Create a valid auth file
	authData := map[string]any{
		"type":      "claude",
		"email":     "test@example.com",
		"proxy_url": "http://proxy.local",
		"prefix":    "test-prefix",
		"headers": map[string]string{
			" X-Test ": " value ",
			"X-Empty":  "  ",
		},
		"disable_cooling": true,
		"request_retry":   2,
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "claude-auth.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "test@example.com" {
		t.Errorf("expected label test@example.com, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "test-prefix" {
		t.Errorf("expected prefix test-prefix, got %s", auths[0].Prefix)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if got := auths[0].Attributes["header:X-Test"]; got != "value" {
		t.Errorf("expected header:X-Test value, got %q", got)
	}
	if _, ok := auths[0].Attributes["header:X-Empty"]; ok {
		t.Errorf("expected header:X-Empty to be absent, got %q", auths[0].Attributes["header:X-Empty"])
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling true, got %v", auths[0].Metadata["disable_cooling"])
	}
	if v, ok := auths[0].Metadata["request_retry"].(float64); !ok || int(v) != 2 {
		t.Errorf("expected request_retry 2, got %v", auths[0].Metadata["request_retry"])
	}
	if auths[0].Status != coreauth.StatusActive {
		t.Errorf("expected status active, got %s", auths[0].Status)
	}
}

func TestSynthesizeAuthFile_AppliesAndValidatesWeight(t *testing.T) {
	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: t.TempDir(),
		Now:     time.Now(),
	}
	for _, tc := range []struct {
		name       string
		weightJSON string
		wantWeight string
		wantAuth   bool
	}{
		{name: "integer", weightJSON: "5", wantWeight: "5", wantAuth: true},
		{name: "numeric string", weightJSON: `"3"`, wantWeight: "3", wantAuth: true},
		{name: "zero", weightJSON: "0", wantWeight: "0", wantAuth: true},
		{name: "negative", weightJSON: "-1", wantWeight: "0", wantAuth: true},
		{name: "fraction", weightJSON: "1.5", wantAuth: false},
		{name: "too large", weightJSON: "1000001", wantAuth: false},
		{name: "non numeric", weightJSON: `"abc"`, wantAuth: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"type":"claude","weight":` + tc.weightJSON + `}`)
			auths := SynthesizeAuthFile(ctx, filepath.Join(ctx.AuthDir, tc.name+".json"), data)
			if !tc.wantAuth {
				if len(auths) != 0 {
					t.Fatalf("SynthesizeAuthFile() auths = %#v, want none", auths)
				}
				return
			}
			if len(auths) != 1 {
				t.Fatalf("SynthesizeAuthFile() auth count = %d, want 1", len(auths))
			}
			if got := auths[0].Attributes[coreauth.AttributeWeight]; got != tc.wantWeight {
				t.Fatalf("weight attribute = %q, want %q", got, tc.wantWeight)
			}
		})
	}
}

func TestFileSynthesizer_Synthesize_CodexAuthFileUserAgentOverride(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":       "codex",
		"email":      "codex@example.com",
		"user_agent": "my-codex-client/1.0",
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "codex-auth.json"), data, 0o644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if got := auths[0].Attributes["header:User-Agent"]; got != "my-codex-client/1.0" {
		t.Fatalf("header:User-Agent = %q, want %q", got, "my-codex-client/1.0")
	}
}

func TestFileSynthesizer_Synthesize_CodexAuthFileDefaultsWebsocketsEnabled(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":         "codex",
		"email":        "codex@example.com",
		"access_token": "oauth-token",
	}
	data, _ := json.Marshal(authData)
	if err := os.WriteFile(filepath.Join(tempDir, "codex-auth.json"), data, 0o644); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if got := auths[0].Attributes["websockets"]; got != "true" {
		t.Fatalf("websockets attr = %q, want true; attrs=%#v", got, auths[0].Attributes)
	}
	if _, ok := auths[0].Metadata["websockets"]; ok {
		t.Fatalf("default websockets should not be persisted into metadata: %#v", auths[0].Metadata)
	}
}

func TestFileSynthesizer_Synthesize_CodexMinimalMetadataWithoutIDToken(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":         "codex",
		"email":        "codex@example.com",
		"access_token": "oauth-token",
		"account_id":   "acct_123",
		"plan_type":    "plus",
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "codex-auth.json"), data, 0o644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if got := auths[0].Attributes["email"]; got != "codex@example.com" {
		t.Fatalf("email = %q, want %q", got, "codex@example.com")
	}
	if got := auths[0].Attributes["account_id"]; got != "acct_123" {
		t.Fatalf("account_id = %q, want %q", got, "acct_123")
	}
	if got := auths[0].Attributes["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %q, want %q", got, "plus")
	}
}

func TestFileSynthesizer_Synthesize_PreservesCodexAgentIdentity(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":              "codex",
		"auth_kind":         "agent_identity",
		"agent_runtime_id":  "runtime-reload",
		"agent_private_key": "private-reload",
		"task_id":           "task-reload",
		"account_id":        "account-reload",
	}
	data, err := json.Marshal(authData)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err = os.WriteFile(filepath.Join(tempDir, "codex-agent.json"), data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	auths, err := NewFileSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize auth file: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if got := auth.Attributes["auth_kind"]; got != "agent_identity" {
		t.Fatalf("auth_kind = %q, want agent_identity", got)
	}
	for key, want := range map[string]string{
		"agent_runtime_id":  "runtime-reload",
		"agent_private_key": "private-reload",
		"task_id":           "task-reload",
		"account_id":        "account-reload",
	} {
		if got := auth.Metadata[key]; got != want {
			t.Fatalf("metadata[%q] = %#v, want %q", key, got, want)
		}
	}
}

func TestFileSynthesizer_Synthesize_OpenAISessionExportAsCodexAuth(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"WARNING_BANNER": "sensitive",
		"accessToken":    "oauth-token",
		"authProvider":   "openai",
		"user": map[string]any{
			"email": "codex@example.com",
		},
		"account": map[string]any{
			"id":       "acct_123",
			"planType": "plus",
		},
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "codex-session.json"), data, 0o644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if got := auths[0].Provider; got != "codex" {
		t.Fatalf("provider = %q, want %q", got, "codex")
	}
	if got := auths[0].Metadata["access_token"]; got != "oauth-token" {
		t.Fatalf("access_token = %#v, want %q", got, "oauth-token")
	}
	if got := auths[0].Attributes["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %q, want %q", got, "plus")
	}
}

func TestFileSynthesizer_Synthesize_IgnoresLookalikeWithoutOpenAIProvider(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"accessToken": "oauth-token",
		"user": map[string]any{
			"email": "codex@example.com",
		},
		"account": map[string]any{
			"id": "acct_123",
		},
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "codex-session.json"), data, 0o644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected no synthesized auths, got %d", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_SkipsInvalidFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Create various invalid files
	_ = os.WriteFile(filepath.Join(tempDir, "not-json.txt"), []byte("text content"), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "invalid.json"), []byte("not valid json"), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "empty.json"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "no-type.json"), []byte(`{"email": "test@example.com"}`), 0644)

	// Create one valid file
	validData, _ := json.Marshal(map[string]any{"type": "claude", "email": "valid@example.com"})
	_ = os.WriteFile(filepath.Join(tempDir, "valid.json"), validData, 0644)

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("only valid auth file should be processed, got %d", len(auths))
	}
	if auths[0].Label != "valid@example.com" {
		t.Errorf("expected label valid@example.com, got %s", auths[0].Label)
	}
}

func TestFileSynthesizer_Synthesize_SkipsDirectories(t *testing.T) {
	tempDir := t.TempDir()

	// Create a subdirectory with a json file inside
	subDir := filepath.Join(tempDir, "subdir.json")
	err := os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	// Create a valid file in root
	validData, _ := json.Marshal(map[string]any{"type": "claude"})
	_ = os.WriteFile(filepath.Join(tempDir, "valid.json"), validData, 0644)

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_RelativeID(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{"type": "claude"}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "my-auth.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	// ID should be relative path
	if auths[0].ID != "my-auth.json" {
		t.Errorf("expected ID my-auth.json, got %s", auths[0].ID)
	}
}

func TestFileSynthesizer_Synthesize_PrefixValidation(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		wantPrefix string
	}{
		{"valid prefix", "myprefix", "myprefix"},
		{"prefix with slashes trimmed", "/myprefix/", "myprefix"},
		{"prefix with spaces trimmed", "  myprefix  ", "myprefix"},
		{"prefix with internal slash rejected", "my/prefix", ""},
		{"empty prefix", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type":   "claude",
				"prefix": tt.prefix,
			}
			data, _ := json.Marshal(authData)
			_ = os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != 1 {
				t.Fatalf("expected 1 auth, got %d", len(auths))
			}
			if auths[0].Prefix != tt.wantPrefix {
				t.Errorf("expected prefix %q, got %q", tt.wantPrefix, auths[0].Prefix)
			}
		})
	}
}

func TestFileSynthesizer_Synthesize_PriorityParsing(t *testing.T) {
	tests := []struct {
		name     string
		priority any
		want     string
		hasValue bool
	}{
		{
			name:     "string with spaces",
			priority: " 10 ",
			want:     "10",
			hasValue: true,
		},
		{
			name:     "number",
			priority: 8,
			want:     "8",
			hasValue: true,
		},
		{
			name:     "invalid string",
			priority: "1x",
			hasValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type":     "claude",
				"priority": tt.priority,
			}
			data, _ := json.Marshal(authData)
			errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
			if errWriteFile != nil {
				t.Fatalf("failed to write auth file: %v", errWriteFile)
			}

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, errSynthesize := synth.Synthesize(ctx)
			if errSynthesize != nil {
				t.Fatalf("unexpected error: %v", errSynthesize)
			}
			if len(auths) != 1 {
				t.Fatalf("expected 1 auth, got %d", len(auths))
			}

			value, ok := auths[0].Attributes["priority"]
			if tt.hasValue {
				if !ok {
					t.Fatal("expected priority attribute to be set")
				}
				if value != tt.want {
					t.Fatalf("expected priority %q, got %q", tt.want, value)
				}
				return
			}
			if ok {
				t.Fatalf("expected priority attribute to be absent, got %q", value)
			}
		})
	}
}

func TestFileSynthesizer_Synthesize_OAuthExcludedModelsMerged(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":            "claude",
		"excluded_models": []string{"custom-model", "MODEL-B"},
	}
	data, _ := json.Marshal(authData)
	errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
	if errWriteFile != nil {
		t.Fatalf("failed to write auth file: %v", errWriteFile)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"claude": {"shared", "model-b"},
			},
		},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("unexpected error: %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	got := auths[0].Attributes["excluded_models"]
	want := "custom-model,model-b,shared"
	if got != want {
		t.Fatalf("expected excluded_models %q, got %q", want, got)
	}
}

func TestFileSynthesizer_Synthesize_NoteParsing(t *testing.T) {
	tests := []struct {
		name     string
		note     any
		want     string
		hasValue bool
	}{
		{
			name:     "valid string note",
			note:     "hello world",
			want:     "hello world",
			hasValue: true,
		},
		{
			name:     "string note with whitespace",
			note:     "  trimmed note  ",
			want:     "trimmed note",
			hasValue: true,
		},
		{
			name:     "empty string note",
			note:     "",
			hasValue: false,
		},
		{
			name:     "whitespace only note",
			note:     "   ",
			hasValue: false,
		},
		{
			name:     "non-string note ignored",
			note:     12345,
			hasValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type": "claude",
				"note": tt.note,
			}
			data, _ := json.Marshal(authData)
			errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
			if errWriteFile != nil {
				t.Fatalf("failed to write auth file: %v", errWriteFile)
			}

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, errSynthesize := synth.Synthesize(ctx)
			if errSynthesize != nil {
				t.Fatalf("unexpected error: %v", errSynthesize)
			}
			if len(auths) != 1 {
				t.Fatalf("expected 1 auth, got %d", len(auths))
			}

			value, ok := auths[0].Attributes["note"]
			if tt.hasValue {
				if !ok {
					t.Fatal("expected note attribute to be set")
				}
				if value != tt.want {
					t.Fatalf("expected note %q, got %q", tt.want, value)
				}
				return
			}
			if ok {
				t.Fatalf("expected note attribute to be absent, got %q", value)
			}
		})
	}
}

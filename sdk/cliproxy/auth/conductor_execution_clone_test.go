package auth

import (
	"testing"
	"time"
)

func TestCloneAuthForExecution_CodexUsesShallowClone(t *testing.T) {
	state := &ModelState{Status: StatusActive}
	auth := &Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "k"},
		Metadata:   map[string]any{"account_id": "a"},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": state,
		},
	}

	cloned := cloneAuthForExecution(" CoDeX ", auth)
	if cloned == nil {
		t.Fatal("cloneAuthForExecution returned nil")
	}
	if cloned == auth {
		t.Fatal("expected a distinct auth struct copy")
	}
	cloned.Attributes["region"] = "us"
	if auth.Attributes["region"] != "us" {
		t.Fatal("expected codex execution clone to reuse Attributes map")
	}
	cloned.Metadata["plan"] = "plus"
	if auth.Metadata["plan"] != "plus" {
		t.Fatal("expected codex execution clone to reuse Metadata map")
	}
	cloned.ModelStates["gpt-5-codex"] = &ModelState{Status: StatusError}
	if auth.ModelStates["gpt-5-codex"].Status != StatusError {
		t.Fatal("expected codex execution clone to reuse ModelStates map")
	}
}

func TestCloneAuthForExecution_NonCodexDeepClonesMaps(t *testing.T) {
	state := &ModelState{Status: StatusActive}
	auth := &Auth{
		ID:         "claude-auth",
		Provider:   "claude",
		Attributes: map[string]string{"priority": "10"},
		Metadata:   map[string]any{"email": "x@example.com"},
		ModelStates: map[string]*ModelState{
			"claude-3-5-sonnet": state,
		},
	}

	cloned := cloneAuthForExecution("claude", auth)
	if cloned == nil {
		t.Fatal("cloneAuthForExecution returned nil")
	}
	cloned.Attributes["priority"] = "20"
	if auth.Attributes["priority"] != "10" {
		t.Fatal("expected non-codex execution clone to deep copy Attributes map")
	}
	cloned.Metadata["email"] = "y@example.com"
	if auth.Metadata["email"] != "x@example.com" {
		t.Fatal("expected non-codex execution clone to deep copy Metadata map")
	}
	cloned.ModelStates["claude-3-5-sonnet"] = &ModelState{Status: StatusError}
	if auth.ModelStates["claude-3-5-sonnet"] != state {
		t.Fatal("expected non-codex execution clone to deep copy ModelStates map")
	}
	if cloned.ModelStates["claude-3-5-sonnet"] == state {
		t.Fatal("expected non-codex execution clone to deep copy model state")
	}
}

func TestAuthCloneForScheduler_MinimizesMutableMaps(t *testing.T) {
	auth := &Auth{
		ID:             "sched-auth",
		Provider:       "claude",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: parseMustTime(t, "2026-04-26T12:00:00Z"),
		Attributes: map[string]string{
			"priority":   "10",
			"websockets": "true",
			"api_key":    "secret",
		},
		Metadata: map[string]any{
			"websockets": true,
			"email":      "x@example.com",
		},
		ModelStates: map[string]*ModelState{
			"claude-3-5-sonnet": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: parseMustTime(t, "2026-04-26T12:05:00Z"),
				Quota:          QuotaState{Exceeded: true, BackoffLevel: 2},
				LastError:      &Error{Message: "boom"},
			},
		},
		LastError: &Error{Message: "top-level"},
	}

	cloned := auth.CloneForScheduler()
	if cloned == nil {
		t.Fatal("CloneForScheduler returned nil")
	}
	if cloned.Attributes["priority"] != "10" || cloned.Attributes["websockets"] != "true" {
		t.Fatalf("CloneForScheduler attributes = %#v", cloned.Attributes)
	}
	if _, ok := cloned.Attributes["api_key"]; ok {
		t.Fatalf("CloneForScheduler should drop unused attributes, got %#v", cloned.Attributes)
	}
	if cloned.Metadata["websockets"] != true {
		t.Fatalf("CloneForScheduler metadata = %#v", cloned.Metadata)
	}
	if _, ok := cloned.Metadata["email"]; ok {
		t.Fatalf("CloneForScheduler should drop unused metadata, got %#v", cloned.Metadata)
	}
	if cloned.LastError != nil {
		t.Fatalf("CloneForScheduler should clear LastError, got %#v", cloned.LastError)
	}
	state := cloned.ModelStates["claude-3-5-sonnet"]
	if state == nil {
		t.Fatal("CloneForScheduler lost model state")
	}
	if state.LastError != nil {
		t.Fatalf("CloneForScheduler model state should clear LastError, got %#v", state.LastError)
	}
	state.Quota.BackoffLevel = 9
	if auth.ModelStates["claude-3-5-sonnet"].Quota.BackoffLevel != 2 {
		t.Fatal("CloneForScheduler should deep copy model state quota")
	}
}

func TestAuthCloneForScheduler_CodexRetainsExecutionFields(t *testing.T) {
	auth := &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":           "sk-test",
			"base_url":          "https://chatgpt.com/backend-api/codex",
			"plan_type":         "plus",
			"header:X-Test":     "1",
			"installation_id":   "install-1",
			"header:Originator": "codex_vscode",
			"header:User-Agent": "codex_vscode/1.0.0",
			"websockets":        "true",
		},
		Metadata: map[string]any{
			"access_token":    "oauth-token",
			"refresh_token":   "refresh-token",
			"account_id":      "acct-1",
			"installation_id": "install-1",
			"user_agent":      "codex_vscode/1.0.0",
			"originator":      "codex_vscode",
			"websockets":      true,
		},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: parseMustTime(t, "2026-04-26T12:05:00Z"),
				Quota:          QuotaState{Exceeded: true, BackoffLevel: 2},
				LastError:      &Error{Message: "boom"},
			},
		},
		LastError: &Error{Message: "top-level"},
	}

	cloned := auth.CloneForScheduler()
	if cloned == nil {
		t.Fatal("CloneForScheduler returned nil")
	}
	if cloned.Attributes["api_key"] != "sk-test" || cloned.Attributes["plan_type"] != "plus" || cloned.Attributes["header:X-Test"] != "1" {
		t.Fatalf("CloneForScheduler codex attributes = %#v", cloned.Attributes)
	}
	if cloned.Metadata["access_token"] != "oauth-token" || cloned.Metadata["account_id"] != "acct-1" || cloned.Metadata["websockets"] != true {
		t.Fatalf("CloneForScheduler codex metadata = %#v", cloned.Metadata)
	}
	if cloned.LastError != nil {
		t.Fatalf("CloneForScheduler should clear LastError, got %#v", cloned.LastError)
	}
	state := cloned.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatal("CloneForScheduler lost codex model state")
	}
	if state.LastError != nil {
		t.Fatalf("CloneForScheduler codex model state should clear LastError, got %#v", state.LastError)
	}
	cloned.Attributes["api_key"] = "mutated"
	if auth.Attributes["api_key"] != "sk-test" {
		t.Fatal("CloneForScheduler should deep copy codex attributes")
	}
	cloned.Metadata["access_token"] = "mutated"
	if auth.Metadata["access_token"] != "oauth-token" {
		t.Fatal("CloneForScheduler should deep copy codex metadata")
	}
}

func TestAuthCloneForManagementSummaryDropsLargeTokenMetadata(t *testing.T) {
	auth := &Auth{
		ID:            "codex-auth",
		Index:         "idx-1",
		Provider:      "codex",
		FileName:      "codex.json",
		StatusMessage: "quota cached",
		Success:       11,
		Failed:        2,
		Attributes: map[string]string{
			"path":                           "/tmp/codex.json",
			"api_key":                        "sk-test",
			"base_url":                       "https://chatgpt.com/backend-api/codex",
			"header:X-Test":                  "drop-me",
			"header:User-Agent":              "codex_vscode/1.0.0",
			"originator":                     "codex_vscode",
			"header:Originator":              "codex_vscode",
			"header:X-Codex-Beta-Features":   "feature-a",
			"header:X-Codex-Installation-Id": "install-1",
			"header:x-responsesapi-include-timing-metrics": "true",
		},
		Metadata: map[string]any{
			"email":                             "x@example.com",
			"access_token":                      "large-access-token",
			"id_token":                          "large-id-token",
			"account_id":                        "acct-1",
			"refresh_token":                     "refresh-token",
			"plan_type":                         "plus",
			"chatgpt_subscription_active_until": "2026-06-19T11:44:26Z",
			"chatgpt_subscription_active_start": "2026-05-19T11:44:26Z",
			"subscription_active_days":          3,
			"websockets":                        true,
			"originator":                        "codex_vscode",
			"beta_features":                     "feature-a",
			"installation_id":                   "install-1",
			"include_timing_metrics":            true,
			"token":                             map[string]any{"refresh_token": "nested-refresh", "access_token": "nested-access"},
			"subscription":                      map[string]any{"current_period_end": "2026-06-19T11:44:26Z", "large_blob": "drop-me"},
			runtimeStateMetadataKey:             map[string]any{"updated_at": "2026-05-20T00:00:00Z"},
		},
	}
	now := time.Now()
	auth.recordRecentRequest(now, true)
	auth.recordRecentRequest(now, false)

	cloned := auth.CloneForManagementSummary()
	if cloned == nil {
		t.Fatal("CloneForManagementSummary returned nil")
	}
	if cloned.StatusMessage != "quota cached" || cloned.Success != 11 || cloned.Failed != 2 {
		t.Fatalf("management summary lost runtime counters: %#v", cloned)
	}
	var recentSuccess, recentFailed int64
	for _, bucket := range cloned.RecentRequestsSnapshot(now) {
		recentSuccess += bucket.Success
		recentFailed += bucket.Failed
	}
	if recentSuccess != 1 || recentFailed != 1 {
		t.Fatalf("management summary lost recent requests: success=%d failed=%d", recentSuccess, recentFailed)
	}
	if cloned.Attributes["path"] != "/tmp/codex.json" || cloned.Attributes["api_key"] != "sk-test" || cloned.Attributes["header:User-Agent"] == "" {
		t.Fatalf("management summary attributes = %#v", cloned.Attributes)
	}
	for _, key := range []string{
		"originator",
		"header:Originator",
		"header:X-Codex-Beta-Features",
		"header:X-Codex-Installation-Id",
		"header:x-responsesapi-include-timing-metrics",
	} {
		if cloned.Attributes[key] == "" {
			t.Fatalf("management summary dropped %s attribute: %#v", key, cloned.Attributes)
		}
	}
	if _, ok := cloned.Attributes["base_url"]; ok {
		t.Fatalf("management summary kept base_url: %#v", cloned.Attributes)
	}
	if _, ok := cloned.Attributes["header:X-Test"]; ok {
		t.Fatalf("management summary kept unrelated header: %#v", cloned.Attributes)
	}
	for _, key := range []string{"access_token", "id_token", "account_id"} {
		if _, ok := cloned.Metadata[key]; ok {
			t.Fatalf("management summary kept %s: %#v", key, cloned.Metadata)
		}
	}
	if cloned.Metadata["refresh_token"] != "refresh-token" || cloned.Metadata["plan_type"] != "plus" {
		t.Fatalf("management summary metadata = %#v", cloned.Metadata)
	}
	for _, key := range []string{"originator", "beta_features", "installation_id", "include_timing_metrics"} {
		if _, ok := cloned.Metadata[key]; !ok {
			t.Fatalf("management summary dropped %s metadata: %#v", key, cloned.Metadata)
		}
	}
	if nested, ok := cloned.Metadata["token"].(map[string]any); !ok || nested["refresh_token"] != "nested-refresh" || nested["access_token"] != nil {
		t.Fatalf("management summary nested token = %#v", cloned.Metadata["token"])
	}
	if nested, ok := cloned.Metadata["subscription"].(map[string]any); !ok || nested["current_period_end"] == nil || nested["large_blob"] != nil {
		t.Fatalf("management summary nested subscription = %#v", cloned.Metadata["subscription"])
	}

	cloned.Metadata["email"] = "mutated@example.com"
	if auth.Metadata["email"] != "x@example.com" {
		t.Fatal("CloneForManagementSummary should copy metadata map")
	}
	clonedToken := cloned.Metadata["token"].(map[string]any)
	clonedToken["refresh_token"] = "mutated"
	originalToken := auth.Metadata["token"].(map[string]any)
	if originalToken["refresh_token"] != "nested-refresh" {
		t.Fatal("CloneForManagementSummary should copy nested token map")
	}
}

func BenchmarkCloneAuthForExecutionCodex(b *testing.B) {
	auth := &Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "k", "base_url": "https://chatgpt.com/backend-api/codex"},
		Metadata: map[string]any{
			"account_id":   "acct",
			"originator":   "cli",
			"installation": "install",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {Status: StatusActive},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cloned := cloneAuthForExecution("codex", auth)
		if cloned == nil {
			b.Fatal("cloneAuthForExecution returned nil")
		}
	}
}

func BenchmarkCloneAuthForScheduler(b *testing.B) {
	auth := &Auth{
		ID:          "sched-auth",
		Provider:    "claude",
		Status:      StatusError,
		Unavailable: true,
		Attributes: map[string]string{
			"priority":   "10",
			"websockets": "true",
			"api_key":    "secret",
		},
		Metadata: map[string]any{
			"websockets": true,
			"email":      "x@example.com",
		},
		ModelStates: map[string]*ModelState{
			"claude-3-5-sonnet": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: parseMustTime(b, "2026-04-26T12:05:00Z"),
				Quota:          QuotaState{Exceeded: true, BackoffLevel: 2},
				LastError:      &Error{Message: "boom"},
			},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cloned := auth.CloneForScheduler()
		if cloned == nil {
			b.Fatal("CloneForScheduler returned nil")
		}
	}
}

type timeParserTB interface {
	Fatalf(string, ...any)
	Helper()
}

func parseMustTime(tb timeParserTB, raw string) time.Time {
	tb.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		tb.Fatalf("time.Parse(%q) error = %v", raw, err)
	}
	return parsed
}

func BenchmarkCloneAuthForExecutionProvider(b *testing.B) {
	auth := &Auth{
		ID:         "compat-auth",
		Provider:   "openai-compatibility",
		Attributes: map[string]string{"priority": "10", "proxy_url": "http://127.0.0.1:7890"},
		Metadata: map[string]any{
			"email":      "x@example.com",
			"websockets": true,
		},
		ModelStates: map[string]*ModelState{
			"gpt-5": {Status: StatusActive},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cloned := cloneAuthForExecution("openai-compatibility", auth)
		if cloned == nil {
			b.Fatal("cloneAuthForExecution returned nil")
		}
	}
}

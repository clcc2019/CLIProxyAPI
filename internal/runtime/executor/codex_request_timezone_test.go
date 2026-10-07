package executor

import (
	"encoding/json"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexRequestTimezoneTestAuth(timezone string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			cliproxyauth.AuthFileOpenAIRequestTimezoneKey: timezone,
		},
	}
}

func TestNormalizeCodexRequestTimezoneRewritesScopedFields(t *testing.T) {
	body := []byte(`{
  "input": [
    {"role":"developer","content":[{"type":"input_text","text":"<environment_context><timezone>Europe/London</timezone></environment_context>"}]},
    {"role":"user","content":[
      {"type":"input_text","text":"<environment_context>\n  <current_date>2001-01-01</current_date>\n  <timezone>Europe/London</timezone>\n  <cwd>/workspace</cwd>\n</environment_context>"},
      {"type":"input_text","text":"ordinary Europe/London"}
    ]},
    {"role":"assistant","content":"<environment_context><timezone>Europe/London</timezone></environment_context>"}
  ],
  "tools": [
    {"type":"web_search","user_location":{"country":"US","timezone":"Europe/London"}},
    {"type":"function","user_location":{"timezone":"Europe/London"}}
  ]
}`)
	auth := codexRequestTimezoneTestAuth("Asia/Singapore")

	got := normalizeCodexRequestTimezone(body, auth)
	if !json.Valid(got) {
		t.Fatalf("normalized body is invalid JSON: %s", got)
	}
	if value := gjson.GetBytes(got, "input.1.content.0.text").String(); value != "<environment_context>\n  <current_date>2001-01-01</current_date>\n  <timezone>Asia/Singapore</timezone>\n  <cwd>/workspace</cwd>\n</environment_context>" {
		t.Fatalf("environment context = %q", value)
	}
	if value := gjson.GetBytes(got, "input.1.content.1.text").String(); value != "ordinary Europe/London" {
		t.Fatalf("ordinary user text changed: %q", value)
	}
	if value := gjson.GetBytes(got, "input.0.content.0.text").String(); value != gjson.GetBytes(body, "input.0.content.0.text").String() {
		t.Fatalf("developer context changed: %q", value)
	}
	if value := gjson.GetBytes(got, "input.2.content").String(); value != gjson.GetBytes(body, "input.2.content").String() {
		t.Fatalf("assistant context changed: %q", value)
	}
	if value := gjson.GetBytes(got, "tools.0.user_location.timezone").String(); value != "Asia/Singapore" {
		t.Fatalf("web search timezone = %q", value)
	}
	if value := gjson.GetBytes(got, "tools.0.user_location.country").String(); value != "US" {
		t.Fatalf("web search country changed: %q", value)
	}
	if value := gjson.GetBytes(got, "tools.1.user_location.timezone").String(); value != "Europe/London" {
		t.Fatalf("non-web-search tool timezone changed: %q", value)
	}
	if again := normalizeCodexRequestTimezone(got, auth); string(again) != string(got) {
		t.Fatalf("normalization is not idempotent:\nfirst: %s\nagain: %s", got, again)
	}
}

func TestNormalizeCodexRequestTimezoneLeavesAmbiguousEnvironmentTextUntouched(t *testing.T) {
	cases := []string{
		"Example: <environment_context><timezone>Europe/London</timezone></environment_context>",
		"<environment_context><timezone /></environment_context>",
		"<environment_context><timezone>Europe/London</timezone><timezone>Asia/Tokyo</timezone></environment_context>",
		"<environment_context><timezone><value>Europe/London</value></timezone></environment_context>",
	}
	auth := codexRequestTimezoneTestAuth("Asia/Singapore")
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"input": []any{map[string]any{
					"role":    "user",
					"content": text,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := normalizeCodexRequestTimezone(body, auth)
			if string(got) != string(body) {
				t.Fatalf("ambiguous environment text was changed:\nwant: %s\n got: %s", body, got)
			}
		})
	}
}

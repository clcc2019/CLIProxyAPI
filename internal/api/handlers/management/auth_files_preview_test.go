package management

import (
	"encoding/json"
	"testing"
)

func TestAuthFilePreviewJSONRecognizesCodexClientProfileOnlyFile(t *testing.T) {
	input := []byte(`{
  "installation_id": "03cc2394-b574-43b1-bda8-bc368436d9a3",
  "user_agent": "node"
}`)

	data, err := authFilePreviewJSON(input)
	if err != nil {
		t.Fatalf("authFilePreviewJSON() error = %v", err)
	}

	var preview map[string]any
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatalf("Unmarshal preview: %v", err)
	}
	if got := preview["type"]; got != "codex" {
		t.Fatalf("preview[type] = %#v, want codex", got)
	}
	if got := preview["user_agent"]; got != "node" {
		t.Fatalf("preview[user_agent] = %#v, want node", got)
	}
	if got := preview["installation_id"]; got != "03cc2394-b574-43b1-bda8-bc368436d9a3" {
		t.Fatalf("preview[installation_id] = %#v, want source installation id", got)
	}
}

func TestAuthFilePreviewJSONReadsNestedCodexClientProfile(t *testing.T) {
	input := []byte(`{
  "client_profile": {
    "installation_id": "install-1",
    "user_agent": "node"
  }
}`)

	data, err := authFilePreviewJSON(input)
	if err != nil {
		t.Fatalf("authFilePreviewJSON() error = %v", err)
	}

	var preview map[string]any
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatalf("Unmarshal preview: %v", err)
	}
	if got := preview["type"]; got != "codex" {
		t.Fatalf("preview[type] = %#v, want codex", got)
	}
	if got := preview["user_agent"]; got != "node" {
		t.Fatalf("preview[user_agent] = %#v, want node", got)
	}
	if got := preview["installation_id"]; got != "install-1" {
		t.Fatalf("preview[installation_id] = %#v, want nested installation id", got)
	}
}

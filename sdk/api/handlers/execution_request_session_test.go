package handlers

import "testing"

func TestExtractParentSessionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"metadata", `{"metadata":{"parent_session_id":"parent-a"}}`, "parent-a"},
		{"nested client metadata", `{"client_metadata":{"parentSessionId":"parent-b"}}`, "parent-b"},
		{"input item", `{"input":[{"metadata":{"parent_session":"parent-c"}}]}`, "parent-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractParentSessionID([]byte(tc.body)); got != tc.want {
				t.Fatalf("extractParentSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}

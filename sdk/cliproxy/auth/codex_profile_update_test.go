package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestManagerMergesConcurrentCodexProfileVersionsMonotonically(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	base := codexProfileTestAuth("profile-race", "1.0.0")
	if _, err := manager.Register(context.Background(), base); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	var wg sync.WaitGroup
	for minor := 1; minor <= 12; minor++ {
		version := fmt.Sprintf("1.%d.0", minor)
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := codexProfileTestAuth(base.ID, version)
			manager.handleExecutionAuthUpdate(withExecutionAuthProfileUpdate(context.Background()), candidate)
		}()
	}
	wg.Wait()

	got, ok := manager.GetByID(base.ID)
	if !ok || got == nil {
		t.Fatal("updated auth not found")
	}
	if userAgent := codexProfileUserAgent(got); userAgent != "codex_vscode/1.12.0" {
		t.Fatalf("User-Agent = %q, want highest concurrent version", userAgent)
	}
	if version := codexProfileAttributeHeader(got.Attributes, "Version"); version != "1.12.0" {
		t.Fatalf("Version = %q, want highest concurrent version", version)
	}
}

func TestMergeCodexExecutionAuthProfileRejectsDifferentClientProduct(t *testing.T) {
	existing := codexProfileTestAuth("profile-product", "2.0.0")
	candidate := codexProfileTestAuth("profile-product", "9.0.0")
	setCodexProfileUserAgent(candidate, "codex_desktop/9.0.0")

	merged, changed := mergeCodexExecutionAuthProfile(existing, candidate)

	if changed {
		t.Fatal("different client product unexpectedly changed fixed profile")
	}
	if got := codexProfileUserAgent(merged); got != "codex_vscode/2.0.0" {
		t.Fatalf("User-Agent = %q, want fixed client product", got)
	}
}

func TestMergeCodexExecutionAuthProfileAcceptsFirstPinnedProfile(t *testing.T) {
	existing := &Auth{
		ID:         "profile-first-pin",
		Provider:   "codex",
		Status:     StatusActive,
		Metadata:   map[string]any{"access_token": "token"},
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	candidate := codexProfileTestAuth(existing.ID, "2.0.0")
	candidate.Metadata["access_token"] = "token"
	candidate.Attributes["header:X-Codex-Beta-Features"] = "feature-a"
	candidate.Metadata["headers"].(map[string]any)["X-Codex-Beta-Features"] = "feature-a"

	merged, changed := mergeCodexExecutionAuthProfile(existing, candidate)

	if !changed {
		t.Fatal("first pinned profile was not accepted")
	}
	if !codexAuthProfilePinned(merged) {
		t.Fatal("merged profile is not pinned")
	}
	if got := codexProfileAttributeHeader(merged.Attributes, "X-Codex-Beta-Features"); got != "feature-a" {
		t.Fatalf("X-Codex-Beta-Features = %q, want feature-a", got)
	}
	if got := merged.Metadata["access_token"]; got != "token" {
		t.Fatalf("access_token = %v, want preserved manager value", got)
	}
}

func codexProfileTestAuth(id string, version string) *Auth {
	userAgent := "codex_vscode/" + version
	return &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"codex_client_profile_pinned": true,
			"user_agent":                  userAgent,
			"headers": map[string]any{
				"User-Agent": userAgent,
				"Version":    version,
			},
		},
		Attributes: map[string]string{
			"header:User-Agent": userAgent,
			"header:Version":    version,
		},
	}
}

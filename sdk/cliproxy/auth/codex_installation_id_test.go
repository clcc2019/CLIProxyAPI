package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type codexInstallationIDTestStore struct {
	items []*Auth
	err   error
}

func (s *codexInstallationIDTestStore) List(context.Context) ([]*Auth, error) {
	return s.items, s.err
}

func (*codexInstallationIDTestStore) Save(context.Context, *Auth) (string, error) {
	return "", nil
}

func (*codexInstallationIDTestStore) Delete(context.Context, string) error { return nil }

func TestPrepareCodexInstallationIDForSaveGeneratesStableUUID(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	first := PrepareCodexInstallationIDForSave(auth, nil)
	second := PrepareCodexInstallationIDForSave(auth, nil)

	if first == "" || second != first {
		t.Fatalf("installation IDs = %q then %q, want one stable value", first, second)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("installation ID = %q, want UUID: %v", first, err)
	}
	if got, _ := auth.Metadata[AuthFileCodexInstallationIDKey].(string); got != first {
		t.Fatalf("metadata installation_id = %q, want %q", got, first)
	}
	if got := auth.Attributes["header:"+AuthFileCodexInstallationIDHeader]; got != first {
		t.Fatalf("installation header = %q, want %q", got, first)
	}
}

func TestPrepareCodexInstallationIDForSavePreservesExistingUnlessExplicit(t *testing.T) {
	existing := []byte(`{
		"type": "codex",
		"client_profile": {"installation_id": "installation-existing"}
	}`)

	t.Run("generated login ID is replaced", func(t *testing.T) {
		auth := &Auth{
			Provider: "codex",
			Metadata: map[string]any{AuthFileCodexInstallationIDKey: "installation-new"},
		}
		if got := PrepareCodexInstallationIDForSave(auth, existing); got != "installation-existing" {
			t.Fatalf("installation ID = %q, want existing value", got)
		}
	})

	t.Run("explicit login ID wins", func(t *testing.T) {
		auth := &Auth{
			Provider: "codex",
			Metadata: map[string]any{AuthFileCodexInstallationIDKey: "installation-explicit"},
		}
		MarkCodexInstallationIDExplicit(auth, true)
		if got := PrepareCodexInstallationIDForSave(auth, existing); got != "installation-explicit" {
			t.Fatalf("installation ID = %q, want explicit value", got)
		}
	})
}

func TestReuseCodexInstallationIDMatchesStableCredentialIdentity(t *testing.T) {
	existing := []*Auth{
		{
			ID:       "codex-old-plan.json",
			Provider: "codex",
			Metadata: map[string]any{
				"account_id":      "account-1",
				"email":           "user@example.com",
				"installation_id": "installation-existing",
			},
		},
	}
	target := &Auth{
		ID:       "codex-new-plan.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_id":      "account-1",
			"email":           "user@example.com",
			"installation_id": "installation-generated",
		},
	}

	if err := ReuseCodexInstallationID(context.Background(), &codexInstallationIDTestStore{items: existing}, target); err != nil {
		t.Fatalf("ReuseCodexInstallationID() error = %v", err)
	}
	if got := CodexInstallationID(target); got != "installation-existing" {
		t.Fatalf("installation ID = %q, want account-matched existing value", got)
	}
}

func TestReuseCodexInstallationIDDoesNotJoinDifferentKnownAccountsByEmail(t *testing.T) {
	existing := []*Auth{
		{
			ID:       "codex-account-1.json",
			Provider: "codex",
			Metadata: map[string]any{
				"account_id":      "account-1",
				"email":           "shared@example.com",
				"installation_id": "installation-existing",
			},
		},
	}
	target := &Auth{
		ID:       "codex-account-2.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_id":      "account-2",
			"email":           "shared@example.com",
			"installation_id": "installation-generated",
		},
	}

	if err := ReuseCodexInstallationID(context.Background(), &codexInstallationIDTestStore{items: existing}, target); err != nil {
		t.Fatalf("ReuseCodexInstallationID() error = %v", err)
	}
	if got := CodexInstallationID(target); got != "installation-generated" {
		t.Fatalf("installation ID = %q, want target account value", got)
	}
}

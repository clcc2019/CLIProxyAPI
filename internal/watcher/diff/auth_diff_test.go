package diff

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildAuthChangeDetailsUsesEffectiveDisabledState(t *testing.T) {
	oldAuth := &coreauth.Auth{ID: "auth", Status: coreauth.StatusActive}
	newAuth := &coreauth.Auth{ID: "auth", Status: coreauth.StatusDisabled}

	changes := BuildAuthChangeDetails(oldAuth, newAuth)
	if len(changes) != 1 || changes[0] != "disabled: false -> true" {
		t.Fatalf("changes = %#v, want effective disabled transition", changes)
	}
}

package management

import (
	"context"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSyncCodexUsageQuotaCooldownClearsRecoveredAuthCooldown(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "codex-quota-recovered", Provider: "codex"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkAuthQuotaCooldown(context.Background(), auth.ID, time.Now().Add(time.Hour))

	handler := &Handler{}
	handler.SetAuthManager(manager)
	handler.syncCodexUsageQuotaCooldown(context.Background(), auth, gin.H{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 42.0},
			"secondary_window": map[string]any{"used_percent": 13.0},
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Quota.Exceeded || updated.Unavailable || updated.StatusMessage != "" || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("quota cooldown was not cleared after a usage response with headroom: %#v", updated)
	}
}

func TestSyncCodexUsageQuotaCooldownKeepsCooldownForIncompleteUsage(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "codex-quota-unknown", Provider: "codex"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkAuthQuotaCooldown(context.Background(), auth.ID, time.Now().Add(time.Hour))

	handler := &Handler{}
	handler.SetAuthManager(manager)
	handler.syncCodexUsageQuotaCooldown(context.Background(), auth, gin.H{
		"rate_limit": map[string]any{
			"primary_window": map[string]any{"reset_at": time.Now().Add(time.Hour).Unix()},
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Quota.Exceeded || !updated.Unavailable {
		t.Fatalf("incomplete usage response unexpectedly cleared cooldown: %#v", updated)
	}
}

package auth

import (
	"context"
	"testing"
	"time"
)

func TestManagerClearAuthQuotaCooldownFromUsageKeepsModelCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	recoverAt := time.Now().Add(time.Hour)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:             "codex-auth-usage-clear",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: recoverAt,
			AuthScope:     true,
		},
		ModelStates: map[string]*ModelState{
			"model-with-own-cooldown": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	if !manager.ClearAuthQuotaCooldownFromUsage(context.Background(), "codex-auth-usage-clear") {
		t.Fatal("ClearAuthQuotaCooldownFromUsage() = false, want true")
	}

	updated, ok := manager.GetByID("codex-auth-usage-clear")
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	model := updated.ModelStates["model-with-own-cooldown"]
	if model == nil || !model.Quota.Exceeded || !model.Unavailable {
		t.Fatalf("model-specific quota cooldown was cleared by auth usage refresh: %#v", model)
	}
}

package auth

import "testing"

func TestCredentialWeightIsPreservedForSchedulerManagementAndRefresh(t *testing.T) {
	auth := &Auth{Attributes: map[string]string{AttributeWeight: "5", "api_key": "secret"}}
	if got := cloneAuthAttributesForScheduler(auth.Attributes)[AttributeWeight]; got != "5" {
		t.Fatalf("scheduler weight = %q, want 5", got)
	}
	if got := auth.CloneForManagementSummary().Attributes[AttributeWeight]; got != "5" {
		t.Fatalf("management weight = %q, want 5", got)
	}

	existing := &Auth{Attributes: map[string]string{AttributeWeight: "5"}}
	updated := &Auth{}
	preserveEditableAuthFileOptions(existing, updated)
	if got := updated.Attributes[AttributeWeight]; got != "5" {
		t.Fatalf("refreshed weight = %q, want 5", got)
	}
}

func TestApplyAuthWeightMetadata(t *testing.T) {
	auth := &Auth{}
	if err := ApplyAuthWeightMetadata(auth, map[string]any{AttributeWeight: float64(-4)}); err != nil {
		t.Fatalf("ApplyAuthWeightMetadata() error = %v", err)
	}
	if got := auth.Attributes[AttributeWeight]; got != "0" {
		t.Fatalf("weight = %q, want 0", got)
	}
	if err := ApplyAuthWeightMetadata(auth, map[string]any{AttributeWeight: 1.5}); err == nil {
		t.Fatal("ApplyAuthWeightMetadata() error = nil, want invalid-weight error")
	}
}

package auth

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagerListByProviderFiltersBeforeCloning(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for _, auth := range []*Auth{
		{ID: "codex-a", Provider: "codex", Metadata: map[string]any{"access_token": "token-a"}},
		{ID: "codex-b", Provider: " CODEX ", Metadata: map[string]any{"access_token": "token-b"}},
		{ID: "claude-a", Provider: "claude", Metadata: map[string]any{"access_token": "token-c"}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}

	got := manager.ListByProvider(" CoDeX ")
	if len(got) != 2 {
		t.Fatalf("ListByProvider() returned %d auths, want 2", len(got))
	}
	for _, auth := range got {
		if auth == nil || auth.Provider == "claude" {
			t.Fatalf("ListByProvider() returned unexpected auth: %#v", auth)
		}
	}
	got[0].Metadata["access_token"] = "mutated"
	stored, ok := manager.GetByID(got[0].ID)
	if !ok || stored == nil {
		t.Fatalf("GetByID(%q) failed", got[0].ID)
	}
	if stored.Metadata["access_token"] == "mutated" {
		t.Fatal("ListByProvider() result aliases manager metadata")
	}
}

func TestManagerListByProviderHandlesEmptyInput(t *testing.T) {
	var nilManager *Manager
	if got := nilManager.ListByProvider("codex"); got != nil {
		t.Fatalf("nil manager result = %#v, want nil", got)
	}
	manager := NewManager(nil, nil, nil)
	if got := manager.ListByProvider("  "); got != nil {
		t.Fatalf("empty provider result = %#v, want nil", got)
	}
}

func TestManagerSingleAuthLookupsFilterBeforeCloning(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for _, auth := range []*Auth{
		{ID: "first", Provider: "codex", FileName: "/accounts/first.json", Metadata: map[string]any{"access_token": "first-token"}},
		{ID: "second", Provider: "codex", FileName: "/accounts/Second.JSON", Metadata: map[string]any{"access_token": "second-token"}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}

	exact, ok := manager.GetByFileName("/accounts/first.json")
	if !ok || exact == nil || exact.ID != "first" {
		t.Fatalf("GetByFileName() = %#v, %t; want first auth", exact, ok)
	}
	if _, ok = manager.GetByFileName("first.json"); ok {
		t.Fatal("GetByFileName() unexpectedly performed base-name matching")
	}
	relaxed, ok := manager.GetByName("second.json")
	if !ok || relaxed == nil || relaxed.ID != "second" {
		t.Fatalf("GetByName() = %#v, %t; want second auth", relaxed, ok)
	}
	relaxed.Metadata["access_token"] = "mutated"
	stored, ok := manager.GetByID("second")
	if !ok || stored.Metadata["access_token"] != "second-token" {
		t.Fatal("single-auth lookup result aliases manager metadata")
	}

	var nilManager *Manager
	if auth, ok := nilManager.GetByName("first"); ok || auth != nil {
		t.Fatalf("nil manager GetByName() = %#v, %t", auth, ok)
	}
	if auth, ok := manager.GetByFileName("  "); ok || auth != nil {
		t.Fatalf("empty GetByFileName() = %#v, %t", auth, ok)
	}
}

func TestManagerIndexLookupsAvoidFullAuthClones(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auths := []*Auth{
		{ID: "first", Provider: "codex", Metadata: map[string]any{"access_token": "first-token"}},
		{ID: "second", Provider: "claude", Metadata: map[string]any{"access_token": "second-token"}},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}

	wantIndex := auths[1].EnsureIndex()
	got, ok := manager.GetByIndex(wantIndex)
	if !ok || got == nil || got.ID != "second" {
		t.Fatalf("GetByIndex(%q) = %#v, %t", wantIndex, got, ok)
	}
	got.Metadata["access_token"] = "mutated"
	stored, ok := manager.GetByID("second")
	if !ok || stored.Metadata["access_token"] != "second-token" {
		t.Fatal("GetByIndex() result aliases manager metadata")
	}
	legacy := &Auth{ID: "legacy", Provider: "codex"}
	wantLegacyIndex := legacy.Clone().EnsureIndex()
	manager.mu.Lock()
	manager.auths[legacy.ID] = legacy
	manager.mu.Unlock()
	if gotLegacy, found := manager.GetByIndex(wantLegacyIndex); !found || gotLegacy == nil || gotLegacy.ID != legacy.ID {
		t.Fatalf("GetByIndex() legacy fallback = %#v, %t", gotLegacy, found)
	}
	if legacy.Index != "" {
		t.Fatal("GetByIndex() legacy fallback mutated shared auth")
	}

	indexes := manager.AuthIndexesByID()
	if len(indexes) != len(auths)+1 || indexes["first"] != auths[0].EnsureIndex() || indexes["second"] != wantIndex || indexes["legacy"] != wantLegacyIndex {
		t.Fatalf("AuthIndexesByID() = %#v", indexes)
	}
	indexes["second"] = "mutated"
	if again := manager.AuthIndexesByID(); again["second"] != wantIndex {
		t.Fatal("AuthIndexesByID() returned manager-owned map")
	}

	if auth, ok := manager.GetByIndex("  "); ok || auth != nil {
		t.Fatalf("empty GetByIndex() = %#v, %t", auth, ok)
	}
	var nilManager *Manager
	if indexes := nilManager.AuthIndexesByID(); indexes == nil || len(indexes) != 0 {
		t.Fatalf("nil manager AuthIndexesByID() = %#v", indexes)
	}
}

func TestManagerAPIKeyUsageSnapshotsContainOnlyEligibleAuths(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auths := []*Auth{
		{
			ID:       "api-key",
			Provider: "codex",
			Attributes: map[string]string{
				"api_key":  " key ",
				"base-url": " https://example.com ",
			},
			Metadata: map[string]any{"access_token": "must-not-be-copied"},
		},
		{
			ID:         "oauth",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "ignored-key"},
			Metadata:   map[string]any{"email": "oauth@example.com"},
		},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}
	manager.MarkResult(context.Background(), Result{AuthID: "api-key", Provider: "codex", Success: true})

	snapshots := manager.APIKeyUsageSnapshots(time.Now())
	if len(snapshots) != 1 {
		t.Fatalf("APIKeyUsageSnapshots() returned %d entries, want 1", len(snapshots))
	}
	got := snapshots[0]
	if got.Provider != "codex" || got.APIKey != "key" || got.BaseURL != "https://example.com" || got.Success != 1 || got.Failed != 0 {
		t.Fatalf("APIKeyUsageSnapshots() entry = %#v", got)
	}
	if len(got.RecentRequests) != recentRequestBucketCount {
		t.Fatalf("recent request buckets = %d, want %d", len(got.RecentRequests), recentRequestBucketCount)
	}
	var nilManager *Manager
	if got := nilManager.APIKeyUsageSnapshots(time.Now()); got != nil {
		t.Fatalf("nil manager snapshots = %#v, want nil", got)
	}
}

func TestManagerAuthLookupSnapshotsContainOnlyScalarLookupFields(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "lookup-auth",
		Provider: "codex",
		FileName: "lookup.json",
		Attributes: map[string]string{
			"path":    "/auth/lookup.json",
			"source":  "/legacy/lookup.json",
			"api_key": "must-not-be-exposed",
		},
		Metadata: map[string]any{"access_token": "must-not-be-exposed"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	snapshots := manager.AuthLookupSnapshots()
	if len(snapshots) != 1 {
		t.Fatalf("AuthLookupSnapshots() returned %d entries, want 1", len(snapshots))
	}
	got := snapshots[0]
	if got.ID != auth.ID || got.Index != auth.EnsureIndex() || got.FileName != auth.FileName || got.Path != auth.Attributes["path"] || got.Source != auth.Attributes["source"] {
		t.Fatalf("AuthLookupSnapshots() entry = %#v", got)
	}

	var nilManager *Manager
	if got := nilManager.AuthLookupSnapshots(); got != nil {
		t.Fatalf("nil manager snapshots = %#v, want nil", got)
	}
}

func TestManagerManagementSummaryStopsForCanceledContext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := manager.ListManagementSummaryWithoutRecentRequestsContext(ctx); got != nil {
		t.Fatalf("canceled management summary = %#v, want nil", got)
	}
}

func BenchmarkManagerListByProvider(b *testing.B) {
	manager := NewManager(nil, nil, nil)
	const codexCount = 100
	for index := 0; index < 1_000; index++ {
		provider := "claude"
		if index < codexCount {
			provider = "codex"
		}
		auth := &Auth{
			ID:       provider + "-" + strconv.Itoa(index),
			Provider: provider,
			Metadata: map[string]any{
				"access_token": "representative-token-value",
				"account_id":   "account-" + strconv.Itoa(index),
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}

	b.Run("filter_before_clone", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if got := manager.ListByProvider("codex"); len(got) != codexCount {
				b.Fatalf("ListByProvider() returned %d auths, want %d", len(got), codexCount)
			}
		}
	})
	b.Run("clone_all_then_filter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			count := 0
			for _, auth := range manager.List() {
				if auth != nil && auth.Provider == "codex" {
					count++
				}
			}
			if count != codexCount {
				b.Fatalf("filtered List() returned %d auths, want %d", count, codexCount)
			}
		}
	})
}

func BenchmarkManagerGetByName(b *testing.B) {
	manager := NewManager(nil, nil, nil)
	const authCount = 1_000
	for index := 0; index < authCount; index++ {
		auth := &Auth{
			ID:       "codex-" + strconv.Itoa(index),
			Provider: "codex",
			FileName: "/representative/accounts/account-" + strconv.Itoa(index) + ".json",
			Metadata: map[string]any{
				"access_token": "representative-token-value",
				"account_id":   "account-" + strconv.Itoa(index),
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}
	const target = "account-999.json"
	b.Run("filter_before_clone", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			auth, ok := manager.GetByName(target)
			if !ok || auth.ID != "codex-999" {
				b.Fatalf("GetByName(%q) = %#v, %t", target, auth, ok)
			}
		}
	})
	b.Run("clone_all_then_filter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var found *Auth
			for _, auth := range manager.List() {
				if auth != nil && filepath.Base(auth.FileName) == target {
					found = auth
					break
				}
			}
			if found == nil || found.ID != "codex-999" {
				b.Fatalf("List() lookup for %q failed", target)
			}
		}
	})
}

func BenchmarkManagerAuthIndexQueries(b *testing.B) {
	manager := NewManager(nil, nil, nil)
	const authCount = 1_000
	var targetIndex string
	for index := 0; index < authCount; index++ {
		auth := &Auth{
			ID:       "codex-" + strconv.Itoa(index),
			Provider: "codex",
			Metadata: map[string]any{
				"access_token": "representative-token-value",
				"account_id":   "account-" + strconv.Itoa(index),
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
		if index == authCount-1 {
			targetIndex = auth.EnsureIndex()
		}
	}

	b.Run("get_filter_before_clone", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			auth, ok := manager.GetByIndex(targetIndex)
			if !ok || auth.ID != "codex-999" {
				b.Fatalf("GetByIndex(%q) = %#v, %t", targetIndex, auth, ok)
			}
		}
	})
	b.Run("get_clone_all_then_filter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var found *Auth
			for _, auth := range manager.List() {
				if auth != nil && auth.EnsureIndex() == targetIndex {
					found = auth
					break
				}
			}
			if found == nil || found.ID != "codex-999" {
				b.Fatalf("List() index lookup for %q failed", targetIndex)
			}
		}
	})
	b.Run("map_without_auth_clones", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			indexes := manager.AuthIndexesByID()
			if len(indexes) != authCount || indexes["codex-999"] != targetIndex {
				b.Fatalf("AuthIndexesByID() returned invalid map")
			}
		}
	})
	b.Run("map_from_full_clones", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			indexes := make(map[string]string, authCount)
			for _, auth := range manager.List() {
				if auth != nil {
					indexes[auth.ID] = auth.EnsureIndex()
				}
			}
			if len(indexes) != authCount || indexes["codex-999"] != targetIndex {
				b.Fatalf("List() index map returned invalid map")
			}
		}
	})
}

func BenchmarkManagerAPIKeyUsageSnapshots(b *testing.B) {
	manager := NewManager(nil, nil, nil)
	const authCount = 1_000
	largeToken := strings.Repeat("token", 256)
	for index := 0; index < authCount; index++ {
		modelStates := make(map[string]*ModelState, 8)
		for modelIndex := 0; modelIndex < 8; modelIndex++ {
			modelStates["model-"+strconv.Itoa(modelIndex)] = &ModelState{}
		}
		auth := &Auth{
			ID:       "codex-" + strconv.Itoa(index),
			Provider: "codex",
			Attributes: map[string]string{
				"api_key":  "key-" + strconv.Itoa(index),
				"base_url": "https://example.com",
			},
			Metadata: map[string]any{
				"access_token":  largeToken,
				"refresh_token": largeToken,
			},
			ModelStates: modelStates,
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}
	now := time.Now()
	b.Run("lightweight_snapshots", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			snapshots := manager.APIKeyUsageSnapshots(now)
			if len(snapshots) != authCount || snapshots[authCount-1].APIKey == "" {
				b.Fatalf("APIKeyUsageSnapshots() returned invalid result")
			}
		}
	})
	b.Run("full_clones_then_extract", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			count := 0
			for _, auth := range manager.List() {
				kind, apiKey := auth.AccountInfo()
				if strings.EqualFold(strings.TrimSpace(kind), "api_key") && strings.TrimSpace(apiKey) != "" {
					_ = auth.RecentRequestsSnapshot(now)
					count++
				}
			}
			if count != authCount {
				b.Fatalf("List() extraction returned %d auths", count)
			}
		}
	})
}

func BenchmarkManagerAuthLookupSnapshots(b *testing.B) {
	manager := NewManager(nil, nil, nil)
	const authCount = 1_000
	largeToken := strings.Repeat("token", 256)
	for index := 0; index < authCount; index++ {
		modelStates := make(map[string]*ModelState, 8)
		for modelIndex := 0; modelIndex < 8; modelIndex++ {
			modelStates["model-"+strconv.Itoa(modelIndex)] = &ModelState{}
		}
		auth := &Auth{
			ID:       "auth-" + strconv.Itoa(index),
			Provider: "codex",
			FileName: "auth-" + strconv.Itoa(index) + ".json",
			Attributes: map[string]string{
				"path":    "/auth/auth-" + strconv.Itoa(index) + ".json",
				"source":  "/legacy/auth-" + strconv.Itoa(index) + ".json",
				"api_key": "key-" + strconv.Itoa(index),
			},
			Metadata: map[string]any{
				"access_token":  largeToken,
				"refresh_token": largeToken,
			},
			ModelStates: modelStates,
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			b.Fatalf("Register(%q) error = %v", auth.ID, err)
		}
	}

	b.Run("scalar_lookup_snapshots", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			snapshots := manager.AuthLookupSnapshots()
			if len(snapshots) != authCount || snapshots[authCount-1].Index == "" {
				b.Fatalf("AuthLookupSnapshots() returned invalid result")
			}
		}
	})
	b.Run("full_clones_then_extract", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			total := 0
			for _, auth := range manager.List() {
				total += len(auth.ID) + len(auth.EnsureIndex()) + len(auth.FileName) + len(auth.Attributes["path"]) + len(auth.Attributes["source"])
			}
			if total == 0 {
				b.Fatal("List() extraction returned no lookup data")
			}
		}
	})
}

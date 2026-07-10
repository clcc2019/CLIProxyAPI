package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

func TestCodexAuthenticatorRefreshLeadIsFiveDays(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	lead := authenticator.RefreshLead()
	if lead == nil {
		t.Fatal("RefreshLead() = nil, want 5 days")
	}
	if got, want := *lead, 5*24*time.Hour; got != want {
		t.Fatalf("RefreshLead() = %s, want %s", got, want)
	}
}

func TestBuildAuthRecordPersistsConfiguredUserAgent(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	authSvc := &codex.CodexAuth{}
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			Email:        "codex@example.com",
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
		},
		LastRefresh: "2026-03-31T00:00:00Z",
	}
	opts := &LoginOptions{
		Metadata: map[string]string{
			"user_agent": misc.CodexCLIUserAgent,
		},
	}

	record, err := authenticator.buildAuthRecord(authSvc, bundle, opts)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}

	if got, _ := record.Metadata["user_agent"].(string); got != misc.CodexCLIUserAgent {
		t.Fatalf("Metadata[user_agent] = %q, want %q", got, misc.CodexCLIUserAgent)
	}
	if _, ok := record.Metadata["originator"]; ok {
		t.Fatal("Metadata[originator] should not persist default originator")
	}
	if got := record.Attributes["header:User-Agent"]; got != misc.CodexCLIUserAgent {
		t.Fatalf("Attributes[header:User-Agent] = %q, want %q", got, misc.CodexCLIUserAgent)
	}
	if _, ok := record.Attributes["header:Originator"]; ok {
		t.Fatal("Attributes[header:Originator] should not persist default originator")
	}
}

func TestBuildAuthRecordDoesNotPersistDefaultClientProfileWhenUnset(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	authSvc := &codex.CodexAuth{}
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			Email:        "codex@example.com",
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
		},
		LastRefresh: "2026-03-31T00:00:00Z",
	}

	record, err := authenticator.buildAuthRecord(authSvc, bundle, nil)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}

	if got, _ := record.Metadata["installation_id"].(string); got == "" {
		t.Fatal("Metadata[installation_id] is empty")
	} else if _, errParse := uuid.Parse(got); errParse != nil {
		t.Fatalf("Metadata[installation_id] = %q, want UUID: %v", got, errParse)
	}
	if _, ok := record.Metadata["originator"]; ok {
		t.Fatal("Metadata[originator] should not persist default originator")
	}
	if _, ok := record.Metadata["user_agent"]; ok {
		t.Fatal("Metadata[user_agent] should not persist default user agent")
	}
	if got := record.Attributes["header:X-Codex-Installation-Id"]; got == "" {
		t.Fatal("Attributes[header:X-Codex-Installation-Id] is empty")
	}
	if _, ok := record.Attributes["header:Originator"]; ok {
		t.Fatal("Attributes[header:Originator] should not persist default originator")
	}
	if _, ok := record.Attributes["header:User-Agent"]; ok {
		t.Fatal("Attributes[header:User-Agent] should not persist default user agent")
	}
}

func TestBuildAuthRecordDerivesUserAgentFromConfiguredOriginator(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	authSvc := &codex.CodexAuth{}
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			Email:        "codex@example.com",
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
		},
		LastRefresh: "2026-03-31T00:00:00Z",
	}

	record, err := authenticator.buildAuthRecord(authSvc, bundle, &LoginOptions{
		Metadata: map[string]string{
			"originator": "codex_vscode",
		},
	})
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}

	if got, _ := record.Metadata["originator"].(string); got != "codex_vscode" {
		t.Fatalf("Metadata[originator] = %q, want %q", got, "codex_vscode")
	}
	if got := record.Attributes["header:Originator"]; got != "codex_vscode" {
		t.Fatalf("Attributes[header:Originator] = %q, want %q", got, "codex_vscode")
	}
	if _, ok := record.Metadata["user_agent"]; ok {
		t.Fatal("Metadata[user_agent] should not persist derived user agent")
	}
	if _, ok := record.Attributes["header:User-Agent"]; ok {
		t.Fatal("Attributes[header:User-Agent] should not persist derived user agent")
	}
}

func TestNewCodexClientFeaturesGeneratesRandomInstallationID(t *testing.T) {
	first := NewCodexClientFeatures(nil)
	second := NewCodexClientFeatures(nil)

	if first.InstallationID == "" || second.InstallationID == "" {
		t.Fatalf("InstallationID should be populated: first=%q second=%q", first.InstallationID, second.InstallationID)
	}
	if first.InstallationID == second.InstallationID {
		t.Fatalf("InstallationID should be random per generated profile, got %q twice", first.InstallationID)
	}
	if _, err := uuid.Parse(first.InstallationID); err != nil {
		t.Fatalf("first InstallationID = %q, want UUID: %v", first.InstallationID, err)
	}
	if first.Originator != codexDefaultClientOriginator {
		t.Fatalf("Originator = %q, want %q", first.Originator, codexDefaultClientOriginator)
	}
	if first.OriginatorExplicit {
		t.Fatal("OriginatorExplicit should be false for default originator")
	}
	if !strings.HasPrefix(first.UserAgent, codexDefaultClientOriginator+"/"+codexDefaultClientVersion+" ") {
		t.Fatalf("UserAgent = %q, want Codex CLI version prefix", first.UserAgent)
	}
	if first.UserAgentExplicit {
		t.Fatal("UserAgentExplicit should be false for default user agent")
	}
}

func TestNewCodexClientFeaturesUsesMetadataOverrides(t *testing.T) {
	features := NewCodexClientFeatures(map[string]string{
		"installation_id": "install-1",
		"originator":      "codex_vscode",
		"user_agent":      "codex_vscode/1.2.3",
	})

	if features.InstallationID != "install-1" {
		t.Fatalf("InstallationID = %q, want install-1", features.InstallationID)
	}
	if features.Originator != "codex_vscode" {
		t.Fatalf("Originator = %q, want codex_vscode", features.Originator)
	}
	if features.UserAgent != "codex_vscode/1.2.3" {
		t.Fatalf("UserAgent = %q, want override", features.UserAgent)
	}
	if !features.InstallationExplicit || !features.OriginatorExplicit || !features.UserAgentExplicit {
		t.Fatalf("explicit flags = installation:%v originator:%v user_agent:%v, want all true", features.InstallationExplicit, features.OriginatorExplicit, features.UserAgentExplicit)
	}
}

func TestBuildAuthRecordPersistsPlanTypeFromTokenData(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	authSvc := &codex.CodexAuth{}
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			Email:        "codex@example.com",
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			AccountID:    "account-123",
			PlanType:     "plus",
		},
		LastRefresh: "2026-03-31T00:00:00Z",
	}

	record, err := authenticator.buildAuthRecord(authSvc, bundle, nil)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}

	if got, _ := record.Metadata["plan_type"].(string); got != "plus" {
		t.Fatalf("Metadata[plan_type] = %q, want plus", got)
	}
	if got := record.Attributes["plan_type"]; got != "plus" {
		t.Fatalf("Attributes[plan_type] = %q, want plus", got)
	}
}

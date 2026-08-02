package auth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// MarkCodexInstallationIDExplicit records whether a login caller deliberately
// selected its installation ID. The hint is runtime-only: persistence backends
// use it to distinguish an intentional rotation from the random ID generated
// for a fresh login record.
func MarkCodexInstallationIDExplicit(auth *Auth, explicit bool) {
	if auth == nil {
		return
	}
	auth.codexInstallationIDExplicit = explicit
}

// CodexInstallationID resolves the canonical installation ID carried by an
// auth record, accepting the legacy spellings used by imported profiles.
func CodexInstallationID(auth *Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{
		"header:" + AuthFileCodexInstallationIDHeader,
		"header:x-codex-installation-id",
		AuthFileCodexInstallationIDHeader,
		"x-codex-installation-id",
		AuthFileCodexInstallationIDKey,
		"installation-id",
		"installationId",
		"codex_installation_id",
	} {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return CodexInstallationIDFromMetadata(auth.Metadata)
}

// CodexInstallationIDFromMetadata resolves an installation ID from a native
// auth document or an imported client_profile/client_features object.
func CodexInstallationIDFromMetadata(metadata map[string]any) string {
	value, ok := authFileClientProfileString(
		metadata,
		AuthFileCodexInstallationIDKey,
		"installation-id",
		"installationId",
		"codex_installation_id",
		AuthFileCodexInstallationIDHeader,
		"x-codex-installation-id",
		"header:"+AuthFileCodexInstallationIDHeader,
	)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// PrepareCodexInstallationIDForSave preserves the ID from an existing auth
// file unless the new record carries an explicit override. A missing ID is
// generated once and written back through the normal persistence path.
func PrepareCodexInstallationIDForSave(auth *Auth, existingFileData []byte) string {
	if !isCodexInstallationIDRecord(auth) {
		return ""
	}

	if !auth.codexInstallationIDExplicit && len(existingFileData) > 0 {
		if metadata, err := DecodeAuthFileMetadata(existingFileData); err == nil {
			if existingID := CodexInstallationIDFromMetadata(metadata); existingID != "" {
				setCodexInstallationID(auth, existingID)
			}
		}
	}

	if installationID := CodexInstallationID(auth); installationID != "" {
		setCodexInstallationID(auth, installationID)
		return installationID
	}

	installationID := uuid.NewString()
	setCodexInstallationID(auth, installationID)
	return installationID
}

// ReuseCodexInstallationID looks across a store before a login record is
// persisted. Exact credential IDs win, followed by account ID and then email.
// The wider identity match preserves the installation ID when a plan change
// causes CodexCredentialFileName to produce a different filename.
func ReuseCodexInstallationID(ctx context.Context, store Store, auth *Auth) error {
	if !isCodexInstallationIDRecord(auth) {
		return nil
	}
	if auth.codexInstallationIDExplicit || store == nil {
		PrepareCodexInstallationIDForSave(auth, nil)
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	existing, err := store.List(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			PrepareCodexInstallationIDForSave(auth, nil)
			return nil
		}
		return fmt.Errorf("resolve existing Codex installation ID: %w", err)
	}

	if installationID := matchingCodexInstallationID(auth, existing); installationID != "" {
		setCodexInstallationID(auth, installationID)
	}
	PrepareCodexInstallationIDForSave(auth, nil)
	return nil
}

func isCodexInstallationIDRecord(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func setCodexInstallationID(auth *Auth, installationID string) {
	if auth == nil {
		return
	}
	installationID = strings.TrimSpace(installationID)
	if installationID == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[AuthFileCodexInstallationIDKey] = installationID
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["header:"+AuthFileCodexInstallationIDHeader] = installationID
}

func matchingCodexInstallationID(target *Auth, existing []*Auth) string {
	for _, candidate := range existing {
		if candidateInstallationID(candidate) == "" || !sameCodexCredentialFile(target, candidate) {
			continue
		}
		return CodexInstallationID(candidate)
	}

	targetAccountID := codexCredentialIdentityValue(target, "account_id", "accountId", "chatgpt_account_id")
	if targetAccountID != "" {
		for _, candidate := range existing {
			if candidateInstallationID(candidate) == "" || !isCodexInstallationIDRecord(candidate) {
				continue
			}
			candidateAccountID := codexCredentialIdentityValue(candidate, "account_id", "accountId", "chatgpt_account_id")
			if candidateAccountID == targetAccountID {
				return CodexInstallationID(candidate)
			}
		}
	}

	targetEmail := codexCredentialIdentityValue(target, "email")
	if targetEmail == "" {
		return ""
	}
	for _, candidate := range existing {
		if candidateInstallationID(candidate) == "" || !isCodexInstallationIDRecord(candidate) {
			continue
		}
		candidateAccountID := codexCredentialIdentityValue(candidate, "account_id", "accountId", "chatgpt_account_id")
		// Two known, different account IDs must never be joined just because they
		// happen to expose the same email address.
		if targetAccountID != "" && candidateAccountID != "" && targetAccountID != candidateAccountID {
			continue
		}
		if strings.EqualFold(targetEmail, codexCredentialIdentityValue(candidate, "email")) {
			return CodexInstallationID(candidate)
		}
	}
	return ""
}

func candidateInstallationID(auth *Auth) string {
	if !isCodexInstallationIDRecord(auth) {
		return ""
	}
	return CodexInstallationID(auth)
}

func sameCodexCredentialFile(left, right *Auth) bool {
	if left == nil || right == nil || !isCodexInstallationIDRecord(right) {
		return false
	}
	leftKeys := codexCredentialFileKeys(left)
	rightKeys := codexCredentialFileKeys(right)
	for _, leftKey := range leftKeys {
		for _, rightKey := range rightKeys {
			if leftKey == rightKey {
				return true
			}
		}
	}
	return false
}

func codexCredentialFileKeys(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	keys := make([]string, 0, 4)
	appendKey := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		keys = append(keys, filepath.ToSlash(value))
		if base := filepath.Base(value); base != value {
			keys = append(keys, base)
		}
	}
	appendKey(auth.ID)
	appendKey(auth.FileName)
	appendKey(auth.Attributes["path"])
	return keys
}

func codexCredentialIdentityValue(auth *Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	if value, ok := authFileMetadataFirstAnyString(auth.Metadata, keys...); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

package misc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// credentialFileMode is the permission applied to persisted credential files.
// Credentials are secrets, so they stay owner-only.
const credentialFileMode = 0o600

// credentialDirMode is the permission applied to the credential directory.
const credentialDirMode = 0o700

// WriteCredentialJSONAtomic serializes data as JSON and installs it at
// authFilePath atomically.
//
// Writing credentials in place (os.Create + encode) is not safe: os.Create
// truncates the existing file before the new contents are written, so a crash,
// a full disk, or any mid-write error leaves a truncated or empty credential
// file. That is unrecoverable for providers whose refresh tokens are
// single-use — the old token has already been consumed upstream by the time we
// persist the new one, so losing the write means losing the account until the
// user re-authenticates from scratch.
//
// Instead write a sibling temp file, fsync it so the bytes reach disk, then
// rename over the target. Rename within a directory is atomic, so a reader
// concurrent with this call sees either the complete old file or the complete
// new one, never a partial write.
//
// indent selects the JSON layout: empty for compact output, otherwise the
// per-level indent string (for example "  ").
func WriteCredentialJSONAtomic(authFilePath string, data any, indent string) error {
	dir := filepath.Dir(authFilePath)
	if err := os.MkdirAll(dir, credentialDirMode); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create the temp file in the destination directory so the rename below
	// stays within one filesystem; os.Rename cannot be atomic across mounts.
	tmp, err := os.CreateTemp(dir, filepath.Base(authFilePath)+".tmp*")
	if err != nil {
		return fmt.Errorf("failed to create temp token file: %w", err)
	}
	tmpName := tmp.Name()

	// Any failure past this point must not leave the temp file behind. On the
	// success path removeTmp is disarmed, since the file has been renamed away.
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(credentialFileMode); err != nil {
		return fmt.Errorf("failed to set token file permissions: %w", err)
	}

	encoder := json.NewEncoder(tmp)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}

	// Without the fsync the rename can be durable while the contents are not,
	// which reintroduces the empty-credential-file failure after a host crash.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("failed to sync token file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("failed to close token file: %w", err)
	}
	if err = os.Rename(tmpName, authFilePath); err != nil {
		return fmt.Errorf("failed to install token file: %w", err)
	}
	cleanup = false
	return nil
}

// SaveCredentialJSONAtomic merges metadata into source and writes the result to
// authFilePath atomically. It is the common body shared by every provider's
// SaveTokenToFile.
func SaveCredentialJSONAtomic(authFilePath string, source any, metadata map[string]any, indent string) error {
	LogSavingCredentials(authFilePath)
	data, err := MergeMetadata(source, metadata)
	if err != nil {
		return fmt.Errorf("failed to merge metadata: %w", err)
	}
	return WriteCredentialJSONAtomic(authFilePath, data, indent)
}

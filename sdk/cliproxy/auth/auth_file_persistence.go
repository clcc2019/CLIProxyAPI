package auth

import (
	"fmt"
	"strings"
)

// PrepareAuthFileMetadataForSave applies the stable auth-file metadata fields
// that every persistence backend must write before serialising credentials.
func PrepareAuthFileMetadataForSave(auth *Auth) map[string]any {
	if auth == nil {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = auth.Disabled
	CleanAuthFileMetadataBeforeSave(auth.Metadata)
	return auth.Metadata
}

// CleanAuthFileMetadataBeforeSave removes transient empty fields that should
// not be written back into auth JSON files.
func CleanAuthFileMetadataBeforeSave(metadata map[string]any) {
	if strings.EqualFold(authFilePersistenceString(metadata, "type"), "kiro") {
		RemoveEmptyKiroMetadataFields(metadata)
	}
}

// RemoveEmptyKiroMetadataFields drops optional Kiro metadata keys when their
// value is empty, keeping saved auth files compact and stable across stores.
func RemoveEmptyKiroMetadataFields(metadata map[string]any) {
	if metadata == nil {
		return
	}
	for _, key := range []string{
		"client_id",
		"clientId",
		"client_secret",
		"clientSecret",
		"client_id_hash",
		"clientIdHash",
		"email",
		"region",
		"start_url",
		"startUrl",
		"profile_arn",
		"profileArn",
		"auth_method",
		"authMethod",
		"provider",
		"machine_id",
		"machineId",
		"device_id",
		"deviceId",
	} {
		if value, ok := metadata[key]; ok && kiroMetadataValueEmpty(value) {
			delete(metadata, key)
		}
	}
}

func authFilePersistenceString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func kiroMetadataValueEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	default:
		return false
	}
}

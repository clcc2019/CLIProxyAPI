package auth

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
	return auth.Metadata
}

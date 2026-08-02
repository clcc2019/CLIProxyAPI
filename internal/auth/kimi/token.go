// Package kimi provides token storage and refresh support for existing Kimi
// credentials.
package kimi

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// KimiTokenStorage stores OAuth2 token information for Kimi API authentication.
type KimiTokenStorage struct {
	// AccessToken is the OAuth2 access token used for authenticating API requests.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token used to obtain new access tokens.
	RefreshToken string `json:"refresh_token"`
	// TokenType is the type of token, typically "Bearer".
	TokenType string `json:"token_type"`
	// Scope is the OAuth2 scope granted to the token.
	Scope string `json:"scope,omitempty"`
	// DeviceID is the OAuth device flow identifier used for Kimi requests.
	DeviceID string `json:"device_id,omitempty"`
	// Expired is the RFC3339 timestamp when the access token expires.
	Expired string `json:"expired,omitempty"`
	// Type indicates the authentication provider type, always "kimi" for this storage.
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *KimiTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// KimiTokenData holds the raw OAuth token response from Kimi.
type KimiTokenData struct {
	// AccessToken is the OAuth2 access token.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token.
	RefreshToken string `json:"refresh_token"`
	// TokenType is the type of token, typically "Bearer".
	TokenType string `json:"token_type"`
	// ExpiresAt is the Unix timestamp when the token expires.
	ExpiresAt int64 `json:"expires_at"`
	// Scope is the OAuth2 scope granted to the token.
	Scope string `json:"scope"`
}

// SaveTokenToFile serializes the Kimi token storage to a JSON file.
func (ts *KimiTokenStorage) SaveTokenToFile(authFilePath string) error {
	ts.Type = "kimi"
	return misc.SaveCredentialJSONAtomic(authFilePath, ts, ts.Metadata, "  ")
}

// IsExpired checks if the token has expired.
func (ts *KimiTokenStorage) IsExpired() bool {
	if ts.Expired == "" {
		return false // No expiry set, assume valid
	}
	t, err := time.Parse(time.RFC3339, ts.Expired)
	if err != nil {
		return true // Has expiry string but can't parse
	}
	// Consider expired if within refresh threshold
	return time.Now().Add(time.Duration(refreshThresholdSeconds) * time.Second).After(t)
}

// NeedsRefresh checks if the token should be refreshed.
func (ts *KimiTokenStorage) NeedsRefresh() bool {
	if ts.RefreshToken == "" {
		return false // Can't refresh without refresh token
	}
	return ts.IsExpired()
}

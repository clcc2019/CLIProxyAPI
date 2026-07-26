package xai

import (
	"fmt"

	authfile "github.com/router-for-me/CLIProxyAPI/v7/internal/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// TokenStorage stores xAI OAuth credentials on disk.
type TokenStorage struct {
	Type          string `json:"type"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	ExpiresIn     int    `json:"expires_in,omitempty"`
	Expire        string `json:"expired,omitempty"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Email         string `json:"email,omitempty"`
	Subject       string `json:"sub,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	AuthKind      string `json:"auth_kind,omitempty"`

	Metadata map[string]any `json:"-"`
}

// SetMetadata allows the token store to merge status fields before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes xAI credentials to a JSON auth file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	ts.Type = "xai"
	ts.AuthKind = "oauth"
	if err := misc.SaveCredentialJSONAtomic(authFilePath, ts, ts.Metadata, "  "); err != nil {
		return fmt.Errorf("xai token storage: %w", err)
	}
	return nil
}

// CredentialFileName returns the filename used for xAI credentials.
func CredentialFileName(email, subject string) string {
	return authfile.CredentialFileName(email, subject)
}

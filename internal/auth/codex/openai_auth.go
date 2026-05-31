// Package codex provides authentication and token management for OpenAI's Codex API.
// It handles the OAuth2 flow, including generating authorization URLs, exchanging
// authorization codes for tokens, and refreshing expired tokens. The package also
// defines data structures for storing and managing Codex authentication credentials.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for OpenAI Codex
const (
	AuthURL          = "https://auth.openai.com/oauth/authorize"
	TokenURL         = "https://auth.openai.com/oauth/token"
	ClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI      = "http://localhost:1455/auth/callback"
	DefaultAuthScope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// CodexAuth handles the OpenAI OAuth2 authentication flow.
// It manages the HTTP client and provides methods for generating authorization URLs,
// exchanging authorization codes for tokens, and refreshing access tokens.
type CodexAuth struct {
	httpClient *http.Client
}

// NewCodexAuth creates a new CodexAuth service instance.
// It initializes an HTTP client with proxy settings from the provided configuration.
func NewCodexAuth(cfg *config.Config) *CodexAuth {
	return NewCodexAuthWithProxyURL(cfg, "")
}

// NewCodexAuthWithProxyURL creates a new CodexAuth service instance.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewCodexAuthWithProxyURL(cfg *config.Config, proxyURL string) *CodexAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &CodexAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{}),
	}
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE (Proof Key for Code Exchange).
// It constructs the URL with the necessary parameters, including the client ID,
// response type, redirect URI, scopes, and PKCE challenge.
func (o *CodexAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, error) {
	return o.GenerateAuthURLWithOptions(state, pkceCodes, "", "")
}

// GenerateAuthURLWithOptions creates the OAuth authorization URL with optional
// latest Codex-specific parameters such as originator and allowed_workspace_id.
func (o *CodexAuth) GenerateAuthURLWithOptions(state string, pkceCodes *PKCECodes, originator string, allowedWorkspaceID string) (string, error) {
	if pkceCodes == nil {
		return "", fmt.Errorf("PKCE codes are required")
	}

	params := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI},
		"scope":                      {DefaultAuthScope},
		"state":                      {state},
		"code_challenge":             {pkceCodes.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {resolvedCodexOAuthOriginator(originator)},
	}
	if workspaceID := strings.TrimSpace(allowedWorkspaceID); workspaceID != "" {
		params.Set("allowed_workspace_id", workspaceID)
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, nil
}

func resolvedCodexOAuthOriginator(originator string) string {
	originator = strings.TrimSpace(originator)
	if originator != "" {
		return originator
	}
	return misc.CodexCLIOriginator
}

// ExchangeCodeForTokens exchanges an authorization code for access and refresh tokens.
// It performs an HTTP POST request to the OpenAI token endpoint with the provided
// authorization code and PKCE verifier.
func (o *CodexAuth) ExchangeCodeForTokens(ctx context.Context, code string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	return o.ExchangeCodeForTokensWithRedirect(ctx, code, RedirectURI, pkceCodes)
}

// ExchangeCodeForTokensWithRedirect exchanges an authorization code for tokens using
// a caller-provided redirect URI. This supports alternate auth flows such as device
// login while preserving the existing token parsing and storage behavior.
func (o *CodexAuth) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect URI is required for token exchange")
	}

	// Prepare token exchange request
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
		"code_verifier": {pkceCodes.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := util.ReadResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}
	// log.Debugf("Token response: %s", string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse ID token: %v", err)
	}

	accountID := ""
	email := ""
	planType := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.GetUserEmail()
		planType = claims.GetPlanType()
	}
	expire := ""
	if tokenResp.AccessToken != "" {
		if accessClaims, errParseAccess := ParseJWTToken(tokenResp.AccessToken); errParseAccess == nil {
			if accountID == "" {
				accountID = accessClaims.GetAccountID()
			}
			if email == "" {
				email = accessClaims.GetUserEmail()
			}
			if planType == "" {
				planType = accessClaims.GetPlanType()
			}
			if exp, ok := accessClaims.ExpirationTime(); ok {
				expire = exp.Format(time.RFC3339)
			}
		}
	}
	if expire == "" && tokenResp.ExpiresIn > 0 {
		expire = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}

	// Create token data
	tokenData := CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		PlanType:     planType,
		Expire:       expire,
	}

	// Create auth bundle
	bundle := &CodexAuthBundle{
		TokenData:   tokenData,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// RefreshTokens refreshes an access token using a refresh token.
// This method is called when an access token has expired. It makes a request to the
// token endpoint to obtain a new set of tokens.
func (o *CodexAuth) RefreshTokens(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}

	requestBody, err := json.Marshal(codexRefreshTokenRequest{
		ClientID:     ClientID,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := util.ReadResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCodexRefreshHTTPError(resp.StatusCode, body)
	}

	var tokenResp codexRefreshTokenResponse

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	return codexTokenDataFromRefreshResponse(tokenResp, time.Now()), nil
}

type codexRefreshTokenRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type codexRefreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func codexTokenDataFromRefreshResponse(tokenResp codexRefreshTokenResponse, now time.Time) *CodexTokenData {
	data := &CodexTokenData{
		IDToken:      strings.TrimSpace(tokenResp.IDToken),
		AccessToken:  strings.TrimSpace(tokenResp.AccessToken),
		RefreshToken: strings.TrimSpace(tokenResp.RefreshToken),
	}

	for _, rawToken := range []string{data.IDToken, data.AccessToken} {
		if rawToken == "" {
			continue
		}
		claims, err := ParseJWTToken(rawToken)
		if err != nil {
			log.Warnf("Failed to parse refreshed Codex token claims: %v", err)
			continue
		}
		if data.AccountID == "" {
			data.AccountID = claims.GetAccountID()
		}
		if data.Email == "" {
			data.Email = claims.GetUserEmail()
		}
		if data.PlanType == "" {
			data.PlanType = claims.GetPlanType()
		}
	}

	if data.AccessToken != "" {
		if claims, err := ParseJWTToken(data.AccessToken); err == nil {
			if exp, ok := claims.ExpirationTime(); ok {
				data.Expire = exp.Format(time.RFC3339)
			}
		}
	}
	if data.Expire == "" && tokenResp.ExpiresIn > 0 {
		data.Expire = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	} else if data.Expire == "" && data.IDToken != "" {
		if claims, err := ParseJWTToken(data.IDToken); err == nil {
			if exp, ok := claims.ExpirationTime(); ok {
				data.Expire = exp.Format(time.RFC3339)
			}
		}
	}
	return data
}

type CodexRefreshHTTPError struct {
	StatusCodeValue int
	Code            string
	Body            string
	Permanent       bool
}

func newCodexRefreshHTTPError(statusCode int, body []byte) *CodexRefreshHTTPError {
	code := codexRefreshErrorCode(body)
	return &CodexRefreshHTTPError{
		StatusCodeValue: statusCode,
		Code:            code,
		Body:            strings.TrimSpace(string(body)),
		Permanent:       codexRefreshErrorIsPermanent(statusCode, code) || codexRefreshBodyIsPermanent(body),
	}
}

func (e *CodexRefreshHTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body != "" {
		return fmt.Sprintf("token refresh failed with status %d: %s", e.StatusCodeValue, e.Body)
	}
	return fmt.Sprintf("token refresh failed with status %d", e.StatusCodeValue)
}

func (e *CodexRefreshHTTPError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCodeValue
}

func (e *CodexRefreshHTTPError) IsPermanentAuthError() bool {
	return e != nil && e.Permanent
}

func codexRefreshErrorCode(body []byte) string {
	var payload map[string]any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if code := strings.TrimSpace(valueAsStringForRefreshError(payload["code"])); code != "" {
		return code
	}
	if raw, ok := payload["error"].(map[string]any); ok {
		if code := strings.TrimSpace(valueAsStringForRefreshError(raw["code"])); code != "" {
			return code
		}
		if typ := strings.TrimSpace(valueAsStringForRefreshError(raw["type"])); typ != "" {
			return typ
		}
	}
	if code := strings.TrimSpace(valueAsStringForRefreshError(payload["error"])); code != "" {
		return code
	}
	return ""
}

func valueAsStringForRefreshError(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		if value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
}

func codexRefreshErrorIsPermanent(statusCode int, code string) bool {
	normalized := strings.ToLower(strings.TrimSpace(code))
	switch normalized {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated", "invalid_grant", "invalid_client":
		return true
	}
	if strings.Contains(normalized, "refresh token") &&
		(strings.Contains(normalized, "already been used") || strings.Contains(normalized, "sign in again") || strings.Contains(normalized, "signing in again")) {
		return true
	}
	return statusCode == http.StatusUnauthorized
}

func codexRefreshBodyIsPermanent(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(string(body)))
	return strings.Contains(raw, "refresh token") &&
		(strings.Contains(raw, "already been used") ||
			strings.Contains(raw, "sign in again") ||
			strings.Contains(raw, "signing in again"))
}

// CreateTokenStorage creates a new CodexTokenStorage from a CodexAuthBundle.
// It populates the storage struct with token data, user information, and timestamps.
func (o *CodexAuth) CreateTokenStorage(bundle *CodexAuthBundle) *CodexTokenStorage {
	storage := &CodexTokenStorage{
		IDToken:      bundle.TokenData.IDToken,
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AccountID:    bundle.TokenData.AccountID,
		LastRefresh:  bundle.LastRefresh,
		Email:        bundle.TokenData.Email,
		PlanType:     bundle.TokenData.PlanType,
		Expire:       bundle.TokenData.Expire,
	}

	return storage
}

// RefreshTokensWithRetry refreshes tokens with a built-in retry mechanism.
// It attempts to refresh the tokens up to a specified maximum number of retries,
// with an exponential backoff strategy to handle transient network errors.
func (o *CodexAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			if errWait := refreshRetryWait(ctx, refreshRetryDelay(attempt)); errWait != nil {
				return nil, errWait
			}
		}

		tokenData, err := o.RefreshTokens(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}
		if isNonRetryableRefreshErr(err) {
			log.Warnf("Token refresh attempt %d failed with non-retryable error: %v", attempt+1, err)
			return nil, err
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

func isNonRetryableRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	if permanent, ok := err.(interface{ IsPermanentAuthError() bool }); ok && permanent.IsPermanentAuthError() {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "refresh_token_reused") ||
		strings.Contains(raw, "refresh_token_expired") ||
		strings.Contains(raw, "refresh_token_invalidated") ||
		(strings.Contains(raw, "refresh token") &&
			(strings.Contains(raw, "already been used") ||
				strings.Contains(raw, "sign in again") ||
				strings.Contains(raw, "signing in again"))) ||
		strings.Contains(raw, "invalid_grant") ||
		strings.Contains(raw, "invalid_client")
}

// UpdateTokenStorage updates an existing CodexTokenStorage with new token data.
// This is typically called after a successful token refresh to persist the new credentials.
func (o *CodexAuth) UpdateTokenStorage(storage *CodexTokenStorage, tokenData *CodexTokenData) {
	if storage == nil || tokenData == nil {
		return
	}
	if tokenData.IDToken != "" {
		storage.IDToken = tokenData.IDToken
	}
	if tokenData.AccessToken != "" {
		storage.AccessToken = tokenData.AccessToken
	}
	if tokenData.RefreshToken != "" {
		storage.RefreshToken = tokenData.RefreshToken
	}
	if tokenData.AccountID != "" {
		storage.AccountID = tokenData.AccountID
	}
	storage.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	if tokenData.Email != "" {
		storage.Email = tokenData.Email
	}
	if tokenData.PlanType != "" {
		storage.PlanType = tokenData.PlanType
	}
	if tokenData.Expire != "" {
		storage.Expire = tokenData.Expire
	}
}

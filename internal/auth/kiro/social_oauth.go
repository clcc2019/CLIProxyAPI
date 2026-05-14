package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	SocialAuthEndpoint       = "https://prod.us-east-1.auth.desktop.kiro.dev"
	SocialSigninEndpoint     = SocialAuthEndpoint
	SocialOAuthRedirectURI   = "kiro://kiro.kiroAgent/authenticate-success"
	DefaultOAuthCallbackPort = 3128
	OAuthCallbackPath        = "/oauth/callback"
	OAuthRedirectHost        = "localhost"
)

type PKCECodes struct {
	CodeVerifier  string
	CodeChallenge string
}

type SocialOAuthClient struct {
	httpClient     *http.Client
	tokenEndpoint  string
	signinEndpoint string
}

type SocialLoginURLOptions struct {
	Prompt      string
	ForceReauth bool
}

type SocialTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileArn   string `json:"profileArn"`
	ExpiresIn    int    `json:"expiresIn"`
}

func (r *SocialTokenResponse) UnmarshalJSON(data []byte) error {
	type alias SocialTokenResponse
	var wire struct {
		alias
		AccessTokenSnake  string `json:"access_token"`
		RefreshTokenSnake string `json:"refresh_token"`
		ProfileArnSnake   string `json:"profile_arn"`
		ExpiresInSnake    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = SocialTokenResponse(wire.alias)
	if strings.TrimSpace(r.AccessToken) == "" {
		r.AccessToken = strings.TrimSpace(wire.AccessTokenSnake)
	}
	if strings.TrimSpace(r.RefreshToken) == "" {
		r.RefreshToken = strings.TrimSpace(wire.RefreshTokenSnake)
	}
	if strings.TrimSpace(r.ProfileArn) == "" {
		r.ProfileArn = strings.TrimSpace(wire.ProfileArnSnake)
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = wire.ExpiresInSnake
	}
	return nil
}

func GeneratePKCECodes() (*PKCECodes, error) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	return &PKCECodes{CodeVerifier: verifier, CodeChallenge: challenge}, nil
}

func GenerateOAuthState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("kiro oauth: generate state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func OAuthRedirectURI(port int) string {
	if port <= 0 {
		port = DefaultOAuthCallbackPort
	}
	if OAuthCallbackPath == "" || OAuthCallbackPath == "/" {
		return fmt.Sprintf("http://%s:%d", OAuthRedirectHost, port)
	}
	return fmt.Sprintf("http://%s:%d%s", OAuthRedirectHost, port, OAuthCallbackPath)
}

func KiroSocialOAuthRedirectURI() string {
	return SocialOAuthRedirectURI
}

func NewSocialOAuthClient(cfg *config.Config) *SocialOAuthClient {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &SocialOAuthClient{httpClient: client, tokenEndpoint: SocialAuthEndpoint, signinEndpoint: SocialSigninEndpoint}
}

func NewSocialOAuthClientWithHTTPClient(endpoint string, httpClient *http.Client) *SocialOAuthClient {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		endpoint = SocialAuthEndpoint
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &SocialOAuthClient{httpClient: httpClient, tokenEndpoint: endpoint, signinEndpoint: endpoint}
}

func (c *SocialOAuthClient) BuildLoginURL(provider, redirectURI, state, codeChallenge string) (string, error) {
	return c.BuildLoginURLWithOptions(provider, redirectURI, state, codeChallenge, SocialLoginURLOptions{})
}

func (c *SocialOAuthClient) BuildLoginURLWithOptions(provider, redirectURI, state, codeChallenge string, opts SocialLoginURLOptions) (string, error) {
	redirectURI = strings.TrimSpace(redirectURI)
	state = strings.TrimSpace(state)
	codeChallenge = strings.TrimSpace(codeChallenge)
	if redirectURI == "" {
		return "", fmt.Errorf("kiro oauth: redirect URI is required")
	}
	if state == "" {
		return "", fmt.Errorf("kiro oauth: state is required")
	}
	if codeChallenge == "" {
		return "", fmt.Errorf("kiro oauth: code challenge is required")
	}
	idp, err := kiroSocialIdentityProvider(provider)
	if err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("idp", idp)
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("redirect_uri", redirectURI)
	if prompt := strings.TrimSpace(opts.Prompt); prompt != "" {
		params.Set("prompt", prompt)
	} else if opts.ForceReauth {
		params.Set("prompt", "select_account")
	}
	return strings.TrimRight(c.signinEndpointURL(), "/") + "/login?" + params.Encode(), nil
}

func (c *SocialOAuthClient) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (*SocialTokenResponse, error) {
	code = strings.TrimSpace(code)
	redirectURI = strings.TrimSpace(redirectURI)
	codeVerifier = strings.TrimSpace(codeVerifier)
	if code == "" {
		return nil, fmt.Errorf("kiro oauth: authorization code is required")
	}
	if redirectURI == "" {
		return nil, fmt.Errorf("kiro oauth: redirect URI is required")
	}
	if codeVerifier == "" {
		return nil, fmt.Errorf("kiro oauth: code verifier is required")
	}
	payload := map[string]string{
		"code":          code,
		"code_verifier": codeVerifier,
		"redirect_uri":  redirectURI,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.tokenEndpointURL(), "/")+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &StatusError{Operation: "oauth token exchange", StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
	}
	var token SocialTokenResponse
	if err := json.Unmarshal(respBody, &token); err != nil {
		return nil, fmt.Errorf("kiro oauth: decode token response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("kiro oauth: token response missing access token")
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return nil, fmt.Errorf("kiro oauth: token response missing refresh token")
	}
	if token.ExpiresIn <= 0 {
		token.ExpiresIn = 3600
	}
	return &token, nil
}

func TokenDataFromSocialResponse(provider string, token *SocialTokenResponse) *TokenData {
	if token == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "google"
	}
	expiresIn := token.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &TokenData{
		AccessToken:  strings.TrimSpace(token.AccessToken),
		RefreshToken: strings.TrimSpace(token.RefreshToken),
		ProfileArn:   strings.TrimSpace(token.ProfileArn),
		ExpiresAt:    time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:   "kiro-cli-social",
		Provider:     provider,
		Email:        ExtractEmailFromJWT(token.AccessToken),
		Region:       DefaultRegion,
	}
}

func normalizeKiroSocialProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "google":
		return "google", nil
	case "github":
		return "github", nil
	default:
		return "", fmt.Errorf("kiro oauth: unsupported social provider %q", provider)
	}
}

func kiroSocialIdentityProvider(provider string) (string, error) {
	normalized, err := normalizeKiroSocialProvider(provider)
	if err != nil {
		return "", err
	}
	switch normalized {
	case "google":
		return "Google", nil
	case "github":
		return "Github", nil
	default:
		return "", fmt.Errorf("kiro oauth: unsupported social provider %q", provider)
	}
}

func generateCodeVerifier() (string, error) {
	buf := make([]byte, 96)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("kiro oauth: generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (c *SocialOAuthClient) tokenEndpointURL() string {
	if c != nil && strings.TrimSpace(c.tokenEndpoint) != "" {
		return strings.TrimRight(strings.TrimSpace(c.tokenEndpoint), "/")
	}
	return SocialAuthEndpoint
}

func (c *SocialOAuthClient) signinEndpointURL() string {
	if c != nil && strings.TrimSpace(c.signinEndpoint) != "" {
		return strings.TrimRight(strings.TrimSpace(c.signinEndpoint), "/")
	}
	return SocialSigninEndpoint
}

func (c *SocialOAuthClient) client() *http.Client {
	if c != nil && c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

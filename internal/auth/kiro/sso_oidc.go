package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	DefaultRegion     = "us-east-1"
	BuilderIDStartURL = "https://view.awsapps.com/start"

	kiroUserAgent    = "aws-sdk-rust/1.3.9 os/linux lang/rust/1.92.0"
	kiroAmzUserAgent = "aws-sdk-rust/1.3.9 ua/2.1 api/ssooidc/1.92.0 os/linux lang/rust/1.92.0 m/E app/AmazonQ-For-CLI"
)

var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
)

type SSOOIDCClient struct {
	httpClient *http.Client
	endpoint   string
}

type RegisterClientResponse struct {
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	ClientIDIssuedAt      int64  `json:"clientIdIssuedAt"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
}

type StartDeviceAuthResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type CreateTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
	RefreshToken string `json:"refreshToken"`
}

func NewSSOOIDCClient(cfg *config.Config) *SSOOIDCClient {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &SSOOIDCClient{
		httpClient: client,
		endpoint:   "https://oidc." + DefaultRegion + ".amazonaws.com",
	}
}

func NewSSOOIDCClientForRegion(cfg *config.Config, region string) *SSOOIDCClient {
	client := NewSSOOIDCClient(cfg)
	region = strings.TrimSpace(region)
	if region == "" {
		region = DefaultRegion
	}
	client.endpoint = "https://oidc." + region + ".amazonaws.com"
	return client
}

func NewSSOOIDCClientWithHTTPClient(endpoint string, httpClient *http.Client) *SSOOIDCClient {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		endpoint = "https://oidc." + DefaultRegion + ".amazonaws.com"
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &SSOOIDCClient{
		httpClient: httpClient,
		endpoint:   endpoint,
	}
}

func (c *SSOOIDCClient) RegisterClient(ctx context.Context) (*RegisterClientResponse, error) {
	payload := map[string]any{
		"clientName": "Kiro IDE",
		"clientType": "public",
		"scopes": []string{
			"codewhisperer:completions",
			"codewhisperer:analysis",
			"codewhisperer:conversations",
			"codewhisperer:transformations",
			"codewhisperer:taskassist",
		},
		"grantTypes": []string{
			"urn:ietf:params:oauth:grant-type:device_code",
			"refresh_token",
		},
	}

	var result RegisterClientResponse
	if err := c.postJSON(ctx, "/client/register", payload, &result); err != nil {
		return nil, fmt.Errorf("register client failed: %w", err)
	}
	if strings.TrimSpace(result.ClientID) == "" || strings.TrimSpace(result.ClientSecret) == "" {
		return nil, fmt.Errorf("register client failed: missing client credentials")
	}
	return &result, nil
}

func (c *SSOOIDCClient) StartDeviceAuthorization(ctx context.Context, clientID, clientSecret string) (*StartDeviceAuthResponse, error) {
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"startUrl":     BuilderIDStartURL,
	}

	var result StartDeviceAuthResponse
	if err := c.postJSON(ctx, "/device_authorization", payload, &result); err != nil {
		return nil, fmt.Errorf("start device authorization failed: %w", err)
	}
	if strings.TrimSpace(result.DeviceCode) == "" {
		return nil, fmt.Errorf("start device authorization failed: missing device code")
	}
	return &result, nil
}

func (c *SSOOIDCClient) CreateToken(ctx context.Context, clientID, clientSecret, deviceCode string) (*CreateTokenResponse, error) {
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"deviceCode":   deviceCode,
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
	}

	var result CreateTokenResponse
	if err := c.postJSON(ctx, "/token", payload, &result); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, fmt.Errorf("create token failed: missing access token")
	}
	return &result, nil
}

func (c *SSOOIDCClient) RefreshToken(ctx context.Context, clientID, clientSecret, refreshToken string) (*CreateTokenResponse, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	refreshToken = strings.TrimSpace(refreshToken)
	if clientID == "" {
		return nil, fmt.Errorf("refresh token failed: missing client id")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("refresh token failed: missing client secret")
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token failed: missing refresh token")
	}

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"grantType":    "refresh_token",
		"refreshToken": refreshToken,
	}

	var result CreateTokenResponse
	if err := c.postJSON(ctx, "/token", payload, &result); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, fmt.Errorf("refresh token failed: missing access token")
	}
	if strings.TrimSpace(result.RefreshToken) == "" {
		return nil, fmt.Errorf("refresh token failed: missing refresh token")
	}
	return &result, nil
}

func (c *SSOOIDCClient) FetchUserEmail(ctx context.Context, accessToken string) string {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ""
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.oidcEndpoint(), "/")+"/userinfo", nil)
	if err != nil {
		return ExtractEmailFromJWT(accessToken)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	applyKiroCLIHeaders(req, "1")

	resp, err := c.client().Do(req)
	if err != nil {
		return ExtractEmailFromJWT(accessToken)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return ExtractEmailFromJWT(accessToken)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ExtractEmailFromJWT(accessToken)
	}

	var userInfo struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return ExtractEmailFromJWT(accessToken)
	}

	for _, candidate := range []string{userInfo.Email, userInfo.PreferredUsername, userInfo.Sub} {
		candidate = strings.TrimSpace(candidate)
		if strings.Contains(candidate, "@") {
			return candidate
		}
	}
	return ExtractEmailFromJWT(accessToken)
}

func (c *SSOOIDCClient) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.oidcEndpoint(), "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyKiroCLIHeaders(req, "1")

	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusBadRequest {
		if pollErr := parsePollingError(respBody); pollErr != nil {
			return pollErr
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &StatusError{
			Operation:  strings.Trim(strings.TrimSpace(path), "/"),
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(respBody)),
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func applyKiroCLIHeaders(req *http.Request, maxAttempts string) {
	if req == nil {
		return
	}
	if strings.TrimSpace(maxAttempts) == "" {
		maxAttempts = "1"
	}
	req.Header.Set("User-Agent", kiroUserAgent)
	req.Header.Set("x-amz-user-agent", kiroAmzUserAgent)
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max="+maxAttempts)
}

func parsePollingError(body []byte) error {
	var errResp struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		return nil
	}
	switch strings.TrimSpace(errResp.Error) {
	case "authorization_pending":
		return ErrAuthorizationPending
	case "slow_down":
		return ErrSlowDown
	default:
		return nil
	}
}

func (c *SSOOIDCClient) oidcEndpoint() string {
	if c != nil && strings.TrimSpace(c.endpoint) != "" {
		return strings.TrimSpace(c.endpoint)
	}
	return "https://oidc." + DefaultRegion + ".amazonaws.com"
}

func (c *SSOOIDCClient) client() *http.Client {
	if c != nil && c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

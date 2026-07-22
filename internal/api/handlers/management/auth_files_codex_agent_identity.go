package management

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/ssh"
)

const (
	codexAuthModeAccessToken                     = "access_token"
	codexAuthModeAgentIdentity                   = "agent_identity"
	codexAgentRegistrationURL                    = coreauth.CodexAgentIdentityProductionAuthAPIBaseURL + "/v1/agent/register"
	codexAgentRegistrationTimeout                = 30 * time.Second
	codexAgentRegistrationMaxBytes               = 64 << 10
	codexAgentRegistrationMaxAttempts            = 3
	codexAgentRegistrationCapabilityResponsesAPI = "responsesapi"
	codexAgentRegistrationCLIHarnessID           = "codex-cli"
	codexAgentRegistrationAppHarnessID           = "codex-app"
	codexAgentIdentityAccountIDMetadataKey       = "agent_identity_account_id"
	codexAgentIdentityChatGPTUserIDMetadataKey   = "agent_identity_chatgpt_user_id"
)

var codexAgentIdentityRegisterURL = codexAgentRegistrationURL

type codexAuthModeRequest struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type codexAgentRegistrationRequest struct {
	ABOM         codexAgentRegistrationABOM `json:"abom"`
	Key          string                     `json:"agent_public_key"`
	Capabilities []string                   `json:"capabilities"`
	TTL          *uint64                    `json:"ttl"`
}

type codexAgentRegistrationABOM struct {
	AgentVersion    string `json:"agent_version"`
	AgentHarnessID  string `json:"agent_harness_id"`
	RunningLocation string `json:"running_location"`
}

type codexAgentRegistrationResponse struct {
	AgentRuntimeID      string `json:"agent_runtime_id"`
	AgentRuntimeIDCamel string `json:"agentRuntimeId"`
}

type codexGeneratedAgentIdentity struct {
	RuntimeID  string
	PrivateKey string
}

// PatchCodexAuthMode switches a file-backed Codex credential between its
// retained access token and Agent Identity signing material.
func (h *Handler) PatchCodexAuthMode(c *gin.Context) {
	manager := h.authManagerSnapshot()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req codexAuthModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Mode = normalizeCodexAuthMode(req.Mode)
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Mode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be access_token or agent_identity"})
		return
	}

	targetAuth := authManagerAuthByIDOrFileName(manager, req.Name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(targetAuth.Provider), "codex") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not a Codex credential"})
		return
	}
	if isRuntimeOnlyAuth(targetAuth) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "runtime-only auth cannot be persisted"})
		return
	}

	authDir := h.authDirSnapshot()
	path := resolvePatchAuthFilePath(targetAuth, authDir, req.Name)
	if path == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	data, err := readManagedAuthPathFile(path, authDir)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": "failed to read auth file"})
		return
	}
	doc := make(map[string]any)
	if err = json.Unmarshal(data, &doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth file"})
		return
	}

	fileAuth := coreauth.NewAuthFromAuthFileMetadata(doc, coreauth.AuthFileProjectionOptions{
		ID:                    targetAuth.ID,
		Path:                  path,
		BaseDir:               authDir,
		FileName:              targetAuth.FileName,
		UseBaseNameAsFileName: true,
		CreatedAt:             targetAuth.CreatedAt,
		UpdatedAt:             targetAuth.UpdatedAt,
	})
	if !strings.EqualFold(strings.TrimSpace(fileAuth.Provider), "codex") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not a Codex credential"})
		return
	}

	created := false
	switch req.Mode {
	case codexAuthModeAccessToken:
		if !coreauth.CodexAccessTokenAvailable(fileAuth) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "access_token is missing"})
			return
		}
		setCodexAuthModeDocument(doc, coreauth.CodexAuthKindOAuth)
	case codexAuthModeAgentIdentity:
		accessToken := codexUsageAccessToken(fileAuth)
		claims, claimErr := validateCodexAgentAccessToken(accessToken)
		identityAvailable := coreauth.CodexAgentIdentityAvailable(fileAuth)
		var identityErr error
		if identityAvailable {
			identityErr = validateCodexAgentIdentity(fileAuth)
		}
		needsRegistration := !identityAvailable || identityErr != nil
		if !needsRegistration && claimErr == nil && !codexAgentIdentityMatchesClaims(fileAuth, claims) {
			needsRegistration = true
		}
		if needsRegistration {
			if claimErr != nil {
				if identityErr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": identityErr.Error()})
				} else {
					c.JSON(http.StatusBadRequest, gin.H{"error": claimErr.Error()})
				}
				return
			}
			identity, createErr := h.createCodexAgentIdentity(c.Request.Context(), fileAuth, accessToken, claims)
			if createErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": createErr.Error()})
				return
			}
			doc["agent_runtime_id"] = identity.RuntimeID
			doc["agent_private_key"] = identity.PrivateKey
			delete(doc, "task_id")
			delete(doc, "taskId")
			applyCodexAgentClaims(doc, claims)
			applyCodexAgentIdentityBinding(doc, claims)
			created = true
		}
		doc[coreauth.AuthFileCodexClientProfilePinnedKey] = true
		setCodexAuthModeDocument(doc, coreauth.CodexAuthKindAgentIdentity)
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode auth file"})
		return
	}
	encoded = append(encoded, '\n')
	if err = writeManagedAuthPathFile(path, authDir, encoded, 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write auth file"})
		return
	}

	updatedAuth := targetAuth.Clone()
	updatedAuth.Metadata = doc
	if updatedAuth.Attributes == nil {
		updatedAuth.Attributes = make(map[string]string)
	}
	if req.Mode == codexAuthModeAgentIdentity {
		updatedAuth.Attributes["auth_kind"] = coreauth.CodexAuthKindAgentIdentity
	} else {
		updatedAuth.Attributes["auth_kind"] = coreauth.CodexAuthKindOAuth
	}
	coreauth.ApplyAuthFileOptionsFromMetadata(updatedAuth)
	coreauth.ApplyCodexMetadataFromMetadata(updatedAuth)
	coreauth.ApplyCustomHeadersFromMetadata(updatedAuth)
	updatedAuth.UpdatedAt = time.Now()

	updatedAuth, err = manager.Update(c.Request.Context(), updatedAuth)
	if err != nil || updatedAuth == nil {
		// Manager.Update installs its in-memory snapshot before persisting. Put
		// both layers back so a persistence failure cannot leave a half-switched
		// credential behind.
		_ = writeManagedAuthPathFile(path, authDir, data, 0o600)
		_, _ = manager.Update(c.Request.Context(), targetAuth.Clone())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update auth"})
		return
	}
	executor.CloseCodexWebsocketSessionsForAuthID(updatedAuth.ID, "auth_mode_changed")
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"mode":    req.Mode,
		"created": created,
		"file":    h.buildAuthFileEntry(updatedAuth),
	})
}

func normalizeCodexAuthMode(mode string) string {
	mode = strings.TrimSpace(mode)
	switch {
	case strings.EqualFold(mode, codexAuthModeAccessToken),
		strings.EqualFold(mode, "accessToken"),
		strings.EqualFold(mode, "oauth"):
		return codexAuthModeAccessToken
	case strings.EqualFold(mode, codexAuthModeAgentIdentity),
		strings.EqualFold(mode, "agentIdentity"):
		return codexAuthModeAgentIdentity
	default:
		return ""
	}
}

func setCodexAuthModeDocument(doc map[string]any, kind string) {
	if doc == nil {
		return
	}
	if strings.TrimSpace(valueAsString(doc["type"])) == "" {
		doc["type"] = "codex"
	}
	delete(doc, "authKind")
	delete(doc, "auth_mode")
	delete(doc, "authMode")
	doc["auth_kind"] = kind
}

func validateCodexAgentAccessToken(accessToken string) (*codexauth.JWTClaims, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errors.New("access_token is missing")
	}
	claims, err := codexauth.ParseJWTToken(accessToken)
	if err != nil {
		return nil, errors.New("access_token is not a valid ChatGPT JWT")
	}
	if strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID) == "" {
		return nil, errors.New("access_token is missing chatgpt_account_id")
	}
	if strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID) == "" {
		return nil, errors.New("access_token is missing chatgpt_user_id")
	}
	return claims, nil
}

func validateCodexAgentIdentity(auth *coreauth.Auth) error {
	if auth == nil || !coreauth.CodexAgentIdentityAvailable(auth) {
		return errors.New("Agent Identity material is incomplete")
	}
	raw := codexAuthMetadataString(auth.Metadata, "agent_private_key", "agentPrivateKey")
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return errors.New("agent_private_key is not valid base64")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return errors.New("agent_private_key is not valid PKCS#8")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("agent_private_key is not Ed25519")
	}
	return nil
}

func (h *Handler) createCodexAgentIdentity(ctx context.Context, auth *coreauth.Auth, accessToken string, claims *codexauth.JWTClaims) (codexGeneratedAgentIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return codexGeneratedAgentIdentity{}, errors.New("failed to generate Agent Identity key")
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return codexGeneratedAgentIdentity{}, errors.New("failed to encode Agent Identity key")
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return codexGeneratedAgentIdentity{}, errors.New("failed to encode Agent Identity public key")
	}

	agentVersion, headers := codexAgentRegistrationHeaders(auth)
	payload := codexAgentRegistrationRequest{
		ABOM:         codexAgentRegistrationABOMForHeaders(agentVersion, headers),
		Key:          strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey))),
		Capabilities: []string{codexAgentRegistrationCapabilityResponsesAPI},
		TTL:          nil,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return codexGeneratedAgentIdentity{}, errors.New("failed to encode Agent Identity registration")
	}

	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, codexAgentRegistrationTimeout)
	defer cancel()

	h.mu.RLock()
	cfg := h.cfg
	h.mu.RUnlock()
	client := helps.NewOpenAIAuthHTTPClient(cfg, auth, codexAgentRegistrationTimeout)
	registrationURL := codexAgentRegistrationURLForAuth(auth)
	for attempt := 1; attempt <= codexAgentRegistrationMaxAttempts; attempt++ {
		req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, registrationURL, bytes.NewReader(body))
		if requestErr != nil {
			return codexGeneratedAgentIdentity{}, errors.New("failed to build Agent Identity registration")
		}
		for key, values := range headers {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
		if codexAgentRegistrationIsFedramp(auth, claims) {
			req.Header.Set("X-OpenAI-Fedramp", "true")
		}

		resp, requestErr := client.Do(req)
		if requestErr != nil {
			if attempt < codexAgentRegistrationMaxAttempts && requestCtx.Err() == nil {
				continue
			}
			return codexGeneratedAgentIdentity{}, errors.New("Agent Identity registration request failed")
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			statusCode := resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, codexAgentRegistrationMaxBytes))
			_ = resp.Body.Close()
			if attempt < codexAgentRegistrationMaxAttempts && codexAgentRegistrationRetryableStatus(statusCode) {
				continue
			}
			return codexGeneratedAgentIdentity{}, fmt.Errorf("Agent Identity registration returned status %d", statusCode)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, codexAgentRegistrationMaxBytes+1))
		_ = resp.Body.Close()
		if readErr != nil || len(responseBody) > codexAgentRegistrationMaxBytes {
			return codexGeneratedAgentIdentity{}, errors.New("Agent Identity registration response is invalid")
		}
		var result codexAgentRegistrationResponse
		if err = json.Unmarshal(responseBody, &result); err != nil {
			return codexGeneratedAgentIdentity{}, errors.New("Agent Identity registration response is invalid")
		}
		runtimeID := strings.TrimSpace(result.AgentRuntimeID)
		if runtimeID == "" {
			runtimeID = strings.TrimSpace(result.AgentRuntimeIDCamel)
		}
		if runtimeID == "" {
			return codexGeneratedAgentIdentity{}, errors.New("Agent Identity registration response omitted agent_runtime_id")
		}
		return codexGeneratedAgentIdentity{
			RuntimeID:  runtimeID,
			PrivateKey: base64.StdEncoding.EncodeToString(privateDER),
		}, nil
	}
	return codexGeneratedAgentIdentity{}, errors.New("Agent Identity registration request failed")
}

func codexAgentRegistrationURLForAuth(auth *coreauth.Auth) string {
	if override := strings.TrimSpace(codexAgentIdentityRegisterURL); override != "" && override != codexAgentRegistrationURL {
		return override
	}
	return strings.TrimRight(coreauth.CodexAgentIdentityAuthAPIBaseURL(auth), "/") + "/v1/agent/register"
}

func codexAgentRegistrationABOMForHeaders(agentVersion string, headers http.Header) codexAgentRegistrationABOM {
	harnessID := codexAgentRegistrationCLIHarnessID
	source := "cli"
	clientHint := strings.ToLower(strings.TrimSpace(headers.Get(coreauth.AuthFileCodexOriginatorHeader) + " " + headers.Get("User-Agent")))
	if strings.Contains(clientHint, "vscode") || strings.Contains(clientHint, "codex-app") {
		harnessID = codexAgentRegistrationAppHarnessID
		source = "vscode"
	}
	return codexAgentRegistrationABOM{
		AgentVersion:    strings.TrimSpace(agentVersion),
		AgentHarnessID:  harnessID,
		RunningLocation: source + "-" + runtime.GOOS,
	}
}

func codexAgentRegistrationIsFedramp(auth *coreauth.Auth, claims *codexauth.JWTClaims) bool {
	if claims != nil && claims.CodexAuthInfo.ChatgptAccountIsFedramp != nil {
		return *claims.CodexAuthInfo.ChatgptAccountIsFedramp
	}
	return codexUsageFedramp(auth)
}

func codexAgentRegistrationRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func codexAgentRegistrationHeaders(auth *coreauth.Auth) (string, http.Header) {
	headers := make(http.Header)
	userAgent := codexAgentClientFeatureString(auth, "user_agent", "user-agent", "userAgent", "User-Agent")
	originator := codexAgentClientFeatureString(auth, coreauth.AuthFileCodexOriginatorKey, coreauth.AuthFileCodexOriginatorHeader)
	version := codexAgentClientFeatureString(auth, "version", "Version")
	if userAgent == "" {
		userAgent = misc.CodexCLIUserAgentWithOriginatorAndVersion(originator, version)
	}
	if originator == "" {
		originator = codexAgentUserAgentProduct(userAgent)
	}
	if originator == "" {
		originator = misc.CodexCLIOriginator
	}
	if version == "" {
		version = codexAgentUserAgentVersion(userAgent)
	}
	if version == "" {
		version = misc.CodexCLIVersion
	}
	headers.Set("User-Agent", userAgent)
	headers.Set(coreauth.AuthFileCodexOriginatorHeader, originator)
	headers.Set("Version", version)
	for _, item := range []struct {
		header string
		keys   []string
	}{
		{coreauth.AuthFileCodexBetaFeaturesHeader, []string{coreauth.AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures", coreauth.AuthFileCodexBetaFeaturesHeader}},
		{coreauth.AuthFileCodexInstallationIDHeader, []string{coreauth.AuthFileCodexInstallationIDKey, "installation-id", "installationId", coreauth.AuthFileCodexInstallationIDHeader}},
		{coreauth.AuthFileCodexIncludeTimingMetricsHeader, []string{coreauth.AuthFileCodexIncludeTimingMetricsKey, "include-timing-metrics", "includeTimingMetrics", coreauth.AuthFileCodexIncludeTimingMetricsHeader}},
	} {
		if value := codexAgentClientFeatureString(auth, item.keys...); value != "" {
			headers.Set(item.header, value)
		}
	}
	return version, headers
}

func codexAgentClientFeatureString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		for _, key := range keys {
			for _, candidate := range []string{key, "header:" + key} {
				if value := strings.TrimSpace(auth.Attributes[candidate]); value != "" {
					return value
				}
			}
		}
	}
	return codexAgentMetadataFeatureString(auth.Metadata, keys...)
}

func codexAgentMetadataFeatureString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(valueAsString(metadata[key])); value != "" {
			return value
		}
	}
	if headers := authFileMetadataHeaders(metadata); len(headers) > 0 {
		for _, key := range keys {
			if value := authFileHeaderValue(headers, key); value != "" {
				return value
			}
		}
	}
	for _, objectKey := range []string{"client_profile", "clientProfile", "client_features", "clientFeatures"} {
		if nested, ok := metadata[objectKey].(map[string]any); ok {
			if value := codexAgentMetadataFeatureString(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

func codexAgentUserAgentProduct(userAgent string) string {
	token := strings.TrimSpace(strings.SplitN(userAgent, " ", 2)[0])
	product, _, ok := strings.Cut(token, "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(product)
}

func codexAgentUserAgentVersion(userAgent string) string {
	token := strings.TrimSpace(strings.SplitN(userAgent, " ", 2)[0])
	_, version, ok := strings.Cut(token, "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(version)
}

func codexAgentIdentityMatchesClaims(auth *coreauth.Auth, claims *codexauth.JWTClaims) bool {
	if auth == nil || claims == nil {
		return false
	}
	boundAccountID := codexAuthMetadataString(auth.Metadata, codexAgentIdentityAccountIDMetadataKey)
	boundUserID := codexAuthMetadataString(auth.Metadata, codexAgentIdentityChatGPTUserIDMetadataKey)
	if boundAccountID == "" || boundUserID == "" {
		return false
	}
	return boundAccountID == strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID) &&
		boundUserID == strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID)
}

func applyCodexAgentIdentityBinding(doc map[string]any, claims *codexauth.JWTClaims) {
	if doc == nil || claims == nil {
		return
	}
	if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); value != "" {
		doc[codexAgentIdentityAccountIDMetadataKey] = value
	}
	if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID); value != "" {
		doc[codexAgentIdentityChatGPTUserIDMetadataKey] = value
	}
}

func applyCodexAgentClaims(doc map[string]any, claims *codexauth.JWTClaims) {
	if doc == nil || claims == nil {
		return
	}
	if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); value != "" {
		doc["account_id"] = value
	}
	if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID); value != "" {
		doc["chatgpt_user_id"] = value
	}
	if value := strings.TrimSpace(claims.GetUserEmail()); value != "" {
		doc["email"] = value
	}
	if value := strings.TrimSpace(claims.GetPlanType()); value != "" {
		doc["plan_type"] = value
	}
	if claims.CodexAuthInfo.ChatgptAccountIsFedramp != nil {
		doc["fedramp"] = *claims.CodexAuthInfo.ChatgptAccountIsFedramp
	}
}

func applyCodexAuthModeEntry(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	switch coreauth.CodexAuthKind(auth) {
	case coreauth.CodexAuthKindAgentIdentity:
		entry["auth_mode"] = codexAuthModeAgentIdentity
	case coreauth.CodexAuthKindOAuth:
		entry["auth_mode"] = codexAuthModeAccessToken
	}
	entry["has_access_token"] = coreauth.CodexAccessTokenAvailable(auth)
	entry["has_agent_identity"] = coreauth.CodexAgentIdentityAvailable(auth)
}

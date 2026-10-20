package management

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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/antigravity"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	geminiCLIEndpoint = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion  = "v1internal"
)

// RequestGeminiCLIToken starts the Gemini CLI OAuth flow used by the management UI.
// The OAuth callback is forwarded to the management server and the resulting
// credential is onboarded against the selected Google Cloud project.
func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := PopulateAuthContext(context.Background(), c)
	proxyHTTPClient := util.SetProxy(&h.cfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	projectID := c.Query("project_id")
	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	RegisterOAuthSession(state, "gemini")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		payload, errWait := h.waitForOAuthCallbackFile("gemini", state, fmt.Sprintf(".oauth-gemini-%s.oauth", state), 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			log.Errorf("Gemini OAuth callback failed: %v", errWait)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errWait))
			return
		}
		if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
			log.Errorf("Gemini authentication failed: %s", errStr)
			SetOAuthSessionError(state, "Authentication failed: "+errStr)
			return
		}
		authCode := strings.TrimSpace(payload["code"])
		if authCode == "" {
			SetOAuthSessionError(state, "Authentication failed: code not found")
			return
		}

		token, errExchange := conf.Exchange(ctx, authCode)
		if errExchange != nil {
			log.Errorf("Failed to exchange Gemini token: %v", errExchange)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to exchange token", errExchange))
			return
		}

		userInfoReq, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
		if errRequest != nil {
			SetOAuthSessionError(state, "Could not get user info")
			return
		}
		userInfoReq.Header.Set("Content-Type", "application/json")
		userInfoReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
		userInfoResp, errDo := conf.Client(ctx, token).Do(userInfoReq)
		if errDo != nil {
			log.Errorf("Failed to fetch Gemini user info: %v", errDo)
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}
		bodyBytes, errRead := io.ReadAll(userInfoResp.Body)
		if errClose := userInfoResp.Body.Close(); errClose != nil {
			log.Warnf("failed to close Gemini user info response: %v", errClose)
		}
		if errRead != nil {
			SetOAuthSessionError(state, "Failed to read user info")
			return
		}
		if userInfoResp.StatusCode < http.StatusOK || userInfoResp.StatusCode >= http.StatusMultipleChoices {
			log.Errorf("Gemini user info request failed with status %d: %s", userInfoResp.StatusCode, strings.TrimSpace(string(bodyBytes)))
			SetOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", userInfoResp.StatusCode))
			return
		}
		email := strings.TrimSpace(gjson.GetBytes(bodyBytes, "email").String())

		var tokenData map[string]any
		jsonData, errMarshal := json.Marshal(token)
		if errMarshal != nil {
			SetOAuthSessionError(state, "Failed to marshal token")
			return
		}
		if errUnmarshal := json.Unmarshal(jsonData, &tokenData); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal Gemini token: %v", errUnmarshal)
			SetOAuthSessionError(state, "Failed to unmarshal token")
			return
		}
		tokenData["token_uri"] = "https://oauth2.googleapis.com/token"
		tokenData["client_id"] = geminiAuth.ClientID
		tokenData["client_secret"] = geminiAuth.ClientSecret
		tokenData["scopes"] = geminiAuth.Scopes
		tokenData["universe_domain"] = "googleapis.com"

		storage := geminiAuth.GeminiTokenStorage{
			Token:     tokenData,
			ProjectID: strings.TrimSpace(projectID),
			Email:     email,
			Auto:      strings.TrimSpace(projectID) == "",
		}
		gemClient, errGetClient := geminiAuth.NewGeminiAuth().GetAuthenticatedClient(ctx, &storage, h.cfg, &geminiAuth.WebLoginOptions{NoBrowser: true})
		if errGetClient != nil {
			log.Errorf("failed to initialize Gemini client: %v", errGetClient)
			SetOAuthSessionError(state, "Failed to get authenticated client")
			return
		}

		requestedProjectID := strings.TrimSpace(projectID)
		switch {
		case strings.EqualFold(requestedProjectID, "ALL"):
			storage.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &storage)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errAll))
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errVerify))
				return
			}
			storage.ProjectID = strings.Join(projects, ",")
			storage.Checked = true
		case strings.EqualFold(requestedProjectID, "GOOGLE_ONE"):
			storage.Auto = false
			if errSetup := performGeminiCLISetup(ctx, gemClient, &storage, ""); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				SetOAuthSessionError(state, fmt.Sprintf("Google One auto-discovery failed: %v", errSetup))
				return
			}
			if strings.TrimSpace(storage.ProjectID) == "" {
				SetOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, storage.ProjectID)
			if errCheck != nil {
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			storage.Checked = isChecked
			if !isChecked {
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", storage.ProjectID))
				return
			}
		default:
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &storage, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errEnsure))
				return
			}
			if strings.TrimSpace(storage.ProjectID) == "" {
				SetOAuthSessionError(state, "Failed to resolve project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, storage.ProjectID)
			if errCheck != nil {
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			storage.Checked = isChecked
			if !isChecked {
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", storage.ProjectID))
				return
			}
		}

		fileName := geminiAuth.CredentialFileName(storage.Email, storage.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &storage,
			Metadata: map[string]any{"email": storage.Email, "project_id": storage.ProjectID, "auto": storage.Auto, "checked": storage.Checked},
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save Gemini token: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("gemini")
		fmt.Printf("Gemini CLI authentication successful; token saved to %s\n", savedPath)
	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestAntigravityToken starts the Antigravity OAuth flow used by the management UI.
func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	ctx := PopulateAuthContext(context.Background(), c)
	authSvc := antigravity.NewAntigravityAuth(h.cfg, nil)

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", antigravity.CallbackPort)
	authURL := authSvc.BuildAuthURL(state, redirectURI)
	RegisterOAuthSession(state, "antigravity")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/antigravity/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute antigravity callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(antigravity.CallbackPort, "antigravity", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start antigravity callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(antigravity.CallbackPort, forwarder)
		}
		payload, errWait := h.waitForOAuthCallbackFile("antigravity", state, fmt.Sprintf(".oauth-antigravity-%s.oauth", state), 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errWait))
			return
		}
		if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
			SetOAuthSessionError(state, "Authentication failed: "+errStr)
			return
		}
		authCode := strings.TrimSpace(payload["code"])
		if authCode == "" {
			SetOAuthSessionError(state, "Authentication failed: code not found")
			return
		}
		tokenResp, errToken := authSvc.ExchangeCodeForTokens(ctx, authCode, redirectURI)
		if errToken != nil || tokenResp == nil || strings.TrimSpace(tokenResp.AccessToken) == "" {
			log.Errorf("Failed to exchange Antigravity token: %v", errToken)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to exchange token", errToken))
			return
		}

		accessToken := strings.TrimSpace(tokenResp.AccessToken)
		email, errInfo := authSvc.FetchUserInfo(ctx, accessToken)
		if errInfo != nil {
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to fetch user info", errInfo))
			return
		}
		email = strings.TrimSpace(email)
		if email == "" {
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}

		projectID := ""
		if fetchedProjectID, errProject := authSvc.FetchProjectID(ctx, accessToken); errProject != nil {
			log.Warnf("antigravity: failed to fetch project ID: %v", errProject)
		} else {
			projectID = fetchedProjectID
			log.Infof("antigravity: obtained project ID %s", util.HideAPIKey(projectID))
		}

		now := time.Now()
		metadata := map[string]any{
			"type":          "antigravity",
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_in":    tokenResp.ExpiresIn,
			"timestamp":     now.UnixMilli(),
			"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
			"email":         email,
		}
		if projectID != "" {
			metadata["project_id"] = projectID
		}
		fileName := antigravity.CredentialFileName(email)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "antigravity",
			FileName: fileName,
			Label:    email,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			SetOAuthSessionError(state, "Failed to save token to file")
			log.Errorf("Failed to save Antigravity token: %v", errSave)
			return
		}
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("antigravity")
		fmt.Printf("Antigravity authentication successful; token saved to %s\n", savedPath)
	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

type projectSelectionRequiredError struct{}

func (*projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}
	projectID := strings.TrimSpace(requestedProject)
	if projectID == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 || strings.TrimSpace(projects[0].ProjectID) == "" {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		projectID = strings.TrimSpace(projects[0].ProjectID)
		storage.Auto = true
	} else {
		storage.Auto = false
	}
	if err := performGeminiCLISetup(ctx, httpClient, storage, projectID); err != nil {
		return err
	}
	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = projectID
	}
	return nil
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		projectID := strings.TrimSpace(project.ProjectID)
		if projectID == "" {
			continue
		}
		if _, ok := seen[projectID]; ok {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, projectID); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", projectID, err)
		}
		if finalID := strings.TrimSpace(storage.ProjectID); finalID != "" {
			activated = append(activated, finalID)
		} else {
			activated = append(activated, projectID)
		}
		seen[projectID] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, projectID := range projectIDs {
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			continue
		}
		enabled, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, projectID)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", projectID, errCheck)
		}
		if !enabled {
			return fmt.Errorf("project %s: Cloud AI API not enabled", projectID)
		}
	}
	return nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}
	requestedProject = strings.TrimSpace(requestedProject)
	loadRequest := map[string]any{"metadata": metadata}
	if requestedProject != "" {
		loadRequest["cloudaicompanionProject"] = requestedProject
	}
	var loadResponse map[string]any
	if err := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadRequest, &loadResponse); err != nil {
		return fmt.Errorf("load code assist: %w", err)
	}

	tierID := "legacy-tier"
	if tiers, ok := loadResponse["allowedTiers"].([]any); ok {
		for _, rawTier := range tiers {
			tier, ok := rawTier.(map[string]any)
			if !ok {
				continue
			}
			if isDefault, ok := tier["isDefault"].(bool); ok && isDefault {
				if id, ok := tier["id"].(string); ok && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := requestedProject
	if projectID == "" {
		switch value := loadResponse["cloudaicompanionProject"].(type) {
		case string:
			projectID = strings.TrimSpace(value)
		case map[string]any:
			if id, ok := value["id"].(string); ok {
				projectID = strings.TrimSpace(id)
			}
		}
	}
	if projectID == "" {
		autoCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for attempt := 1; ; attempt++ {
			var response map[string]any
			if err := callGeminiCLI(autoCtx, httpClient, "onboardUser", map[string]any{"tierId": tierID, "metadata": metadata}, &response); err != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", err)
			}
			if done, ok := response["done"].(bool); ok && done {
				if result, ok := response["response"].(map[string]any); ok {
					switch value := result["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(value)
					case map[string]any:
						if id, ok := value["id"].(string); ok {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}
			log.Debugf("Gemini auto-discovery in progress, attempt %d", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}
		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
	}

	storage.ProjectID = projectID
	onboardRequest := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}
	for {
		var response map[string]any
		if err := callGeminiCLI(ctx, httpClient, "onboardUser", onboardRequest, &response); err != nil {
			return fmt.Errorf("onboard user: %w", err)
		}
		if done, ok := response["done"].(bool); ok && done {
			responseProjectID := ""
			if result, ok := response["response"].(map[string]any); ok {
				switch value := result["cloudaicompanionProject"].(type) {
				case string:
					responseProjectID = strings.TrimSpace(value)
				case map[string]any:
					if id, ok := value["id"].(string); ok {
						responseProjectID = strings.TrimSpace(id)
					}
				}
			}
			if responseProjectID != "" {
				storage.ProjectID = responseProjectID
			}
			if strings.TrimSpace(storage.ProjectID) == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	if httpClient == nil {
		return fmt.Errorf("http client is nil")
	}
	endpointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endpointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}
	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if errDecode := json.NewDecoder(resp.Body).Decode(result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}
	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]interfaces.GCPProjectProjects, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	var projects interfaces.GCPProject
	if errDecode := json.NewDecoder(resp.Body).Decode(&projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}
	return projects.Projects, nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	service := "cloudaicompanion.googleapis.com"
	baseURL := "https://serviceusage.googleapis.com"
	checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", baseURL, projectID, service)
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if errRequest != nil {
		return false, fmt.Errorf("failed to create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return false, fmt.Errorf("failed to execute request: %w", errDo)
	}
	bodyBytes, errRead := io.ReadAll(resp.Body)
	resp.Body.Close()
	if errRead != nil {
		return false, fmt.Errorf("failed to read service status: %w", errRead)
	}
	if resp.StatusCode == http.StatusOK && gjson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
		return true, nil
	}

	enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", baseURL, projectID, service)
	enableReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
	if errRequest != nil {
		return false, fmt.Errorf("failed to create enable request: %w", errRequest)
	}
	enableReq.Header.Set("Content-Type", "application/json")
	enableReq.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
	enableResp, errDo := httpClient.Do(enableReq)
	if errDo != nil {
		return false, fmt.Errorf("failed to enable service: %w", errDo)
	}
	enableBody, _ := io.ReadAll(enableResp.Body)
	enableResp.Body.Close()
	if enableResp.StatusCode == http.StatusOK || enableResp.StatusCode == http.StatusCreated {
		return true, nil
	}
	errMessage := strings.TrimSpace(string(enableBody))
	if message := gjson.GetBytes(enableBody, "error.message").String(); message != "" {
		errMessage = message
	}
	if enableResp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(errMessage), "already enabled") {
		return true, nil
	}
	return false, fmt.Errorf("project activation required: %s", errMessage)
}

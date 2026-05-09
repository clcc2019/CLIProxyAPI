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

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	kiroCodeWhispererEndpoint    = "https://codewhisperer.us-east-1.amazonaws.com"
	kiroListModelsPath           = "listAvailableModels"
	kiroGetUsageLimitsPath       = "getUsageLimits"
	kiroListModelsTarget         = "AmazonCodeWhispererService.ListAvailableModels"
	kiroGetUsageLimitsTarget     = "AmazonCodeWhispererService.GetUsageLimits"
	kiroListProfilesTarget       = "AmazonCodeWhispererService.ListProfiles"
	kiroListCustomizationsTarget = "AmazonCodeWhispererService.ListAvailableCustomizations"
	kiroCodeWhispererAmzAgent    = "aws-sdk-rust/1.3.9 ua/2.1 api/codewhispererruntime/1.92.0 os/linux lang/rust/1.92.0 m/E app/Kiro"
)

type KiroAuth struct {
	httpClient *http.Client
	endpoint   string
}

type KiroModel struct {
	ModelID        string  `json:"modelId"`
	ModelName      string  `json:"modelName"`
	Description    string  `json:"description"`
	RateMultiplier float64 `json:"rateMultiplier"`
	RateUnit       string  `json:"rateUnit"`
	MaxInputTokens int     `json:"maxInputTokens,omitempty"`
}

type StatusError struct {
	Operation  string
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = "request"
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("kiro: %s failed (status %d)", operation, e.StatusCode)
	}
	return fmt.Sprintf("kiro: %s failed (status %d): %s", operation, e.StatusCode, body)
}

func IsUnauthorizedStatusError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
		body := strings.ToLower(strings.TrimSpace(statusErr.Body))
		return strings.Contains(body, "bearer token") && strings.Contains(body, "invalid") ||
			strings.Contains(body, "bad credentials")
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "status 401") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "bad credentials") ||
		(strings.Contains(msg, "bearer token") && strings.Contains(msg, "invalid"))
}

type KiroUsageInfo struct {
	DaysUntilReset                   *int                  `json:"daysUntilReset,omitempty"`
	NextDateReset                    *float64              `json:"nextDateReset,omitempty"`
	NextResetAt                      string                `json:"nextResetAt,omitempty"`
	UserInfo                         *KiroUserInfo         `json:"userInfo,omitempty"`
	SubscriptionInfo                 *KiroSubscriptionInfo `json:"subscriptionInfo,omitempty"`
	UsageBreakdownList               []KiroUsageBreakdown  `json:"usageBreakdownList,omitempty"`
	TotalUsageLimitWithPrecision     *float64              `json:"totalUsageLimitWithPrecision,omitempty"`
	TotalCurrentUsageWithPrecision   *float64              `json:"totalCurrentUsageWithPrecision,omitempty"`
	TotalRemainingUsageWithPrecision *float64              `json:"totalRemainingUsageWithPrecision,omitempty"`
	TotalUsagePercentage             *float64              `json:"totalUsagePercentage,omitempty"`
	Exhausted                        bool                  `json:"exhausted"`
}

type KiroUserInfo struct {
	Email  string `json:"email,omitempty"`
	UserID string `json:"userId,omitempty"`
}

type KiroSubscriptionInfo struct {
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"`
	Type              string `json:"type,omitempty"`
}

type KiroUsageBreakdown struct {
	UsageLimit                *int               `json:"usageLimit,omitempty"`
	CurrentUsage              *int               `json:"currentUsage,omitempty"`
	UsageLimitWithPrecision   *float64           `json:"usageLimitWithPrecision,omitempty"`
	CurrentUsageWithPrecision *float64           `json:"currentUsageWithPrecision,omitempty"`
	NextDateReset             *float64           `json:"nextDateReset,omitempty"`
	DisplayName               string             `json:"displayName,omitempty"`
	ResourceType              string             `json:"resourceType,omitempty"`
	FreeTrialInfo             *KiroFreeTrialInfo `json:"freeTrialInfo,omitempty"`
	RemainingWithPrecision    *float64           `json:"remainingWithPrecision,omitempty"`
	UsagePercentage           *float64           `json:"usagePercentage,omitempty"`
	Exhausted                 bool               `json:"exhausted"`
}

type KiroFreeTrialInfo struct {
	FreeTrialStatus           string   `json:"freeTrialStatus,omitempty"`
	UsageLimit                *int     `json:"usageLimit,omitempty"`
	CurrentUsage              *int     `json:"currentUsage,omitempty"`
	UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision,omitempty"`
	CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision,omitempty"`
	RemainingWithPrecision    *float64 `json:"remainingWithPrecision,omitempty"`
	UsagePercentage           *float64 `json:"usagePercentage,omitempty"`
	Exhausted                 bool     `json:"exhausted"`
}

func NewKiroAuth(cfg *config.Config) *KiroAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &KiroAuth{
		httpClient: client,
		endpoint:   kiroCodeWhispererEndpoint,
	}
}

func (k *KiroAuth) ListAvailableModels(ctx context.Context, tokenData *TokenData) ([]*KiroModel, error) {
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return nil, fmt.Errorf("kiro: access token is required to list models")
	}

	profileArn := effectiveKiroProfileArn(tokenData)
	if profileArn == "" && shouldResolveKiroProfileArn(tokenData) {
		if resolved, err := k.ResolveProfileArn(ctx, tokenData); err == nil && resolved != "" {
			tokenData.ProfileArn = resolved
			profileArn = resolved
		}
	}
	query := map[string]string{
		"origin":     "AI_EDITOR",
		"profileArn": profileArn,
	}
	body, err := k.makeRequest(ctx, kiroListModelsPath, tokenData, query)
	if err != nil {
		return nil, err
	}

	var result struct {
		Models []struct {
			ModelID             string  `json:"modelId"`
			ModelIDSnake        string  `json:"model_id"`
			ModelName           string  `json:"modelName"`
			ModelNameSnake      string  `json:"model_name"`
			Description         string  `json:"description"`
			RateMultiplier      float64 `json:"rateMultiplier"`
			RateMultiplierSnake float64 `json:"rate_multiplier"`
			RateUnit            string  `json:"rateUnit"`
			RateUnitSnake       string  `json:"rate_unit"`
			ContextWindowTokens int     `json:"context_window_tokens"`
			TokenLimits         *struct {
				MaxInputTokens int `json:"maxInputTokens"`
			} `json:"tokenLimits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("kiro: failed to parse models response: %w", err)
	}

	models := make([]*KiroModel, 0, len(result.Models))
	for _, model := range result.Models {
		maxInputTokens := model.ContextWindowTokens
		if model.TokenLimits != nil && model.TokenLimits.MaxInputTokens > 0 {
			maxInputTokens = model.TokenLimits.MaxInputTokens
		}
		models = append(models, &KiroModel{
			ModelID:        firstNonEmpty(model.ModelID, model.ModelIDSnake),
			ModelName:      firstNonEmpty(model.ModelName, model.ModelNameSnake),
			Description:    strings.TrimSpace(model.Description),
			RateMultiplier: firstNonZeroFloat(model.RateMultiplier, model.RateMultiplierSnake),
			RateUnit:       firstNonEmpty(model.RateUnit, model.RateUnitSnake),
			MaxInputTokens: maxInputTokens,
		})
	}
	return models, nil
}

func (k *KiroAuth) GetUsageLimits(ctx context.Context, tokenData *TokenData) (*KiroUsageInfo, error) {
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return nil, fmt.Errorf("kiro: access token is required to get usage limits")
	}

	profileArn := effectiveKiroProfileArn(tokenData)
	if profileArn == "" && shouldResolveKiroProfileArn(tokenData) {
		if resolved, err := k.ResolveProfileArn(ctx, tokenData); err == nil && resolved != "" {
			tokenData.ProfileArn = resolved
			profileArn = resolved
		}
	}
	query := map[string]string{
		"origin":       "AI_EDITOR",
		"resourceType": "AGENTIC_REQUEST",
	}
	if profileArn != "" {
		query["profileArn"] = profileArn
	} else {
		query["isEmailRequired"] = "true"
	}

	body, err := k.makeRequest(ctx, kiroGetUsageLimitsPath, tokenData, query)
	if err != nil {
		return nil, err
	}

	var usage KiroUsageInfo
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("kiro: failed to parse usage limits response: %w", err)
	}
	usage.normalize()
	return &usage, nil
}

func (k *KiroAuth) ResolveProfileArn(ctx context.Context, tokenData *TokenData) (string, error) {
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return "", fmt.Errorf("kiro: access token is required to resolve profile arn")
	}
	if !shouldResolveKiroProfileArn(tokenData) {
		return "", fmt.Errorf("kiro: profile arn is not used for %s auth", strings.TrimSpace(tokenData.AuthMethod))
	}
	if profileArn := effectiveKiroProfileArn(tokenData); profileArn != "" {
		return profileArn, nil
	}

	if profileArn, err := k.fetchProfileArnFromTarget(ctx, tokenData, kiroListProfilesTarget); err == nil && profileArn != "" {
		return profileArn, nil
	}
	if profileArn, err := k.fetchProfileArnFromTarget(ctx, tokenData, kiroListCustomizationsTarget); err == nil && profileArn != "" {
		return profileArn, nil
	}
	return "", fmt.Errorf("kiro: profile arn not found")
}

func (u *KiroUsageInfo) normalize() {
	if u == nil {
		return
	}
	if u.NextDateReset != nil && *u.NextDateReset > 0 {
		u.NextResetAt = time.UnixMilli(int64(*u.NextDateReset)).UTC().Format(time.RFC3339)
	}

	var totalLimit float64
	var totalCurrent float64
	var hasLimit bool
	u.Exhausted = len(u.UsageBreakdownList) > 0
	for i := range u.UsageBreakdownList {
		u.UsageBreakdownList[i].normalize()
		limit, okLimit := u.UsageBreakdownList[i].limitPrecision()
		current, okCurrent := u.UsageBreakdownList[i].currentPrecision()
		if okLimit {
			totalLimit += limit
			hasLimit = true
		}
		if okCurrent {
			totalCurrent += current
		}
		if !u.UsageBreakdownList[i].Exhausted {
			u.Exhausted = false
		}
		if u.UsageBreakdownList[i].FreeTrialInfo != nil {
			freeLimit, okFreeLimit := u.UsageBreakdownList[i].FreeTrialInfo.limitPrecision()
			freeCurrent, okFreeCurrent := u.UsageBreakdownList[i].FreeTrialInfo.currentPrecision()
			if okFreeLimit {
				totalLimit += freeLimit
				hasLimit = true
			}
			if okFreeCurrent {
				totalCurrent += freeCurrent
			}
			if !u.UsageBreakdownList[i].FreeTrialInfo.Exhausted {
				u.Exhausted = false
			}
		}
	}
	if hasLimit {
		remaining := totalLimit - totalCurrent
		if remaining < 0 {
			remaining = 0
		}
		percentage := 0.0
		if totalLimit > 0 {
			percentage = (totalCurrent / totalLimit) * 100
		}
		u.TotalUsageLimitWithPrecision = floatPtr(totalLimit)
		u.TotalCurrentUsageWithPrecision = floatPtr(totalCurrent)
		u.TotalRemainingUsageWithPrecision = floatPtr(remaining)
		u.TotalUsagePercentage = floatPtr(percentage)
	}
}

func (b *KiroUsageBreakdown) normalize() {
	if b == nil {
		return
	}
	limit, okLimit := b.limitPrecision()
	current, okCurrent := b.currentPrecision()
	if okLimit && okCurrent {
		remaining := limit - current
		if remaining < 0 {
			remaining = 0
		}
		percentage := 0.0
		if limit > 0 {
			percentage = (current / limit) * 100
		}
		b.RemainingWithPrecision = floatPtr(remaining)
		b.UsagePercentage = floatPtr(percentage)
		b.Exhausted = current >= limit
	}
	if b.FreeTrialInfo != nil {
		b.FreeTrialInfo.normalize()
	}
}

func (f *KiroFreeTrialInfo) normalize() {
	if f == nil {
		return
	}
	limit, okLimit := f.limitPrecision()
	current, okCurrent := f.currentPrecision()
	if okLimit && okCurrent {
		remaining := limit - current
		if remaining < 0 {
			remaining = 0
		}
		percentage := 0.0
		if limit > 0 {
			percentage = (current / limit) * 100
		}
		f.RemainingWithPrecision = floatPtr(remaining)
		f.UsagePercentage = floatPtr(percentage)
		f.Exhausted = current >= limit
	}
}

func (b KiroUsageBreakdown) limitPrecision() (float64, bool) {
	if b.UsageLimitWithPrecision != nil {
		return *b.UsageLimitWithPrecision, true
	}
	if b.UsageLimit != nil {
		return float64(*b.UsageLimit), true
	}
	return 0, false
}

func (b KiroUsageBreakdown) currentPrecision() (float64, bool) {
	if b.CurrentUsageWithPrecision != nil {
		return *b.CurrentUsageWithPrecision, true
	}
	if b.CurrentUsage != nil {
		return float64(*b.CurrentUsage), true
	}
	return 0, false
}

func (f KiroFreeTrialInfo) limitPrecision() (float64, bool) {
	if f.UsageLimitWithPrecision != nil {
		return *f.UsageLimitWithPrecision, true
	}
	if f.UsageLimit != nil {
		return float64(*f.UsageLimit), true
	}
	return 0, false
}

func (f KiroFreeTrialInfo) currentPrecision() (float64, bool) {
	if f.CurrentUsageWithPrecision != nil {
		return *f.CurrentUsageWithPrecision, true
	}
	if f.CurrentUsage != nil {
		return float64(*f.CurrentUsage), true
	}
	return 0, false
}

func floatPtr(value float64) *float64 {
	return &value
}

func firstNonZeroFloat(values ...float64) float64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func effectiveKiroProfileArn(tokenData *TokenData) string {
	if tokenData == nil || isKiroBuilderIDLikeAuth(tokenData) {
		return ""
	}
	return strings.TrimSpace(tokenData.ProfileArn)
}

func shouldResolveKiroProfileArn(tokenData *TokenData) bool {
	return tokenData != nil && !isKiroBuilderIDLikeAuth(tokenData)
}

func isKiroBuilderIDLikeAuth(tokenData *TokenData) bool {
	if tokenData == nil {
		return false
	}
	authMethod := strings.ToLower(strings.TrimSpace(tokenData.AuthMethod))
	if authMethod == kiroCLISocialAuthMethod || authMethod == "social" || authMethod == "kiro-social" {
		return false
	}
	if authMethod == "builder-id" || authMethod == "idc" || authMethod == "aws_sso_oidc" {
		return true
	}
	return strings.TrimSpace(tokenData.ClientID) != "" && strings.TrimSpace(tokenData.ClientSecret) != ""
}

func (k *KiroAuth) makeRequest(ctx context.Context, path string, tokenData *TokenData, queryParams map[string]string) ([]byte, error) {
	profileArn := ""
	if queryParams != nil {
		profileArn = queryParams["profileArn"]
	}
	target := kiroTargetForOperation(path)
	if target == "" {
		return nil, fmt.Errorf("kiro: unsupported operation %q", path)
	}
	payload := make(map[string]any, len(queryParams))
	for key, value := range queryParams {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			if key == "isEmailRequired" {
				payload[key] = strings.EqualFold(trimmed, "true")
				continue
			}
			payload[key] = trimmed
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to encode %s request: %w", strings.Trim(path, "/"), err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(k.endpointFor(profileArn), "/")+"/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tokenData.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", target)
	applyKiroCodeWhispererHeaders(req, "1")

	resp, err := k.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro: %s request failed: %w", strings.Trim(path, "/"), err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			Operation:  strings.Trim(path, "/"),
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return respBody, nil
}

func kiroTargetForOperation(path string) string {
	switch strings.Trim(path, "/") {
	case kiroListModelsPath:
		return kiroListModelsTarget
	case kiroGetUsageLimitsPath:
		return kiroGetUsageLimitsTarget
	default:
		return ""
	}
}

func applyKiroCodeWhispererHeaders(req *http.Request, maxAttempts string) {
	applyKiroCLIHeaders(req, maxAttempts)
	req.Header.Set("x-amz-user-agent", kiroCodeWhispererAmzAgent)
}

func (k *KiroAuth) fetchProfileArnFromTarget(ctx context.Context, tokenData *TokenData, target string) (string, error) {
	payload, err := json.Marshal(map[string]string{"origin": "AI_EDITOR"})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.endpointFor(""), strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("kiro: failed to create %s request: %w", target, err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tokenData.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	applyKiroCodeWhispererHeaders(req, "1")
	req.Header.Set("x-amz-target", target)

	resp, err := k.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("kiro: %s request failed: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kiro: failed to read %s response: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{
			Operation:  target,
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	var result struct {
		ProfileArn string `json:"profileArn"`
		Profiles   []struct {
			Arn        string `json:"arn"`
			ProfileArn string `json:"profileArn"`
		} `json:"profiles"`
		Customizations []struct {
			Arn        string `json:"arn"`
			ProfileArn string `json:"profileArn"`
		} `json:"customizations"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("kiro: failed to parse %s response: %w", target, err)
	}
	if profileArn := strings.TrimSpace(result.ProfileArn); profileArn != "" {
		return profileArn, nil
	}
	for _, profile := range result.Profiles {
		if profileArn := firstNonEmpty(profile.ProfileArn, profile.Arn); profileArn != "" {
			return profileArn, nil
		}
	}
	for _, customization := range result.Customizations {
		if profileArn := firstNonEmpty(customization.ProfileArn, customization.Arn); profileArn != "" {
			return profileArn, nil
		}
	}
	return "", fmt.Errorf("kiro: %s response did not include profile arn", target)
}

func (k *KiroAuth) endpointFor(profileArn string) string {
	if k != nil {
		endpoint := strings.TrimRight(strings.TrimSpace(k.endpoint), "/")
		if endpoint != "" && endpoint != kiroCodeWhispererEndpoint {
			return endpoint
		}
	}
	if region := ExtractRegionFromProfileArn(profileArn); region != "" {
		return "https://codewhisperer." + region + ".amazonaws.com"
	}
	if k != nil && strings.TrimSpace(k.endpoint) != "" {
		return strings.TrimRight(strings.TrimSpace(k.endpoint), "/")
	}
	return kiroCodeWhispererEndpoint
}

func (k *KiroAuth) client() *http.Client {
	if k != nil && k.httpClient != nil {
		return k.httpClient
	}
	return http.DefaultClient
}

func ExtractRegionFromProfileArn(profileArn string) string {
	parts := strings.Split(profileArn, ":")
	if len(parts) >= 4 && parts[0] == "arn" && parts[2] == "codewhisperer" && strings.Contains(parts[3], "-") {
		return strings.TrimSpace(parts[3])
	}
	return ""
}

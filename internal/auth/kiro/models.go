package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	Message                          string                `json:"message,omitempty"`
	Quotas                           map[string]any        `json:"quotas,omitempty"`
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

type KiroQuotaInfo struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetAt   *string `json:"resetAt"`
	Unlimited bool    `json:"unlimited"`
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
	FreeTrialExpiry           any      `json:"freeTrialExpiry,omitempty"`
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

// WithHTTPClient overrides the HTTP client used for management/quota requests.
// Callers in the runtime path inject a proxy-aware client built from
// helps.NewProxyAwareHTTPClient so the management quota requests honour
// per-account auth.ProxyURL, the global cfg.ProxyURL, the context
// RoundTripper, and the shared transport pool — matching the executor path.
//
// Returns the receiver to support fluent construction in the call site.
// A nil client is ignored so this is safe to call unconditionally.
func (k *KiroAuth) WithHTTPClient(client *http.Client) *KiroAuth {
	if k == nil || client == nil {
		return k
	}
	k.httpClient = client
	return k
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

	body, err := k.makeUsageLimitsRequest(ctx, tokenData, query, profileArn)
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

func (k *KiroAuth) makeUsageLimitsRequest(ctx context.Context, tokenData *TokenData, query map[string]string, profileArn string) ([]byte, error) {
	type attempt struct {
		name string
		run  func() ([]byte, error)
	}

	attempts := []attempt{
		{
			name: "codewhisperer-post",
			run: func() ([]byte, error) {
				return k.makeRequest(ctx, kiroGetUsageLimitsPath, tokenData, query)
			},
		},
		{
			name: "codewhisperer-get",
			run: func() ([]byte, error) {
				params := url.Values{}
				params.Set("isEmailRequired", "true")
				params.Set("origin", "AI_EDITOR")
				params.Set("resourceType", "AGENTIC_REQUEST")
				return k.makeUsageLimitsGET(ctx, strings.TrimRight(k.endpointFor(profileArn), "/")+"/"+kiroGetUsageLimitsPath, tokenData, params)
			},
		},
		{
			name: "q-get",
			run: func() ([]byte, error) {
				params := url.Values{}
				params.Set("origin", "AI_EDITOR")
				params.Set("resourceType", "AGENTIC_REQUEST")
				if strings.TrimSpace(profileArn) != "" {
					params.Set("profileArn", strings.TrimSpace(profileArn))
				} else {
					params.Set("isEmailRequired", "true")
				}
				return k.makeUsageLimitsGET(ctx, strings.TrimRight(k.qEndpointFor(profileArn), "/")+"/"+kiroGetUsageLimitsPath, tokenData, params)
			},
		},
	}

	errorsText := make([]string, 0, len(attempts))
	var authErr error
	for _, attempt := range attempts {
		body, err := attempt.run()
		if err == nil {
			return body, nil
		}
		errorsText = append(errorsText, attempt.name+":"+strings.TrimSpace(err.Error()))
		if authErr == nil && IsUnauthorizedStatusError(err) {
			authErr = err
		}
	}
	if authErr != nil {
		return nil, authErr
	}
	return nil, fmt.Errorf("kiro: get usage limits failed: %s", strings.Join(errorsText, "; "))
}

func (k *KiroAuth) makeUsageLimitsGET(ctx context.Context, endpoint string, tokenData *TokenData, params url.Values) ([]byte, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("kiro: get usage limits endpoint is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to parse get usage limits endpoint: %w", err)
	}
	query := parsed.Query()
	for key, values := range params {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				query.Add(key, value)
			}
		}
	}
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to create get usage limits request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tokenData.AccessToken))
	req.Header.Set("Accept", "application/json")
	applyKiroCodeWhispererHeaders(req, "1", tokenData)

	resp, err := k.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro: getUsageLimits GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to read getUsageLimits GET response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			Operation:  "getUsageLimits GET",
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}
	return body, nil
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
	u.normalizeQuotas()
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

func (u *KiroUsageInfo) normalizeQuotas() {
	if u == nil || len(u.UsageBreakdownList) == 0 {
		return
	}
	quotas := make(map[string]any, len(u.Quotas)+len(u.UsageBreakdownList)*2)
	for key, value := range u.Quotas {
		if strings.TrimSpace(key) != "" {
			quotas[key] = value
		}
	}
	for i := range u.UsageBreakdownList {
		breakdown := u.UsageBreakdownList[i]
		resourceType := strings.ToLower(strings.TrimSpace(breakdown.ResourceType))
		if resourceType == "" {
			resourceType = "unknown"
		}
		quotas[resourceType] = KiroQuotaInfo{
			Used:      kiroQuotaUsed(breakdown),
			Total:     kiroQuotaTotal(breakdown),
			Remaining: kiroQuotaRemaining(breakdown),
			ResetAt:   kiroResetAtPtr(breakdown.NextDateReset, u.NextDateReset, u.NextResetAt),
			Unlimited: false,
		}

		if breakdown.FreeTrialInfo == nil {
			continue
		}
		trial := breakdown.FreeTrialInfo
		quotas[resourceType+"_freetrial"] = KiroQuotaInfo{
			Used:      kiroQuotaUsed(*trial),
			Total:     kiroQuotaTotal(*trial),
			Remaining: kiroQuotaRemaining(*trial),
			ResetAt:   kiroResetAtPtr(trial.FreeTrialExpiry, breakdown.NextDateReset, u.NextDateReset, u.NextResetAt),
			Unlimited: false,
		}
	}
	u.Quotas = quotas
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

func kiroQuotaUsed(source interface{ currentPrecision() (float64, bool) }) float64 {
	value, ok := source.currentPrecision()
	if !ok || value < 0 {
		return 0
	}
	return value
}

func kiroQuotaTotal(source interface{ limitPrecision() (float64, bool) }) float64 {
	value, ok := source.limitPrecision()
	if !ok || value < 0 {
		return 0
	}
	return value
}

func kiroQuotaRemaining(source any) float64 {
	switch value := source.(type) {
	case KiroUsageBreakdown:
		if value.RemainingWithPrecision != nil {
			return nonNegativeFloat(*value.RemainingWithPrecision)
		}
		return kiroQuotaRemainingFromPrecision(value)
	case KiroFreeTrialInfo:
		if value.RemainingWithPrecision != nil {
			return nonNegativeFloat(*value.RemainingWithPrecision)
		}
		return kiroQuotaRemainingFromPrecision(value)
	default:
		return 0
	}
}

func kiroQuotaRemainingFromPrecision(source interface {
	limitPrecision() (float64, bool)
	currentPrecision() (float64, bool)
}) float64 {
	limit, okLimit := source.limitPrecision()
	current, okCurrent := source.currentPrecision()
	if !okLimit || !okCurrent {
		return 0
	}
	return nonNegativeFloat(limit - current)
}

func nonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func kiroResetAtPtr(values ...any) *string {
	for _, value := range values {
		if resetAt := kiroResetAt(value); resetAt != "" {
			return &resetAt
		}
	}
	return nil
}

func kiroResetAt(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return normalizeKiroResetAtString(v)
	case *string:
		if v == nil {
			return ""
		}
		return normalizeKiroResetAtString(*v)
	case float64:
		return kiroResetAtFromUnix(v)
	case *float64:
		if v == nil {
			return ""
		}
		return kiroResetAtFromUnix(*v)
	case int:
		return kiroResetAtFromUnix(float64(v))
	case int64:
		return kiroResetAtFromUnix(float64(v))
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return ""
		}
		return kiroResetAtFromUnix(parsed)
	case time.Time:
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

func normalizeKiroResetAtString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return kiroResetAtFromUnix(parsed)
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	return trimmed
}

func kiroResetAtFromUnix(value float64) string {
	if value <= 0 {
		return ""
	}
	if value < 1e12 {
		return time.Unix(0, int64(value*float64(time.Second))).UTC().Format(time.RFC3339)
	}
	return time.Unix(0, int64(value*float64(time.Millisecond))).UTC().Format(time.RFC3339)
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
	applyKiroCodeWhispererHeaders(req, "1", tokenData)

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

func applyKiroCodeWhispererHeaders(req *http.Request, maxAttempts string, tokenData *TokenData) {
	applyKiroCLIHeaders(req, maxAttempts)
	if isKiroModelIDEIdentity(tokenData) {
		machineID := MachineIDFromTokenData(tokenData)
		req.Header.Set("User-Agent", kiroModelIDEUserAgent(machineID))
		req.Header.Set("x-amz-user-agent", kiroModelIDEAmzUserAgent(machineID))
		return
	}
	req.Header.Set("x-amz-user-agent", kiroCodeWhispererAmzAgent)
}

func isKiroModelIDEIdentity(tokenData *TokenData) bool {
	if tokenData == nil {
		return false
	}
	authMethod := strings.ToLower(strings.TrimSpace(tokenData.AuthMethod))
	provider := strings.ToLower(strings.TrimSpace(tokenData.Provider))
	if authMethod == kiroCLISocialAuthMethod || authMethod == "social" || authMethod == "kiro-social" {
		return true
	}
	switch provider {
	case "google", "github", "gitlab", "kiro-cli", "kiro-social", "social":
		return true
	default:
		return NormalizeKiroMachineID(tokenData.MachineID) != "" && !isKiroBuilderIDLikeAuth(tokenData)
	}
}

func kiroModelIDEUserAgent(machineID string) string {
	suffix := "KiroIDE-0.12.155"
	if machineID = NormalizeKiroMachineID(machineID); machineID != "" {
		suffix += "-" + machineID
	}
	return "aws-sdk-js/1.0.34 ua/2.1 os/linux#6.0.0 lang/js md/nodejs#22.22.0 api/codewhispererstreaming#1.0.34 m/E " + suffix
}

func kiroModelIDEAmzUserAgent(machineID string) string {
	suffix := "KiroIDE-0.12.155"
	if machineID = NormalizeKiroMachineID(machineID); machineID != "" {
		suffix = "KiroIDE 0.12.155 " + machineID
	}
	return "aws-sdk-js/1.0.34 " + suffix
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
	applyKiroCodeWhispererHeaders(req, "1", tokenData)
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

func (k *KiroAuth) qEndpointFor(profileArn string) string {
	if k != nil {
		endpoint := strings.TrimRight(strings.TrimSpace(k.endpoint), "/")
		if endpoint != "" && endpoint != kiroCodeWhispererEndpoint {
			return endpoint
		}
	}
	region := ExtractRegionFromProfileArn(profileArn)
	if region == "" {
		region = DefaultRegion
	}
	return "https://q." + region + ".amazonaws.com"
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

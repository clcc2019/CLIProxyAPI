package kiro

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestParseTokenDataAcceptsCamelAndSnakeCase(t *testing.T) {
	token, err := ParseTokenData([]byte(`{
		"access_token":"access",
		"refreshToken":"refresh",
		"profile_arn":"arn:aws:codewhisperer:us-east-1:123:profile/test",
		"expiresAt":"2026-01-01T00:00:00Z",
		"auth_method":"idc",
		"clientId":"client",
		"client_secret":"secret"
	}`))
	if err != nil {
		t.Fatalf("ParseTokenData() error = %v", err)
	}
	if token.AccessToken != "access" || token.RefreshToken != "refresh" || token.ClientID != "client" || token.ClientSecret != "secret" {
		t.Fatalf("unexpected token parse result: %+v", token)
	}
}

func TestLoadKiroCLITokenFromPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.sqlite3")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`create table auth_kv (key text primary key, value text)`); err != nil {
		t.Fatalf("create auth_kv: %v", err)
	}
	if _, err := db.Exec(`create table state (key text primary key, value blob)`); err != nil {
		t.Fatalf("create state: %v", err)
	}
	tokenJSON := `{
		"access_token":"access-token",
		"refresh_token":"refresh-token",
		"provider":"google",
		"expires_at":"2026-01-01T00:00:00Z"
	}`
	if _, err := db.Exec(`insert into auth_kv (key, value) values (?, ?)`, KiroCLIAuthKVTokenKey, tokenJSON); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	profileJSON := `{"arn":"arn:aws:codewhisperer:us-east-1:123:profile/social"}`
	if _, err := db.Exec(`insert into state (key, value) values (?, ?)`, KiroCLIProfileStateKey, profileJSON); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	token, err := LoadKiroCLITokenFromPath(dbPath)
	if err != nil {
		t.Fatalf("LoadKiroCLITokenFromPath() error = %v", err)
	}
	if token.AccessToken != "access-token" || token.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected token credentials: %+v", token)
	}
	if token.ProfileArn != "arn:aws:codewhisperer:us-east-1:123:profile/social" {
		t.Fatalf("profile arn = %q", token.ProfileArn)
	}
	if token.AuthMethod != kiroCLISocialAuthMethod || token.Provider != "google" {
		t.Fatalf("unexpected token metadata: %+v", token)
	}
}

func TestSSOOIDCClientBuilderIDFlowRequests(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		requireKiroRequest(t, r)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode register payload: %v", err)
		}
		if payload["clientName"] != "Kiro IDE" || payload["clientType"] != "public" {
			t.Fatalf("unexpected register payload: %+v", payload)
		}
		writeJSON(t, w, RegisterClientResponse{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
		})
	})

	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		requireKiroRequest(t, r)
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode device payload: %v", err)
		}
		if payload["clientId"] != "client-id" || payload["clientSecret"] != "client-secret" || payload["startUrl"] != BuilderIDStartURL {
			t.Fatalf("unexpected device payload: %+v", payload)
		}
		writeJSON(t, w, StartDeviceAuthResponse{
			DeviceCode:              "device-code",
			UserCode:                "ABCD-EFGH",
			VerificationURI:         "https://example.test/verify",
			VerificationURIComplete: "https://example.test/verify?user_code=ABCD-EFGH",
			ExpiresIn:               600,
			Interval:                5,
		})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		requireKiroRequest(t, r)
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode token payload: %v", err)
		}
		if payload["clientId"] != "client-id" || payload["clientSecret"] != "client-secret" || payload["deviceCode"] != "device-code" {
			t.Fatalf("unexpected token payload: %+v", payload)
		}
		writeJSON(t, w, CreateTokenResponse{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresIn:    3600,
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &SSOOIDCClient{httpClient: server.Client(), endpoint: server.URL}
	regResp, err := client.RegisterClient(context.Background())
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	authResp, err := client.StartDeviceAuthorization(context.Background(), regResp.ClientID, regResp.ClientSecret)
	if err != nil {
		t.Fatalf("StartDeviceAuthorization() error = %v", err)
	}
	tokenResp, err := client.CreateToken(context.Background(), regResp.ClientID, regResp.ClientSecret, authResp.DeviceCode)
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	if tokenResp.AccessToken != "access-token" || tokenResp.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected token response: %+v", tokenResp)
	}
}

func TestSSOOIDCClientCreateTokenPollingErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "pending", body: `{"error":"authorization_pending"}`, wantErr: ErrAuthorizationPending},
		{name: "slow down", body: `{"error":"slow_down"}`, wantErr: ErrSlowDown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireKiroRequest(t, r)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := &SSOOIDCClient{httpClient: server.Client(), endpoint: server.URL}
			_, err := client.CreateToken(context.Background(), "client-id", "client-secret", "device-code")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateToken() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSSOOIDCClientRefreshTokenUsesAWSRefreshGrant(t *testing.T) {
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireKiroRequest(t, r)
		if r.URL.Path != "/token" {
			t.Fatalf("path = %q, want /token", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode refresh payload: %v", err)
		}
		writeJSON(t, w, CreateTokenResponse{
			AccessToken:  "new-access-token",
			RefreshToken: "new-refresh-token",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()

	client := &SSOOIDCClient{httpClient: server.Client(), endpoint: server.URL}
	tokenResp, err := client.RefreshToken(context.Background(), "client-id", "client-secret", "old-refresh-token")
	if err != nil {
		t.Fatalf("RefreshToken() error = %v", err)
	}
	if gotPayload["grantType"] != "refresh_token" {
		t.Fatalf("grantType = %q, want refresh_token (payload=%v)", gotPayload["grantType"], gotPayload)
	}
	if gotPayload["clientId"] != "client-id" || gotPayload["clientSecret"] != "client-secret" || gotPayload["refreshToken"] != "old-refresh-token" {
		t.Fatalf("unexpected refresh payload: %v", gotPayload)
	}
	if _, ok := gotPayload["deviceCode"]; ok {
		t.Fatalf("refresh payload should not include deviceCode: %v", gotPayload)
	}
	if tokenResp.AccessToken != "new-access-token" || tokenResp.RefreshToken != "new-refresh-token" {
		t.Fatalf("unexpected refresh response: %+v", tokenResp)
	}
}

func TestSSOOIDCClientRefreshTokenHTTPErrorPreservesStatusBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireKiroRequest(t, r)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
	}))
	defer server.Close()

	client := &SSOOIDCClient{httpClient: server.Client(), endpoint: server.URL}
	_, err := client.RefreshToken(context.Background(), "client-id", "client-secret", "old-refresh-token")
	if err == nil {
		t.Fatal("RefreshToken() expected error, got nil")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("RefreshToken() error = %T %v, want StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusBadRequest || statusErr.Body == "" {
		t.Fatalf("unexpected status error: %+v", statusErr)
	}
}

func TestKiroAuthListAvailableModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := requireKiroServiceRequest(t, r, kiroListModelsTarget)
		if body["origin"] != "AI_EDITOR" {
			t.Fatalf("origin = %q", body["origin"])
		}
		if body["profileArn"] != "arn:aws:codewhisperer:us-west-2:123:profile/test" {
			t.Fatalf("profileArn = %q", body["profileArn"])
		}
		writeJSON(t, w, map[string]any{
			"models": []map[string]any{
				{
					"modelId":        "claude-sonnet-4.7",
					"modelName":      "Claude Sonnet 4.7",
					"description":    "latest sonnet",
					"rateMultiplier": 1.3,
					"rateUnit":       "credit",
					"tokenLimits": map[string]any{
						"maxInputTokens": 262144,
					},
				},
			},
		})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	models, err := auth.ListAvailableModels(context.Background(), &TokenData{
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-west-2:123:profile/test",
	})
	if err != nil {
		t.Fatalf("ListAvailableModels() error = %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	if models[0].ModelID != "claude-sonnet-4.7" || models[0].MaxInputTokens != 262144 {
		t.Fatalf("unexpected model: %+v", models[0])
	}
}

func TestKiroAuthListAvailableModelsParsesKiroCLIShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireKiroServiceRequest(t, r, kiroListModelsTarget)
		writeJSON(t, w, map[string]any{
			"models": []map[string]any{
				{
					"model_id":              "minimax-m2.5",
					"model_name":            "minimax-m2.5",
					"description":           "The MiniMax M2.5 model",
					"context_window_tokens": 196000,
					"rate_multiplier":       0.25,
					"rate_unit":             "Credit",
				},
			},
		})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	models, err := auth.ListAvailableModels(context.Background(), &TokenData{
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-west-2:123:profile/test",
	})
	if err != nil {
		t.Fatalf("ListAvailableModels() error = %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	if models[0].ModelID != "minimax-m2.5" || models[0].ModelName != "minimax-m2.5" {
		t.Fatalf("unexpected model identity: %+v", models[0])
	}
	if models[0].MaxInputTokens != 196000 {
		t.Fatalf("max input tokens = %d, want 196000", models[0].MaxInputTokens)
	}
	if models[0].RateMultiplier != 0.25 || models[0].RateUnit != "Credit" {
		t.Fatalf("unexpected rate info: %+v", models[0])
	}
}

func TestKiroAuthListAvailableModelsUnauthorizedStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, map[string]any{"message": "The bearer token included in the request is invalid."})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	_, err := auth.ListAvailableModels(context.Background(), &TokenData{
		AccessToken: "expired-token",
		ProfileArn:  "arn:aws:codewhisperer:us-west-2:123:profile/test",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsUnauthorizedStatusError(err) {
		t.Fatalf("expected unauthorized status error, got %T: %v", err, err)
	}
}

func TestKiroAuthGetUsageLimits(t *testing.T) {
	var sawUsageRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := requireKiroServiceRequest(t, r, kiroGetUsageLimitsTarget)
		if body["origin"] != "AI_EDITOR" {
			t.Fatalf("origin = %q", body["origin"])
		}
		if body["resourceType"] != "AGENTIC_REQUEST" {
			t.Fatalf("resourceType = %q", body["resourceType"])
		}
		if body["profileArn"] != "arn:aws:codewhisperer:us-west-2:123:profile/test" {
			t.Fatalf("profileArn = %q", body["profileArn"])
		}
		if _, ok := body["isEmailRequired"]; ok {
			t.Fatalf("isEmailRequired should be omitted when profileArn exists")
		}
		sawUsageRequest = true
		writeJSON(t, w, map[string]any{
			"subscriptionInfo": map[string]any{
				"subscriptionTitle": "Kiro Pro",
				"type":              "PRO",
			},
			"usageBreakdownList": []map[string]any{
				{
					"resourceType":              "AGENTIC_REQUEST",
					"displayName":               "Agentic requests",
					"currentUsageWithPrecision": 10.5,
					"usageLimitWithPrecision":   100,
					"freeTrialInfo": map[string]any{
						"freeTrialStatus":           "ACTIVE",
						"currentUsageWithPrecision": 1,
						"usageLimitWithPrecision":   5,
					},
				},
			},
			"nextDateReset": 1767225600000,
		})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	usage, err := auth.GetUsageLimits(context.Background(), &TokenData{
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-west-2:123:profile/test",
	})
	if err != nil {
		t.Fatalf("GetUsageLimits() error = %v", err)
	}
	if !sawUsageRequest {
		t.Fatal("expected usage request")
	}
	if usage.SubscriptionInfo == nil || usage.SubscriptionInfo.SubscriptionTitle != "Kiro Pro" {
		t.Fatalf("unexpected subscription info: %+v", usage.SubscriptionInfo)
	}
	if usage.TotalRemainingUsageWithPrecision == nil || *usage.TotalRemainingUsageWithPrecision != 93.5 {
		t.Fatalf("total remaining = %#v, want 93.5", usage.TotalRemainingUsageWithPrecision)
	}
	if len(usage.UsageBreakdownList) != 1 || usage.UsageBreakdownList[0].RemainingWithPrecision == nil || *usage.UsageBreakdownList[0].RemainingWithPrecision != 89.5 {
		t.Fatalf("unexpected usage breakdown: %+v", usage.UsageBreakdownList)
	}
	if usage.NextResetAt == "" {
		t.Fatal("expected next reset timestamp")
	}
}

func TestKiroAuthGetUsageLimitsRequestsEmailWhenProfileArnMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("x-amz-target") {
		case kiroListProfilesTarget, kiroListCustomizationsTarget:
			http.Error(w, "no profile", http.StatusNotFound)
			return
		case kiroGetUsageLimitsTarget:
			body := requireKiroServiceRequest(t, r, kiroGetUsageLimitsTarget)
			if _, ok := body["profileArn"]; ok {
				t.Fatalf("profileArn should be omitted")
			}
			if body["isEmailRequired"] != true {
				t.Fatalf("isEmailRequired = %v, want true", body["isEmailRequired"])
			}
		default:
			t.Fatalf("x-amz-target = %q", r.Header.Get("x-amz-target"))
		}
		writeJSON(t, w, map[string]any{
			"userInfo": map[string]any{
				"email": "user@example.com",
			},
		})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	usage, err := auth.GetUsageLimits(context.Background(), &TokenData{AccessToken: "access-token"})
	if err != nil {
		t.Fatalf("GetUsageLimits() error = %v", err)
	}
	if usage.UserInfo == nil || usage.UserInfo.Email != "user@example.com" {
		t.Fatalf("unexpected user info: %+v", usage.UserInfo)
	}
}

func TestKiroAuthGetUsageLimitsIgnoresProfileArnForBuilderID(t *testing.T) {
	var sawUsageRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := requireKiroServiceRequest(t, r, kiroGetUsageLimitsTarget)
		if _, ok := body["profileArn"]; ok {
			t.Fatalf("profileArn should be omitted for builder-id auth")
		}
		if body["isEmailRequired"] != true {
			t.Fatalf("isEmailRequired = %v, want true", body["isEmailRequired"])
		}
		sawUsageRequest = true
		writeJSON(t, w, map[string]any{
			"subscriptionInfo": map[string]any{
				"subscriptionTitle": "KIRO FREE",
				"type":              "FREE",
			},
			"usageBreakdownList": []map[string]any{
				{
					"resourceType":              "AGENTIC_REQUEST",
					"usageLimitWithPrecision":   50,
					"currentUsageWithPrecision": 1,
				},
			},
		})
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	usage, err := auth.GetUsageLimits(context.Background(), &TokenData{
		AccessToken:  "access-token",
		ProfileArn:   "arn:aws:codewhisperer:us-east-1:123:profile/social",
		AuthMethod:   "builder-id",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
	})
	if err != nil {
		t.Fatalf("GetUsageLimits() error = %v", err)
	}
	if !sawUsageRequest {
		t.Fatal("expected usage request")
	}
	if usage.SubscriptionInfo == nil || usage.SubscriptionInfo.SubscriptionTitle != "KIRO FREE" {
		t.Fatalf("unexpected subscription info: %+v", usage.SubscriptionInfo)
	}
}

func TestKiroAuthGetUsageLimitsResolvesProfileArn(t *testing.T) {
	const profileArn = "arn:aws:codewhisperer:us-east-1:123:profile/pro"
	var sawListProfiles bool
	var sawUsageRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("x-amz-target") {
		case kiroListProfilesTarget:
			requireKiroServiceRequest(t, r, kiroListProfilesTarget)
			sawListProfiles = true
			writeJSON(t, w, map[string]any{
				"profiles": []map[string]any{
					{"arn": profileArn},
				},
			})
		case kiroGetUsageLimitsTarget:
			body := requireKiroServiceRequest(t, r, kiroGetUsageLimitsTarget)
			if body["profileArn"] != profileArn {
				t.Fatalf("profileArn = %q", body["profileArn"])
			}
			if _, ok := body["isEmailRequired"]; ok {
				t.Fatalf("isEmailRequired should be omitted after profile resolution")
			}
			sawUsageRequest = true
			writeJSON(t, w, map[string]any{
				"subscriptionInfo": map[string]any{
					"subscriptionTitle": "KIRO PRO",
					"type":              "Q_DEVELOPER_STANDALONE_PRO",
				},
				"usageBreakdownList": []map[string]any{
					{
						"resourceType":              "CREDIT",
						"displayName":               "Credit",
						"usageLimitWithPrecision":   1000,
						"currentUsageWithPrecision": 98.51,
					},
				},
			})
		default:
			t.Fatalf("x-amz-target = %q", r.Header.Get("x-amz-target"))
		}
	}))
	defer server.Close()

	auth := &KiroAuth{httpClient: server.Client(), endpoint: server.URL}
	tokenData := &TokenData{AccessToken: "access-token"}
	usage, err := auth.GetUsageLimits(context.Background(), tokenData)
	if err != nil {
		t.Fatalf("GetUsageLimits() error = %v", err)
	}
	if !sawListProfiles || !sawUsageRequest {
		t.Fatalf("expected listProfiles=%t usage=%t", sawListProfiles, sawUsageRequest)
	}
	if tokenData.ProfileArn != profileArn {
		t.Fatalf("token profile arn = %q", tokenData.ProfileArn)
	}
	if usage.SubscriptionInfo == nil || usage.SubscriptionInfo.SubscriptionTitle != "KIRO PRO" {
		t.Fatalf("unexpected subscription info: %+v", usage.SubscriptionInfo)
	}
	if usage.TotalRemainingUsageWithPrecision == nil || *usage.TotalRemainingUsageWithPrecision != 901.49 {
		t.Fatalf("total remaining = %#v, want 901.49", usage.TotalRemainingUsageWithPrecision)
	}
}

func requireKiroServiceRequest(t *testing.T, r *http.Request, target string) map[string]any {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", r.Method)
	}
	if r.URL.Path != "/" {
		t.Fatalf("path = %q, want /", r.URL.Path)
	}
	if r.Header.Get("x-amz-target") != target {
		t.Fatalf("x-amz-target = %q, want %q", r.Header.Get("x-amz-target"), target)
	}
	if r.Header.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
	}
	if r.Header.Get("x-amz-user-agent") == "" || r.Header.Get("Amz-Sdk-Invocation-Id") == "" {
		t.Fatalf("missing AWS SDK headers: %+v", r.Header)
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func requireKiroRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", r.Method)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
	}
	if r.Header.Get("User-Agent") != kiroUserAgent {
		t.Fatalf("user-agent = %q", r.Header.Get("User-Agent"))
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

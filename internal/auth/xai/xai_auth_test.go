package xai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildAuthorizeURLIncludesXAIRequiredParameters(t *testing.T) {
	authURL, err := BuildAuthorizeURL(AuthorizeURLParams{
		AuthorizationEndpoint: "https://auth.x.ai/oauth/authorize",
		RedirectURI:           "http://127.0.0.1:56121/callback",
		CodeChallenge:         "challenge",
		State:                 "state-123",
		Nonce:                 "nonce-123",
	})
	if err != nil {
		t.Fatalf("BuildAuthorizeURL() error = %v", err)
	}

	parsed, errParse := url.Parse(authURL)
	if errParse != nil {
		t.Fatalf("parse authorize URL: %v", errParse)
	}
	if parsed.Scheme != "https" || parsed.Host != "auth.x.ai" || parsed.Path != "/oauth/authorize" {
		t.Fatalf("authorize URL endpoint = %s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
	}

	query := parsed.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             ClientID,
		"redirect_uri":          "http://127.0.0.1:56121/callback",
		"scope":                 Scope,
		"code_challenge":        "challenge",
		"code_challenge_method": "S256",
		"state":                 "state-123",
		"nonce":                 "nonce-123",
		"plan":                  "generic",
		"referrer":              "cli-proxy-api",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestRefreshTokensInvalidGrantIsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
	}))
	defer server.Close()

	auth := NewXAIAuth(nil)
	_, err := auth.RefreshTokens(context.Background(), "old-refresh", server.URL)
	if err == nil {
		t.Fatal("expected invalid_grant refresh to fail")
	}
	var permanent interface{ IsPermanentAuthError() bool }
	if !errors.As(err, &permanent) || !permanent.IsPermanentAuthError() {
		t.Fatalf("expected permanent auth error, got %T: %v", err, err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %T: %v", http.StatusBadRequest, err, err)
	}
}

func TestTokenRequestErrorIsPermanentAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  *tokenRequestError
		want bool
	}{
		{
			name: "mixed case invalid grant",
			err:  &tokenRequestError{oauthError: " Invalid_Grant "},
			want: true,
		},
		{
			name: "mixed case invalid client",
			err:  &tokenRequestError{oauthError: "\tINVALID_CLIENT\n"},
			want: true,
		},
		{
			name: "mixed case unauthorized client",
			err:  &tokenRequestError{oauthError: "unauthorized_CLIENT"},
			want: true,
		},
		{
			name: "unauthorized status fallback",
			err:  &tokenRequestError{statusCode: http.StatusUnauthorized, oauthError: "temporarily_unavailable"},
			want: true,
		},
		{
			name: "non permanent oauth error",
			err:  &tokenRequestError{statusCode: http.StatusBadRequest, oauthError: "temporarily_unavailable"},
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsPermanentAuthError(); got != tt.want {
				t.Fatalf("IsPermanentAuthError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateOAuthEndpointRejectsNonXAIOrigin(t *testing.T) {
	if _, err := ValidateOAuthEndpoint("https://auth.x.ai/oauth/token", "token_endpoint"); err != nil {
		t.Fatalf("ValidateOAuthEndpoint(xai) error = %v", err)
	}
	if _, err := ValidateOAuthEndpoint("http://auth.x.ai/oauth/token", "token_endpoint"); err == nil {
		t.Fatal("expected non-HTTPS endpoint to be rejected")
	}
	if _, err := ValidateOAuthEndpoint("https://evil.example/oauth/token", "token_endpoint"); err == nil {
		t.Fatal("expected non-xAI endpoint to be rejected")
	}
}

func TestRefreshTokensPostsClientIDAndRefreshToken(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
			t.Fatalf("Content-Type = %q, want form", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	auth := NewXAIAuth(nil)
	tokenData, err := auth.RefreshTokens(context.Background(), "old-refresh", server.URL)
	if err != nil {
		t.Fatalf("RefreshTokens() error = %v", err)
	}
	if tokenData.AccessToken != "new-access" {
		t.Fatalf("access token = %q, want new-access", tokenData.AccessToken)
	}
	if gotForm.Get("grant_type") != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", gotForm.Get("grant_type"))
	}
	if gotForm.Get("client_id") != ClientID {
		t.Fatalf("client_id = %q, want %q", gotForm.Get("client_id"), ClientID)
	}
	if gotForm.Get("refresh_token") != "old-refresh" {
		t.Fatalf("refresh_token = %q, want old-refresh", gotForm.Get("refresh_token"))
	}
}

func BenchmarkTokenRequestErrorIsPermanentAuthError(b *testing.B) {
	err := &tokenRequestError{
		statusCode: http.StatusBadRequest,
		oauthError: " Invalid_Grant ",
	}

	for b.Loop() {
		if !err.IsPermanentAuthError() {
			b.Fatal("expected permanent auth error")
		}
	}
}

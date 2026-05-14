package kiro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestKiroSocialOAuthBuildLoginURL(t *testing.T) {
	client := NewSocialOAuthClientWithHTTPClient("https://example.test", nil)
	redirectURI := OAuthRedirectURI(DefaultOAuthCallbackPort)
	if redirectURI != "http://localhost:3128/oauth/callback" {
		t.Fatalf("redirectURI = %q, want local callback path", redirectURI)
	}
	loginURL, err := client.BuildLoginURL("github", redirectURI, "state-123", "challenge-123")
	if err != nil {
		t.Fatalf("BuildLoginURL() error = %v", err)
	}
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse login URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "example.test" || parsed.Path != "/login" {
		t.Fatalf("login URL = %s", loginURL)
	}
	q := parsed.Query()
	if q.Get("idp") != "Github" {
		t.Fatalf("idp = %q, want Github", q.Get("idp"))
	}
	if q.Get("redirect_uri") != redirectURI {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "state-123" || q.Get("code_challenge") != "challenge-123" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected query: %v", q)
	}
	if q.Get("redirect_from") != "" {
		t.Fatalf("redirect_from = %q, want empty", q.Get("redirect_from"))
	}
}

func TestKiroSocialOAuthBuildLoginURLOfficialSigninParams(t *testing.T) {
	client := NewSocialOAuthClientWithHTTPClient("https://example.test", nil)
	loginURL, err := client.BuildLoginURLWithOptions("google", OAuthRedirectURI(DefaultOAuthCallbackPort), "state-123", "challenge-123", SocialLoginURLOptions{
		ForceReauth: true,
	})
	if err != nil {
		t.Fatalf("BuildLoginURLWithOptions() error = %v", err)
	}
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse login URL: %v", err)
	}
	q := parsed.Query()
	if parsed.Path != "/login" {
		t.Fatalf("path = %q, want /login", parsed.Path)
	}
	if q.Get("idp") != "Google" {
		t.Fatalf("idp = %q, want Google", q.Get("idp"))
	}
	if q.Get("prompt") != "select_account" {
		t.Fatalf("prompt = %q, want select_account", q.Get("prompt"))
	}
	if q.Get("max_age") != "" {
		t.Fatalf("max_age = %q, want empty", q.Get("max_age"))
	}
}

func TestKiroSocialOAuthExchangeCode(t *testing.T) {
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %q, want /oauth/token", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode token payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"access-token","refreshToken":"refresh-token","profileArn":"profile","expiresIn":3600}`))
	}))
	defer server.Close()

	client := NewSocialOAuthClientWithHTTPClient(server.URL, server.Client())
	redirectURI := OAuthRedirectURI(DefaultOAuthCallbackPort)
	token, err := client.ExchangeCode(context.Background(), "auth-code", redirectURI, "verifier")
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if gotPayload["code"] != "auth-code" || gotPayload["code_verifier"] != "verifier" || gotPayload["redirect_uri"] != redirectURI {
		t.Fatalf("unexpected payload: %v", gotPayload)
	}
	if token.AccessToken != "access-token" || token.RefreshToken != "refresh-token" || token.ProfileArn != "profile" || token.ExpiresIn != 3600 {
		t.Fatalf("unexpected token: %+v", token)
	}
}

func TestKiroSocialOAuthExchangeCodeAcceptsSnakeCaseTokenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-token","refresh_token":"refresh-token","profile_arn":"profile","expires_in":3600}`))
	}))
	defer server.Close()

	client := NewSocialOAuthClientWithHTTPClient(server.URL, server.Client())
	token, err := client.ExchangeCode(context.Background(), "auth-code", OAuthRedirectURI(DefaultOAuthCallbackPort), "verifier")
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if token.AccessToken != "access-token" || token.RefreshToken != "refresh-token" || token.ProfileArn != "profile" || token.ExpiresIn != 3600 {
		t.Fatalf("unexpected token: %+v", token)
	}
}

func TestKiroSocialOAuthExchangeCodeRejectsMissingRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"access-token","expiresIn":3600}`))
	}))
	defer server.Close()

	client := NewSocialOAuthClientWithHTTPClient(server.URL, server.Client())
	if _, err := client.ExchangeCode(context.Background(), "auth-code", OAuthRedirectURI(DefaultOAuthCallbackPort), "verifier"); err == nil {
		t.Fatal("ExchangeCode() expected error, got nil")
	}
}

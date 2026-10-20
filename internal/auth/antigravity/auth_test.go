package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchProjectIDForbiddenPreservesStatusAndMessage(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"Verify your account to continue."}}`)),
		}, nil
	})})

	_, errFetch := auth.FetchProjectID(context.Background(), "access-token")
	if errFetch == nil {
		t.Fatal("FetchProjectID returned nil error for upstream 403")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(errFetch, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("FetchProjectID error status = %v, want %d", errFetch, http.StatusForbidden)
	}
	if !strings.Contains(errFetch.Error(), "Verify your account to continue.") {
		t.Fatalf("FetchProjectID error = %q, want upstream verification message", errFetch)
	}
}

func TestFetchProjectIDUsesShortLoadCodeAssistUserAgent(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("User-Agent"); strings.Contains(got, "google-api-nodejs-client/") {
			t.Fatalf("loadCodeAssist User-Agent = %q, want short UA", got)
		}
		return jsonResponse(`{"cloudaicompanionProject":"test-project"}`), nil
	})})

	projectID, errFetch := auth.FetchProjectID(context.Background(), "access-token")
	if errFetch != nil {
		t.Fatalf("FetchProjectID error: %v", errFetch)
	}
	if projectID != "test-project" {
		t.Fatalf("projectID = %q, want test-project", projectID)
	}
}

func TestOnboardUserUsesLongControlPlaneUserAgent(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("User-Agent"); !strings.Contains(got, "google-api-nodejs-client/10.3.0") {
			t.Fatalf("onboardUser User-Agent = %q, want node client suffix", got)
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "gl-node/22.21.1" {
			t.Fatalf("X-Goog-Api-Client = %q, want gl-node/22.21.1", got)
		}
		return jsonResponse(`{"done":true,"response":{"cloudaicompanionProject":{"id":"test-project"}}}`), nil
	})})

	projectID, errOnboard := auth.OnboardUser(context.Background(), "access-token", "free-tier")
	if errOnboard != nil {
		t.Fatalf("OnboardUser error: %v", errOnboard)
	}
	if projectID != "test-project" {
		t.Fatalf("projectID = %q, want test-project", projectID)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

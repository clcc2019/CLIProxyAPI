package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGetAntigravityUsageFetchesAndCachesCredits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-test","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "antigravity-test.json")
	authFile := map[string]any{
		"type":         "antigravity",
		"access_token": "access-token",
		"base_url":     upstream.URL,
	}
	data, err := json.Marshal(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.GET("/auth-files/antigravity-usage", h.GetAntigravityUsage)

	request := func(query string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/auth-files/antigravity-usage?name=antigravity-test.json&"+query, nil)
		router.ServeHTTP(recorder, req)
		return recorder
	}

	first := request("antigravity_usage=refresh")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatal(err)
	}
	credits, ok := firstPayload["credits"].(map[string]any)
	if !ok {
		t.Fatalf("credits payload = %#v", firstPayload["credits"])
	}
	if got := credits["credit_amount"]; got != float64(25000) {
		t.Fatalf("credit_amount = %#v", got)
	}
	if got := credits["available"]; got != true {
		t.Fatalf("available = %#v", got)
	}

	second := request("antigravity_usage=cache")
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	if upstreamRequests != 1 {
		t.Fatalf("upstream requests = %d, want cached second request", upstreamRequests)
	}
}

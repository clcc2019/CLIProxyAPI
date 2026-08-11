package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestForwardResponsesStreamExposesOnlyClientErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "bad request", status: http.StatusBadRequest, body: "invalid input", want: true},
		{name: "cyber policy behind gateway", status: http.StatusBadGateway, body: `{"error":{"type":"invalid_request","code":"cyber_policy"}}`, want: true},
		{name: "conflict", status: http.StatusConflict, body: "conflict", want: true},
		{name: "authentication", status: http.StatusUnauthorized, body: "invalid credential"},
		{name: "payment", status: http.StatusPaymentRequired, body: "insufficient credits"},
		{name: "quota", status: http.StatusTooManyRequests, body: "quota"},
		{name: "transport", status: http.StatusInternalServerError, body: "unexpected EOF"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			flusher := c.Writer.(http.Flusher)
			data := make(chan []byte)
			errs := make(chan *interfaces.ErrorMessage, 1)
			errs <- &interfaces.ErrorMessage{StatusCode: test.status, Error: errors.New(test.body)}
			close(errs)

			h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
			exposed := strings.Contains(recorder.Body.String(), `"type":"error"`)
			if exposed != test.want {
				t.Fatalf("error exposed = %t, want %t: %q", exposed, test.want, recorder.Body.String())
			}
		})
	}
}

func TestForwardResponsesStreamUsesResponseFailedForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "Codex Desktop/26.803.41515")
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`),
	}
	close(errs)

	h.forwardResponsesStream(c, c.Writer.(http.Flusher), func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") || strings.Contains(body, "event: error") {
		t.Fatalf("unexpected Codex terminal event: %q", body)
	}
	if !strings.Contains(body, `"type":"invalid_request"`) || !strings.Contains(body, `"code":"cyber_policy"`) {
		t.Fatalf("nested error was not preserved: %q", body)
	}
}

package logging

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestFileRequestLoggerToggle(t *testing.T) {
	logger := NewFileRequestLogger(false, "", "", 10)
	if logger.IsEnabled() {
		t.Fatalf("expected logger to start disabled")
	}

	logger.SetEnabled(true)
	if !logger.IsEnabled() {
		t.Fatalf("expected logger to be enabled after SetEnabled(true)")
	}

	logger.SetEnabled(false)
	if logger.IsEnabled() {
		t.Fatalf("expected logger to be disabled after SetEnabled(false)")
	}
}

func TestWriteRequestBodyTempFileSkipsEmptyBody(t *testing.T) {
	logger := NewFileRequestLogger(true, t.TempDir(), "", 10)

	path, err := logger.writeRequestBodyTempFile(nil)
	if err != nil {
		t.Fatalf("writeRequestBodyTempFile(nil) error = %v", err)
	}
	if path != "" {
		t.Fatalf("writeRequestBodyTempFile(nil) path = %q, want empty", path)
	}
}

func TestWriteRequestInfoWithBodyWritesInlineBody(t *testing.T) {
	var output bytes.Buffer
	headers := map[string][]string{
		"Content-Type": {"application/json"},
	}
	body := []byte(`{"hello":"world"}`)
	timestamp := time.Unix(1700000000, 0).UTC()

	err := writeRequestInfoWithBody(&output, "/v1/chat/completions", "POST", headers, body, "", timestamp, "", "", true)
	if err != nil {
		t.Fatalf("writeRequestInfoWithBody error = %v", err)
	}

	logOutput := output.String()
	if !strings.Contains(logOutput, "URL: /v1/chat/completions") {
		t.Fatalf("log output missing URL: %q", logOutput)
	}
	if !strings.Contains(logOutput, "Method: POST") {
		t.Fatalf("log output missing method: %q", logOutput)
	}
	if !strings.Contains(logOutput, `{"hello":"world"}`) {
		t.Fatalf("log output missing request body: %q", logOutput)
	}
}

func TestWriteAPIErrorResponsesRedactsSensitiveValues(t *testing.T) {
	var output bytes.Buffer
	err := writeAPIErrorResponses(&output, []*interfaces.ErrorMessage{{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("upstream failed Authorization: Bearer sk-secret-token access_token=access-secret visible"),
	}})
	if err != nil {
		t.Fatalf("writeAPIErrorResponses error = %v", err)
	}

	logOutput := output.String()
	for _, leaked := range []string{"sk-secret-token", "access-secret"} {
		if strings.Contains(logOutput, leaked) {
			t.Fatalf("API error log leaked %q: %s", leaked, logOutput)
		}
	}
	if !strings.Contains(logOutput, "[REDACTED]") || !strings.Contains(logOutput, "visible") {
		t.Fatalf("API error log missing redacted visible content: %s", logOutput)
	}
}

func TestFormatLogContentRedactsAPIErrorResponses(t *testing.T) {
	logger := NewFileRequestLogger(true, "", "", 10)
	content := logger.formatLogContent(
		"/v1/responses",
		"POST",
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		http.StatusBadGateway,
		nil,
		[]*interfaces.ErrorMessage{{StatusCode: http.StatusBadGateway, Error: errors.New("api_key=sk-secret visible")}},
	)
	if strings.Contains(content, "sk-secret") {
		t.Fatalf("formatted log leaked API key: %s", content)
	}
	if !strings.Contains(content, "api_key=[REDACTED]") || !strings.Contains(content, "visible") {
		t.Fatalf("formatted log missing redacted visible content: %s", content)
	}
}

func TestGenerateFilenameSanitizesPathAndQuery(t *testing.T) {
	logger := NewFileRequestLogger(true, "", "", 10)

	filename := logger.generateFilename("/v1/responses?api_key=secret value", "req-1")
	if strings.Contains(filename, "?") {
		t.Fatalf("filename should not contain query string: %q", filename)
	}
	if strings.Contains(filename, " ") {
		t.Fatalf("filename should not contain spaces: %q", filename)
	}
	if !strings.HasPrefix(filename, "v1-responses-") {
		t.Fatalf("filename prefix = %q, want v1-responses-*", filename)
	}
	if !strings.HasSuffix(filename, "-req-1.log") {
		t.Fatalf("filename suffix = %q, want *-req-1.log", filename)
	}
}

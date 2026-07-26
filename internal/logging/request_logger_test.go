package logging

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestDecompressResponseMatchesMixedCaseContentEncoding(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("gzip write error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip close error = %v", err)
	}

	logger := NewFileRequestLogger(true, "", "", 10)
	got, err := logger.decompressResponse(map[string][]string{
		"cOnTeNt-EnCoDiNg": {" GZip "},
	}, compressed.Bytes())
	if err != nil {
		t.Fatalf("decompressResponse error = %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("decompressed body = %q, want hello", got)
	}
}

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

	err := writeRequestInfoWithBody(&output, "/v1/chat/completions", "POST", "203.0.113.8", headers, body, "", timestamp, "", "", true)
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
	if !strings.Contains(logOutput, "Client IP: 203.0.113.8") {
		t.Fatalf("log output missing client IP: %q", logOutput)
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

func TestFileStreamingLogWriterMarksDroppedChunks(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	streamWriter, err := logger.LogStreamingRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		"dropped-chunks",
	)
	if err != nil {
		t.Fatalf("LogStreamingRequest error = %v", err)
	}
	writer, ok := streamWriter.(*FileStreamingLogWriter)
	if !ok {
		t.Fatalf("stream writer type = %T, want *FileStreamingLogWriter", streamWriter)
	}
	if errStatus := writer.WriteStatus(http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}}); errStatus != nil {
		t.Fatalf("WriteStatus error = %v", errStatus)
	}
	writer.WriteChunkAsync([]byte("data: first\n\n"))
	writer.droppedChunks.Store(3)
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error = %v", errClose)
	}

	content, errRead := os.ReadFile(writer.logFilePath)
	if errRead != nil {
		t.Fatalf("read streaming log: %v", errRead)
	}
	if !bytes.Contains(content, []byte("data: first")) {
		t.Fatalf("streaming log missing captured chunk: %s", content)
	}
	wantMarker := fmt.Sprintf(streamingLogDroppedMarker, 3)
	if !bytes.Contains(content, []byte(wantMarker)) {
		t.Fatalf("streaming log missing drop marker %q: %s", wantMarker, content)
	}
}

func TestCleanupOldRequestLogsKeepsNewestTwenty(t *testing.T) {
	dir := t.TempDir()
	baseTime := time.Unix(1700000000, 0)
	for i := 0; i < 25; i++ {
		name := filepath.Join(dir, fmt.Sprintf("v1-responses-2026-07-11T100000-req-%02d.log", i))
		if err := os.WriteFile(name, []byte("request"), 0o600); err != nil {
			t.Fatalf("write request log: %v", err)
		}
		modTime := baseTime.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(name, modTime, modTime); err != nil {
			t.Fatalf("set request log time: %v", err)
		}
	}
	for _, name := range []string{"error-v1-responses-2026-07-11T100000-failed.log", "app.log", "response-body-part.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unrelated"), 0o600); err != nil {
			t.Fatalf("write unrelated log: %v", err)
		}
	}

	if err := cleanupOldRequestLogs(dir, requestLogRetentionMaxFiles); err != nil {
		t.Fatalf("cleanupOldRequestLogs() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log directory: %v", err)
	}
	requestLogs := 0
	for _, entry := range entries {
		if requestLogFilenamePattern.MatchString(entry.Name()) && !strings.HasPrefix(entry.Name(), "error-") {
			requestLogs++
		}
	}
	if requestLogs != requestLogRetentionMaxFiles {
		t.Fatalf("retained request logs = %d, want %d", requestLogs, requestLogRetentionMaxFiles)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1-responses-2026-07-11T100000-req-04.log")); !os.IsNotExist(err) {
		t.Fatalf("old request log was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1-responses-2026-07-11T100000-req-24.log")); err != nil {
		t.Fatalf("newest request log was removed: %v", err)
	}
	for _, name := range []string{"error-v1-responses-2026-07-11T100000-failed.log", "app.log", "response-body-part.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %s was removed: %v", name, err)
		}
	}
}

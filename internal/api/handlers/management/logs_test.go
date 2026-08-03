package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type logsTestPayload struct {
	Lines  []string `json:"lines"`
	Total  int      `json:"line-count"`
	Latest int64    `json:"latest-timestamp"`
}

func TestGetLogsUsesIncrementalCursorForAppendedMainLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, defaultLogFileName)
	initial := "[2026-08-03 10:00:00] first\n[2026-08-03 10:00:01] second\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial log: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir, LoggingToFile: true}, nil)
	h.SetLogDirectory(dir)
	first := getLogsTestPayload(t, h, "/v0/management/logs?limit=20")
	if len(first.Lines) != 2 || first.Total != 2 {
		t.Fatalf("initial payload = %#v", first)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	if _, err = file.WriteString("[2026-08-03 10:00:02] third\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append log: %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	second := getLogsTestPayload(t, h, "/v0/management/logs?after="+strconv.FormatInt(first.Latest, 10)+"&limit=20")
	if len(second.Lines) != 1 || second.Lines[0] != "[2026-08-03 10:00:02] third" {
		t.Fatalf("incremental payload = %#v", second)
	}
	if second.Total != 3 || second.Latest <= first.Latest {
		t.Fatalf("incremental counters = %#v, first = %#v", second, first)
	}
}

func TestGetLogsCursorFallsBackAfterTruncate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, defaultLogFileName)
	if err := os.WriteFile(path, []byte("[2026-08-03 10:00:00] old\n[2026-08-03 10:00:01] old-new\n"), 0o600); err != nil {
		t.Fatalf("write initial log: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir, LoggingToFile: true}, nil)
	h.SetLogDirectory(dir)
	first := getLogsTestPayload(t, h, "/v0/management/logs")

	if err := os.WriteFile(path, []byte("[2026-08-03 10:00:02] replacement\n"), 0o600); err != nil {
		t.Fatalf("replace log: %v", err)
	}
	second := getLogsTestPayload(t, h, "/v0/management/logs?after="+strconv.FormatInt(first.Latest, 10))
	if len(second.Lines) != 1 || second.Lines[0] != "[2026-08-03 10:00:02] replacement" {
		t.Fatalf("replacement payload = %#v", second)
	}
	if second.Total != 1 {
		t.Fatalf("replacement total = %d, want 1", second.Total)
	}
}

func TestParseTimestampFastPathMatchesExpectedLayout(t *testing.T) {
	want := parseTimestamp("[2026-08-03 00:00:00] midnight")
	if want == 0 {
		t.Fatal("valid midnight timestamp was rejected")
	}
	if got := parseTimestamp("2026-02-30 10:00:00 invalid"); got != 0 {
		t.Fatalf("invalid calendar date parsed as %d", got)
	}
	if got := parseTimestamp("[2026-08-03 10:00:01] valid"); got != parseTimestamp("2026-08-03 10:00:01 valid") {
		t.Fatalf("bracketed and unbracketed timestamps differ: %d", got)
	}
}

func BenchmarkReadLogsIncrementalCacheHit(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, defaultLogFileName)
	var content strings.Builder
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&content, "[2026-08-03 10:00:01] line-%d\n", i)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		b.Fatalf("write benchmark log: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir, LoggingToFile: true}, nil)
	files := []string{path}
	_, _, latest, err := h.readLogsIncrementally(dir, files, 0, 200)
	if err != nil {
		b.Fatalf("prime log cache: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, _, err := h.readLogsIncrementally(dir, files, latest, 200); err != nil {
			b.Fatalf("incremental log read: %v", err)
		}
	}
}

func BenchmarkParseTimestamp(b *testing.B) {
	line := "[2026-08-03 10:00:01] benchmark"
	b.ReportAllocs()
	for b.Loop() {
		if parseTimestamp(line) == 0 {
			b.Fatal("timestamp was not parsed")
		}
	}
}

func getLogsTestPayload(t *testing.T, h *Handler, target string) logsTestPayload {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	h.GetLogs(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetLogs(%s) status = %d, body = %s", target, recorder.Code, recorder.Body.String())
	}
	var payload logsTestPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GetLogs(%s): %v", target, err)
	}
	return payload
}

package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestReadRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("decodes supported content encoding", func(t *testing.T) {
		raw := []byte(`{"model":"compressed"}`)
		compressed := zstdEncodeForTest(t, raw)
		c := requestBodyTestContext(t, compressed, "ZsTd, Identity")

		decoded, err := ReadRequestBody(c)
		if err != nil {
			t.Fatalf("ReadRequestBody() error = %v", err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("decoded = %q, want %q", decoded, raw)
		}
	})

	t.Run("keeps valid json when encoding header is stale", func(t *testing.T) {
		raw := []byte(`{"model":"already-json"}`)
		c := requestBodyTestContext(t, raw, "gzip")

		decoded, err := ReadRequestBody(c)
		if err != nil {
			t.Fatalf("ReadRequestBody() error = %v", err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("decoded = %q, want %q", decoded, raw)
		}
	})

	t.Run("returns error when encoded body cannot be decoded", func(t *testing.T) {
		c := requestBodyTestContext(t, []byte(`not json`), "gzip")
		_, err := ReadRequestBody(c)
		if err == nil {
			t.Fatal("expected unsupported encoding error")
		}
	})
}

func requestBodyTestContext(t *testing.T, body []byte, encoding string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	c.Request = req
	return c
}

func zstdEncodeForTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := encoder.Write(raw); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return compressed.Bytes()
}

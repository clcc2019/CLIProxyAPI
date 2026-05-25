package executor

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexCompressionEnv = "CODEX_ENABLE_ZSTD_REQUEST_COMPRESSION"

var codexZstdEncoderPool sync.Pool

func maybeEnableCodexRequestCompression(req *http.Request, auth *cliproxyauth.Auth) error {
	return maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, nil)
}

func maybeEnableCodexRequestCompressionWithBody(req *http.Request, auth *cliproxyauth.Auth, body []byte) error {
	return maybeEnableCodexRequestCompressionWithConfig(req, auth, nil, body)
}

func maybeEnableCodexRequestCompressionWithConfig(req *http.Request, auth *cliproxyauth.Auth, cfg *config.Config, body []byte) error {
	if req == nil || auth == nil || codexIsAPIKeyAuth(auth) || codexRequestCompressionSkipsTarget(req, auth) || !codexRequestCompressionEnabled(cfg) {
		return nil
	}
	if encoding := strings.TrimSpace(req.Header.Get("Content-Encoding")); encoding != "" {
		return nil
	}
	if contentType := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type"))); !strings.HasPrefix(contentType, "application/json") {
		return nil
	}

	payload := body
	if payload == nil {
		if req.Body == nil {
			return nil
		}
		readBody, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		if errClose := req.Body.Close(); errClose != nil {
			return errClose
		}
		payload = readBody
	}

	if len(payload) == 0 {
		codexResetRequestBody(req, payload)
		return nil
	}

	compressed, err := compressCodexRequestBody(payload)
	if err != nil {
		codexResetRequestBody(req, payload)
		return err
	}
	req.Header.Set("Content-Encoding", "zstd")
	codexResetRequestBody(req, compressed)
	return nil
}

func codexRequestCompressionSkipsTarget(req *http.Request, auth *cliproxyauth.Auth) bool {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "azure") {
		return true
	}
	return req != nil && req.URL != nil && codexMatchesAzureResponsesBaseURL(req.URL.String())
}

func compressCodexRequestBody(body []byte) ([]byte, error) {
	var compressed bytes.Buffer
	encoder, err := borrowCodexZstdEncoder(&compressed)
	if err != nil {
		return nil, err
	}
	if _, errWrite := encoder.Write(body); errWrite != nil {
		_ = encoder.Close()
		return nil, errWrite
	}
	if errClose := encoder.Close(); errClose != nil {
		return nil, errClose
	}
	codexZstdEncoderPool.Put(encoder)
	return compressed.Bytes(), nil
}

func borrowCodexZstdEncoder(w io.Writer) (*zstd.Encoder, error) {
	if cached := codexZstdEncoderPool.Get(); cached != nil {
		if encoder, ok := cached.(*zstd.Encoder); ok && encoder != nil {
			encoder.Reset(w)
			return encoder, nil
		}
	}
	return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
}

func codexRequestCompressionEnabled(cfg *config.Config) bool {
	if cfg != nil && cfg.EnableRequestCompression != nil {
		return *cfg.EnableRequestCompression
	}

	value := strings.TrimSpace(os.Getenv(codexCompressionEnv))
	switch strings.ToLower(value) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

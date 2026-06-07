package executor

import (
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
	rawURL := ""
	if req != nil && req.URL != nil {
		rawURL = req.URL.String()
	}
	return maybeEnableCodexRequestCompressionWithConfigForURL(req, auth, cfg, body, rawURL)
}

func maybeEnableCodexRequestCompressionWithConfigForURL(req *http.Request, auth *cliproxyauth.Auth, cfg *config.Config, body []byte, rawURL string) error {
	if req == nil || auth == nil || codexIsAPIKeyAuth(auth) || codexRequestCompressionSkipsTargetURL(rawURL, auth) || !codexRequestCompressionEnabled(cfg) {
		return nil
	}
	if encoding := strings.TrimSpace(req.Header.Get("Content-Encoding")); encoding != "" {
		return nil
	}
	if !codexRequestContentTypeIsJSON(req.Header.Get("Content-Type")) {
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

func codexRequestContentTypeIsJSON(contentType string) bool {
	const prefix = "application/json"
	contentType = strings.TrimSpace(contentType)
	if len(contentType) < len(prefix) {
		return false
	}
	return strings.EqualFold(contentType[:len(prefix)], prefix)
}

func codexRequestCompressionSkipsTarget(req *http.Request, auth *cliproxyauth.Auth) bool {
	rawURL := ""
	if req != nil && req.URL != nil {
		rawURL = req.URL.String()
	}
	return codexRequestCompressionSkipsTargetURL(rawURL, auth)
}

func codexRequestCompressionSkipsTargetURL(rawURL string, auth *cliproxyauth.Auth) bool {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "azure") {
		return true
	}
	return codexMatchesAzureResponsesBaseURL(rawURL)
}

func compressCodexRequestBody(body []byte) ([]byte, error) {
	encoder, err := borrowCodexZstdEncoder()
	if err != nil {
		return nil, err
	}
	compressed := encoder.EncodeAll(body, make([]byte, 0, 256))
	codexZstdEncoderPool.Put(encoder)
	return compressed, nil
}

func borrowCodexZstdEncoder() (*zstd.Encoder, error) {
	if cached := codexZstdEncoderPool.Get(); cached != nil {
		if encoder, ok := cached.(*zstd.Encoder); ok && encoder != nil {
			return encoder, nil
		}
	}
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
}

func codexRequestCompressionEnabled(cfg *config.Config) bool {
	if cfg != nil && cfg.EnableRequestCompression != nil {
		return *cfg.EnableRequestCompression
	}

	return codexRequestCompressionEnvEnabled(os.Getenv(codexCompressionEnv))
}

func codexRequestCompressionEnvEnabled(value string) bool {
	value = strings.TrimSpace(value)
	switch {
	case value == "",
		value == "1",
		strings.EqualFold(value, "true"),
		strings.EqualFold(value, "yes"),
		strings.EqualFold(value, "on"):
		return true
	case value == "0",
		strings.EqualFold(value, "false"),
		strings.EqualFold(value, "no"),
		strings.EqualFold(value, "off"):
		return false
	default:
		return true
	}
}

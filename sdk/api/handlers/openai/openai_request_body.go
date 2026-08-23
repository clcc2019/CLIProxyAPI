package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpbody"
)

const maxPooledOpenAIRequestBodyCapacity = 1 << 20

var openAIRequestBodyPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func readOpenAIRequestBody(c *gin.Context) ([]byte, *bytes.Buffer, error) {
	buffer := openAIRequestBodyPool.Get().(*bytes.Buffer)
	buffer.Reset()
	if _, err := buffer.ReadFrom(c.Request.Body); err != nil {
		releaseOpenAIRequestBody(buffer)
		return nil, nil, err
	}

	raw := buffer.Bytes()
	encoding := strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, buffer, nil
	}
	decoded, err := httpbody.DecodeContentEncodedRequestBody(raw, encoding)
	if err != nil {
		if json.Valid(raw) {
			return raw, buffer, nil
		}
		releaseOpenAIRequestBody(buffer)
		return nil, nil, err
	}
	return decoded, buffer, nil
}

func releaseOpenAIRequestBody(buffer *bytes.Buffer) {
	if buffer == nil || buffer.Cap() > maxPooledOpenAIRequestBodyCapacity {
		return
	}
	buffer.Reset()
	openAIRequestBodyPool.Put(buffer)
}

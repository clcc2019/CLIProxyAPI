package management

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const authFilesJSONCompressionThreshold = 16 << 10

// writeAuthFilesJSON keeps the auth-file list response compatible with normal
// JSON clients while compressing large dashboard payloads at the API boundary.
// The list endpoint is a particularly good compression candidate because its
// repeated status and recent-request fields are highly redundant.
func writeAuthFilesJSON(c *gin.Context, status int, payload any) {
	if c == nil {
		return
	}

	body, err := json.Marshal(payload)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to encode auth file list"})
		return
	}

	if len(body) >= authFilesJSONCompressionThreshold && acceptsGzip(c.GetHeader("Accept-Encoding")) {
		var compressed bytes.Buffer
		writer, errWriter := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
		if errWriter == nil {
			if _, errWrite := writer.Write(body); errWrite == nil {
				if errClose := writer.Close(); errClose == nil {
					c.Header("Content-Encoding", "gzip")
					c.Header("Vary", "Accept-Encoding")
					c.Data(status, "application/json; charset=utf-8", compressed.Bytes())
					return
				}
			}
			_ = writer.Close()
		}
	}

	c.Data(status, "application/json; charset=utf-8", body)
}

func acceptsGzip(header string) bool {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		encoding := strings.TrimSpace(strings.ToLower(parts[0]))
		if encoding != "gzip" && encoding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				quality = 0
			} else {
				quality = parsed
			}
			break
		}
		return quality > 0
	}
	return false
}

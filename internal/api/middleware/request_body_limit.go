package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequestBodyLimitMiddleware applies a hard cap to inbound request bodies before
// handlers call GetRawData, ParseMultipartForm, or ReadAll.
func RequestBodyLimitMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes <= 0 || c == nil || c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "request body too large",
				"limit": maxBytes,
			})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

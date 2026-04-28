package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestStartNonStreamingKeepAliveDoesNotCommitResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/test", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{NonStreamKeepAliveInterval: 1}}
	stop := handler.StartNonStreamingKeepAlive(ginCtx, ctx)
	defer stop()

	time.Sleep(1100 * time.Millisecond)

	if ginCtx.Writer.Written() {
		t.Fatal("non-streaming keepalive committed the HTTP response before the final payload")
	}
	if body := recorder.Body.String(); body != "" {
		t.Fatalf("non-streaming keepalive wrote %q before the final payload", body)
	}
}

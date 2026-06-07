package management

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type callbackForwarder struct {
	provider string
	server   *http.Server
	done     chan struct{}
}

var (
	callbackForwardersMu sync.Mutex
	callbackForwarders   = make(map[int]*callbackForwarder)
)

func oauthSessionErrorWithDetail(prefix string, err error) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "Authentication failed"
	}
	if err == nil {
		return prefix
	}
	detail := strings.TrimSpace(err.Error())
	if detail == "" {
		return prefix
	}
	if len(detail) > 600 {
		detail = detail[:600] + "..."
	}
	return prefix + ": " + detail
}

func codexLoginRequestUserAgent(c *gin.Context) string {
	if c == nil || isWebUIRequest(c) {
		return ""
	}
	return strings.TrimSpace(c.GetHeader("User-Agent"))
}

func startCallbackForwarder(port int, provider, targetBase string) (*callbackForwarder, error) {
	if err := validateCallbackForwardTarget(targetBase); err != nil {
		return nil, err
	}

	callbackForwardersMu.Lock()
	prev := callbackForwarders[port]
	if prev != nil {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	if prev != nil {
		stopForwarderInstance(port, prev)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := targetBase
		if raw := r.URL.RawQuery; raw != "" {
			if strings.Contains(target, "?") {
				target = target + "&" + raw
			} else {
				target = target + "?" + raw
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		// #nosec G710 -- targetBase is restricted to the local management callback URL before the server starts.
		http.Redirect(w, r, target, http.StatusFound)
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	done := make(chan struct{})

	go func() {
		if errServe := srv.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.WithError(errServe).Warnf("callback forwarder for %s stopped unexpectedly", provider)
		}
		close(done)
	}()

	forwarder := &callbackForwarder{
		provider: provider,
		server:   srv,
		done:     done,
	}

	callbackForwardersMu.Lock()
	callbackForwarders[port] = forwarder
	callbackForwardersMu.Unlock()

	log.Infof("callback forwarder for %s listening on %s", provider, addr)

	return forwarder, nil
}

func validateCallbackForwardTarget(target string) error {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return fmt.Errorf("invalid callback target: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("invalid callback target scheme")
	}
	host := strings.TrimSpace(u.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("callback target must be local")
	}
	if strings.TrimSpace(u.Port()) == "" {
		return fmt.Errorf("callback target port is required")
	}
	if !strings.HasPrefix(u.Path, "/") {
		return fmt.Errorf("callback target path is invalid")
	}
	return nil
}

func stopCallbackForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil {
		return
	}
	callbackForwardersMu.Lock()
	if current := callbackForwarders[port]; current == forwarder {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	stopForwarderInstance(port, forwarder)
}

func stopForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil || forwarder.server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := forwarder.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.WithError(err).Warnf("failed to shut down callback forwarder on port %d", port)
	}

	select {
	case <-forwarder.done:
	case <-time.After(2 * time.Second):
	}

	log.Infof("callback forwarder on port %d stopped", port)
}

func (h *Handler) managementCallbackURL(path string) (string, error) {
	if h == nil || h.cfg == nil || h.cfg.Port <= 0 {
		return "", fmt.Errorf("server port is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if h.cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, h.cfg.Port, path), nil
}

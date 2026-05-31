package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type homeStatusErr struct {
	code int
	msg  string
}

func (e homeStatusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}

func (e homeStatusErr) StatusCode() int { return e.code }

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// RefreshAuthViaHome replaces local refresh logic when home control plane integration is enabled.
// It returns (updatedAuth, true, nil) when home refresh succeeds; (nil, true, err) when home is
// enabled but refresh fails; and (nil, false, nil) when home is disabled.
func RefreshAuthViaHome(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, bool, error) {
	if cfg == nil || !cfg.Home.Enabled {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return nil, true, homeStatusErr{code: http.StatusInternalServerError, msg: "home refresh: auth is nil"}
	}

	client := home.Current()
	if client == nil || !client.HeartbeatOK() {
		return nil, true, homeStatusErr{code: http.StatusServiceUnavailable, msg: "home control center unavailable"}
	}

	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		authIndex = strings.TrimSpace(auth.EnsureIndex())
	}
	if authIndex == "" {
		return nil, true, homeStatusErr{code: http.StatusBadGateway, msg: "home refresh: auth_index is empty"}
	}

	raw, err := client.GetRefreshAuth(ctx, authIndex)
	if err != nil {
		return nil, true, homeStatusErr{code: statusFromHomeRefreshClientError(err), msg: err.Error()}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		return nil, true, homeStatusErr{code: statusFromHomeErrorCode(code), msg: msg}
	}

	var updated cliproxyauth.Auth
	if errUnmarshal := json.Unmarshal(raw, &updated); errUnmarshal != nil {
		return nil, true, homeStatusErr{code: http.StatusBadGateway, msg: "home returned invalid auth payload"}
	}
	updated.Index = authIndex
	updated.EnsureIndex()
	return &updated, true, nil
}

func statusFromHomeErrorCode(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "authentication_error", "unauthorized":
		return http.StatusUnauthorized
	case "model_not_found":
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

func statusFromHomeRefreshClientError(err error) int {
	if err == nil {
		return http.StatusBadGateway
	}
	type statusCoder interface{ StatusCode() int }
	var status statusCoder
	if errors.As(err, &status) && status != nil {
		if code := status.StatusCode(); code > 0 {
			return code
		}
	}
	raw := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(raw, "http 401") ||
		strings.Contains(raw, "status 401") ||
		strings.Contains(raw, "401 unauthorized") ||
		strings.Contains(raw, "please try signing in again") ||
		strings.Contains(raw, "sign in again") {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

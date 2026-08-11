package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
)

type lifecycleStatusError struct {
	status int
	msg    string
	cause  error
}

func (e lifecycleStatusError) Error() string   { return e.msg }
func (e lifecycleStatusError) StatusCode() int { return e.status }
func (e lifecycleStatusError) Unwrap() error   { return e.cause }

func TestResultErrorFromErrorClassifiesConnectionLifecycle(t *testing.T) {
	tests := []error{
		context.Canceled,
		io.EOF,
		fmt.Errorf("read stream: %w", io.ErrUnexpectedEOF),
		&websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "normal"},
		&websocket.CloseError{Code: websocket.CloseGoingAway, Text: "away"},
		&websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"},
	}
	for _, err := range tests {
		got := resultErrorFromError(err)
		if got.Code != connectionLifecycleErrorCode || !shouldSkipCredentialCooldown(got) {
			t.Fatalf("resultErrorFromError(%v) = %#v, want lifecycle", err, got)
		}
	}
}

func TestConnectionLifecycleRequiresMissingHTTPStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		err := lifecycleStatusError{status: status, msg: "unexpected EOF", cause: io.ErrUnexpectedEOF}
		got := resultErrorFromError(err)
		if got.HTTPStatus != status || shouldSkipCredentialCooldown(got) {
			t.Fatalf("status %d result = %#v, want status-bearing failure", status, got)
		}
	}
}

func TestManagerMarkResultSkipsLifecycleAndRequestFaultCooldown(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
	}{
		{name: "lifecycle", err: &Error{Code: connectionLifecycleErrorCode, Message: "unexpected EOF"}},
		{name: "unprocessable request", err: &Error{HTTPStatus: http.StatusUnprocessableEntity, Message: "invalid input"}},
		{
			name: "structured request fault",
			err: &Error{
				HTTPStatus: http.StatusBadGateway,
				Message:    `{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "auth-" + test.name, Provider: "codex"}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register: %v", err)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID: auth.ID,
				Model:  "gpt-5.4",
				Error:  test.err,
			})
			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("auth missing")
			}
			if updated.Unavailable || !updated.NextRetryAfter.IsZero() || len(updated.ModelStates) != 0 {
				t.Fatalf("unexpected cooldown state: %#v", updated)
			}
		})
	}
}

func TestIsRequestInvalidErrorDoesNotTreatUnknown500AsClientFault(t *testing.T) {
	err := lifecycleStatusError{
		status: http.StatusInternalServerError,
		msg:    `{"error":{"code":500,"message":"Internal error encountered.","status":"UNKNOWN"}}`,
	}
	if isRequestInvalidError(err) {
		t.Fatalf("unknown 500 was classified as request fault: %v", err)
	}
	if !isRequestInvalidError(lifecycleStatusError{status: http.StatusConflict, msg: "conflict"}) {
		t.Fatal("409 was not classified as request fault")
	}
	if isRequestInvalidError(errors.New("unexpected EOF")) {
		t.Fatal("transport error was classified as request fault")
	}
}

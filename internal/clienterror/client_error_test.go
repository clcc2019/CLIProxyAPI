package clienterror

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type statusError struct {
	status int
	body   string
	cause  error
}

func (e statusError) Error() string   { return e.body }
func (e statusError) StatusCode() int { return e.status }
func (e statusError) Unwrap() error   { return e.cause }

func TestHTTPStatusFromErrorPrefersExplicitStatus(t *testing.T) {
	err := statusError{status: http.StatusTooManyRequests, body: "canceled", cause: context.Canceled}
	if got := HTTPStatusFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := HTTPStatusFromError(context.Canceled); got != StatusClientClosedRequest {
		t.Fatalf("canceled status = %d, want %d", got, StatusClientClosedRequest)
	}
}

func TestIsRequestFaultHonorsStatusAndStructuredBody(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "bad request", status: http.StatusBadRequest, body: "bad request", want: true},
		{name: "structured fault behind gateway", status: http.StatusBadGateway, body: `{"error":{"type":"invalid_request","code":"cyber_policy"}}`, want: true},
		{name: "authentication", status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","code":"invalid_request_error"}}`},
		{name: "payment", status: http.StatusPaymentRequired, body: `{"error":{"code":"invalid_request_error"}}`},
		{name: "quota", status: http.StatusTooManyRequests, body: `{"error":{"code":"invalid_request_error"}}`},
		{name: "transport", status: http.StatusBadGateway, body: "unexpected EOF"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRequestFault(test.status, errors.New(test.body)); got != test.want {
				t.Fatalf("IsRequestFault() = %t, want %t", got, test.want)
			}
		})
	}
}

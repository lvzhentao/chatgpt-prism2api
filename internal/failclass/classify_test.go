package failclass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type statusError struct {
	status int
	body   string
}

func (e statusError) Error() string   { return e.body }
func (e statusError) StatusCode() int { return e.status }

type statusAndUnwrapError struct {
	status int
	body   string
	cause  error
}

func (e statusAndUnwrapError) Error() string { return e.body }
func (e statusAndUnwrapError) StatusCode() int {
	return e.status
}
func (e statusAndUnwrapError) Unwrap() error { return e.cause }

func TestHTTPStatusFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "plain error", err: errors.New("boom"), want: 0},
		{name: "context canceled", err: context.Canceled, want: StatusClientClosedRequest},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "url error wraps canceled",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
			want: StatusClientClosedRequest,
		},
		{
			name: "url error wraps deadline",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.DeadlineExceeded},
			want: http.StatusGatewayTimeout,
		},
		{
			name: "fmt wrap canceled",
			err:  fmt.Errorf("upstream: %w", context.Canceled),
			want: StatusClientClosedRequest,
		},
		{
			name: "explicit status code wins",
			err:  statusError{status: http.StatusTooManyRequests, body: "rate limited"},
			want: http.StatusTooManyRequests,
		},
		{
			name: "explicit status wins over canceled unwrap",
			err: statusAndUnwrapError{
				status: http.StatusTooManyRequests,
				body:   "rate limited",
				cause:  context.Canceled,
			},
			want: http.StatusTooManyRequests,
		},
		{
			name: "zero status code falls through to canceled unwrap",
			err: statusAndUnwrapError{
				status: 0,
				body:   "canceled",
				cause:  context.Canceled,
			},
			want: StatusClientClosedRequest,
		},
		{
			name: "zero status code without unwrap stays unknown",
			err:  statusError{status: 0, body: context.Canceled.Error()},
			want: 0,
		},
		{
			name: "wrapped status code via errors.As",
			err:  fmt.Errorf("execute failed: %w", statusError{status: http.StatusUnauthorized, body: "unauthorized"}),
			want: http.StatusUnauthorized,
		},
		{
			name: "cursor agent run http wrapper",
			err:  errors.New("agent run http 429: rate limited"),
			want: http.StatusTooManyRequests,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTPStatusFromError(tc.err); got != tc.want {
				t.Fatalf("HTTPStatusFromError() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestIsRequestFaultStructuredIdentifiers(t *testing.T) {
	for _, code := range []string{
		"cyber_policy",
		"context_length_exceeded",
		"message_too_big",
		"string_above_max_length",
		"invalid_prompt",
		"invalid_value",
		"unsupported_value",
		"invalid_request_error",
		"previous_response_not_found",
	} {
		t.Run("code/"+code, func(t *testing.T) {
			err := errors.New(`{"error":{"code":"` + code + `"}}`)
			if !IsRequestFault(http.StatusBadGateway, err) {
				t.Fatalf("code %q was not classified as a request fault", code)
			}
		})
	}

	for _, errType := range []string{
		"invalid_request",
		"invalid_request_error",
		"bad_request_error",
		"invalid_prompt",
	} {
		t.Run("type/"+errType, func(t *testing.T) {
			err := errors.New(`{"error":{"type":"` + errType + `"}}`)
			if !IsRequestFault(http.StatusBadGateway, err) {
				t.Fatalf("type %q was not classified as a request fault", errType)
			}
		})
	}
}

func TestIsRequestFault(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		{name: "bad request status", status: http.StatusBadRequest, err: errors.New("bad request"), want: true},
		{name: "conflict status", status: http.StatusConflict, err: errors.New("conflict"), want: true},
		{name: "entity too large status", status: http.StatusRequestEntityTooLarge, err: errors.New("too large"), want: true},
		{name: "unprocessable status", status: http.StatusUnprocessableEntity, err: errors.New("unprocessable"), want: true},
		{
			name:   "cyber policy behind bad gateway",
			status: http.StatusBadGateway,
			err:    errors.New(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`),
			want:   true,
		},
		{
			name:   "context length behind internal error",
			status: http.StatusInternalServerError,
			err:    errors.New(`{"response":{"error":{"type":"server_error","code":"context_length_exceeded"}}}`),
			want:   true,
		},
		{
			name:   "invalid request type behind bad gateway",
			status: http.StatusBadGateway,
			err:    errors.New(`{"body":{"error":{"type":"invalid_request","message":"invalid"}}}`),
			want:   true,
		},
		{
			name: "status from error",
			err:  statusError{status: http.StatusConflict, body: "conflict"},
			want: true,
		},
		{
			name:   "item not persisted with store=false",
			status: http.StatusNotFound,
			err:    errors.New("Item with id 'rs_0b5f3eb6f51f175c0169ca74e4a85881998539920821603a74' not found. Items are not persisted when `store` is set to false. Try again with `store` set to true, or remove this item from your input."),
			want:   true,
		},
		{
			name:   "upstream unknown internal error",
			status: http.StatusInternalServerError,
			err:    errors.New(`{"error":{"code":500,"message":"Internal error encountered.","status":"UNKNOWN"}}`),
		},
		{name: "plain not found", status: http.StatusNotFound, err: errors.New("model not found")},
		{name: "unauthorized", status: http.StatusUnauthorized, err: errors.New("invalid token")},
		{
			name:   "deepseek authentication failure is credential failure",
			status: http.StatusUnauthorized,
			err:    errors.New(`{"error":{"code":"invalid_request_error","message":"Authentication Fails, Your api key: ****heck is invalid","param":null,"type":"authentication_error"}}`),
			want:   false,
		},
		{
			name:   "deepseek insufficient balance is payment failure",
			status: http.StatusPaymentRequired,
			err:    errors.New(`{"error":{"message":"Insufficient Balance","type":"unknown_error","param":null,"code":"invalid_request_error"}}`),
			want:   false,
		},
		{
			name:   "rate limit status overrides generic request error code",
			status: http.StatusTooManyRequests,
			err:    errors.New(`{"error":{"message":"Rate Limit Reached","type":"unknown_error","param":null,"code":"invalid_request_error"}}`),
			want:   false,
		},
		{name: "quota", status: http.StatusTooManyRequests, err: errors.New("quota")},
		{name: "transport", status: http.StatusBadGateway, err: errors.New("unexpected EOF")},
		{name: "invalid JSON body", status: http.StatusBadGateway, err: errors.New(`{"error":`)},
		{name: "nil", status: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequestFault(tc.status, tc.err); got != tc.want {
				t.Fatalf("IsRequestFault(%d, %v) = %t, want %t", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyCursorSemantics(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     Class
		cool      bool
		switchAcc bool
	}{
		{name: "canceled", err: context.Canceled, class: Canceled, cool: false, switchAcc: false},
		{name: "deadline skips cool still switches", err: context.DeadlineExceeded, class: Server, cool: false, switchAcc: true},
		{name: "eof skips cool still switches", err: io.ErrUnexpectedEOF, class: Server, cool: false, switchAcc: true},
		{name: "region", err: errors.New("agent run error: not supported in your region"), class: Region, cool: false, switchAcc: false},
		{name: "bad model", err: errors.New("error_bad_model_name"), class: BadRequest, cool: false, switchAcc: false},
		{name: "context length plaintext", err: errors.New("context_length_exceeded"), class: BadRequest, cool: false, switchAcc: false},
		{name: "model not available", err: errors.New("model not available"), class: Unavailable, cool: true, switchAcc: true},
		{
			name:      "429 rate limit",
			err:       statusError{status: 429, body: "rate limited"},
			class:     RateLimit,
			cool:      true,
			switchAcc: true,
		},
		{
			name:      "429 with invalid_request_error body still rate limit",
			err:       statusError{status: 429, body: `{"error":{"code":"invalid_request_error","message":"Rate Limit Reached"}}`},
			class:     RateLimit,
			cool:      true,
			switchAcc: true,
		},
		{
			name:      "402 quota",
			err:       statusError{status: 402, body: "payment required"},
			class:     Quota,
			cool:      true,
			switchAcc: true,
		},
		{
			name:      "401 auth",
			err:       statusError{status: 401, body: "unauthorized"},
			class:     Auth,
			cool:      true,
			switchAcc: true,
		},
		{
			name:      "500 server",
			err:       statusError{status: 500, body: "internal"},
			class:     Server,
			cool:      true,
			switchAcc: true,
		},
		{
			name:      "cursor http wrapper 429",
			err:       errors.New("agent run http 429: usage limit"),
			class:     RateLimit,
			cool:      true,
			switchAcc: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Class != tc.class || got.Cool != tc.cool || got.Switch != tc.switchAcc {
				t.Fatalf("Classify() = %+v, want class=%s cool=%t switch=%t", got, tc.class, tc.cool, tc.switchAcc)
			}
		})
	}
}

func TestDurationKiroFormula(t *testing.T) {
	if got := Duration(RateLimit, 1); got != 60*time.Second {
		t.Fatalf("rate limit first = %s, want 60s", got)
	}
	if got := Duration(RateLimit, 2); got != 90*time.Second {
		t.Fatalf("rate limit second = %s, want 90s", got)
	}
	if got := Duration(RateLimit, 8); got != 5*time.Minute {
		t.Fatalf("rate limit capped = %s, want 5m", got)
	}
	if got := Duration(Server, 1); got != 120*time.Second {
		t.Fatalf("server first = %s, want 120s", got)
	}
	if got := Duration(Unavailable, 1); got != 5*time.Minute {
		t.Fatalf("unavailable = %s, want 5m", got)
	}
	if got := Duration(Auth, 1); got != 24*time.Hour {
		t.Fatalf("auth uses kiro long cooldown = %s, want 24h", got)
	}
	if got := Duration(Quota, 3); got != 24*time.Hour {
		t.Fatalf("quota uses kiro long cooldown = %s, want 24h", got)
	}
	if RateLimit.AutoRecoverable() != true || Auth.AutoRecoverable() != false {
		t.Fatal("auto-recoverable flags mismatch kiro")
	}
}

func TestClassifyRequestFaultBeatsServerStatus(t *testing.T) {
	err := errors.New(`{"error":{"type":"server_error","code":"context_length_exceeded"}}`)
	got := Classify(statusError{status: 500, body: err.Error()})
	if got.Class != BadRequest || got.Cool || got.Switch {
		t.Fatalf("context_length on 500 should be BadRequest: %+v", got)
	}
}
func TestClassifyUnpaidInvoiceIsQuota(t *testing.T) {
	err := errors.New("agent run error: Error | ERROR_RATE_LIMITED | You have an unpaid invoice: Your team has an unpaid invoice. Please contact your team administrator to pay your invoice and continue using Cursor.")
	res := Classify(err)
	if res.Class != Quota {
		t.Fatalf("unpaid invoice must classify as Quota, got %s", res.Class)
	}
	if !res.Cool || !res.Switch {
		t.Fatalf("quota must cool and switch: %+v", res)
	}
	if res.HTTPStatus != 402 {
		t.Fatalf("quota status: %d", res.HTTPStatus)
	}
	// 冷却时长：24h（不可自愈）
	if d := Quota.BaseDuration(); d.Hours() != 24 {
		t.Fatalf("quota base duration: %v", d)
	}
}

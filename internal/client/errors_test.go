package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestAPIError_Kind(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, "", "auth_failed"},
		{403, "", "auth_failed"},
		{404, "", "not_found"},
		{429, "", "rate_limited"},
		{500, "", "server_error"},
		{503, "", "server_error"},
		{400, "Unknown column 'stato' in 'field list'", "unknown_column"},
		{400, "Table 'cercafarmaco.foo' doesn't exist", "unknown_table"},
		{400, "plain bad request", "client_error"},
	}
	for _, tc := range cases {
		e := &APIError{StatusCode: tc.status, Body: tc.body}
		if got := e.Kind(); got != tc.want {
			t.Errorf("Kind(status=%d body=%q) = %q, want %q", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestAPIError_ExitCode(t *testing.T) {
	cases := map[int]int{401: 2, 403: 2, 400: 3, 404: 4, 429: 5, 500: 1, 200: 1}
	for status, want := range cases {
		if got := (&APIError{StatusCode: status}).ExitCode(); got != want {
			t.Errorf("ExitCode(%d) = %d, want %d", status, got, want)
		}
	}
}

func TestAPIError_Predicates(t *testing.T) {
	if !(&APIError{StatusCode: 401}).Unauthorized() {
		t.Error("401 should be Unauthorized")
	}
	if !(&APIError{StatusCode: 403}).Unauthorized() {
		t.Error("403 should be Unauthorized")
	}
	if (&APIError{StatusCode: 404}).Unauthorized() {
		t.Error("404 is not Unauthorized")
	}
	if !(&APIError{StatusCode: 404}).NotFound() {
		t.Error("404 should be NotFound")
	}
}

func TestAPIError_ErrorTruncatesBody(t *testing.T) {
	long := make([]byte, 800)
	for i := range long {
		long[i] = 'x'
	}
	e := &APIError{StatusCode: 500, Method: "GET", URL: "/api/x", Body: string(long)}
	if n := len(e.Error()); n > 600 {
		t.Errorf("Error() body not truncated: len=%d", n)
	}
}

// fakeNetTimeoutErr is a minimal net.Error whose Timeout() reports true
// without being (or wrapping) context.DeadlineExceeded — exercises
// classifyTransportErr's net.Error branch independently of the
// errors.Is(context.DeadlineExceeded) shortcut above it.
type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "fake net timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true }

// TestClassifyTransportErr pins W7's taxonomy: DeadlineExceeded and net.Error
// timeouts classify as "timeout"; DNS misses, connection-refused (however
// deeply wrapped), and TLS handshake failures classify as "network";
// anything else (including nil) stays the "client_error" residual.
func TestClassifyTransportErr(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{"nil cause", nil, "client_error"},
		{"context deadline exceeded", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline exceeded", fmt.Errorf("dial tcp: %w", context.DeadlineExceeded), "timeout"},
		{"net.Error timeout (non-DeadlineExceeded)", fakeNetTimeoutErr{}, "timeout"},
		{"dns error", &net.DNSError{Err: "no such host", Name: "invalid.example", IsNotFound: true}, "network"},
		{"connection refused (syscall wrap)", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "network"},
		{"connection refused (bare wrap)", fmt.Errorf("dial tcp 127.0.0.1:1: %w", syscall.ECONNREFUSED), "network"},
		{"tls record header error", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "network"},
		{"residual transport error", errors.New("some other transport blip"), "client_error"},
	}
	for _, tc := range cases {
		if got := classifyTransportErr(tc.cause); got != tc.want {
			t.Errorf("%s: classifyTransportErr(%v) = %q, want %q", tc.name, tc.cause, got, tc.want)
		}
	}
}

// TestAPIError_Kind_TransportErrors pins Kind()'s StatusCode==0 delegation
// to classifyTransportErr — the actual code path an agent-visible A3
// envelope goes through, not just the pure classifier in isolation.
func TestAPIError_Kind_TransportErrors(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{"deadline exceeded", context.DeadlineExceeded, "timeout"},
		{"connection refused", syscall.ECONNREFUSED, "network"},
		{"residual", errors.New("boom"), "client_error"},
	}
	for _, tc := range cases {
		e := &APIError{StatusCode: 0, Method: "GET", URL: "https://metabase.example/api/x", Cause: tc.cause}
		if got := e.Kind(); got != tc.want {
			t.Errorf("%s: Kind() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAPIError_TransportError_UnwrapAndMessage pins two things a caller
// relies on for a StatusCode==0 (transport-layer) APIError: Error() surfaces
// the underlying cause in the message, and Unwrap() exposes it so
// errors.Is/errors.As can reach through this APIError to the raw cause.
func TestAPIError_TransportError_UnwrapAndMessage(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connect: %w", syscall.ECONNREFUSED)}
	e := &APIError{StatusCode: 0, Method: "GET", URL: "https://metabase.example/api/x", Cause: cause}

	if !strings.Contains(e.Error(), "connection refused") {
		t.Errorf("Error() = %q, want it to contain the cause", e.Error())
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is(e, cause) should find the cause through Unwrap()")
	}
	if e.Kind() != "network" {
		t.Errorf("Kind() = %q, want network", e.Kind())
	}
	if e.ExitCode() != 1 {
		t.Errorf("ExitCode() = %d, want 1 (transport failures don't invent a new exit code)", e.ExitCode())
	}
}

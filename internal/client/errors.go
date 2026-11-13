package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/nicolasacchi/clicore/cierrors"
)

// APIError is returned for non-2xx Metabase responses, and — since W7 —
// also for transport-layer failures (StatusCode 0) that never reached the
// server at all: dial errors, DNS misses, TLS handshake failures, client
// timeouts. Metabase itself never returns status 0, so StatusCode == 0 IS
// the transport-error signal Kind() and Error() switch on.
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	Body       string
	// Cause holds the underlying transport error for StatusCode == 0.
	Cause error
}

func (e *APIError) Error() string {
	if e.StatusCode == 0 {
		if e.Cause != nil {
			return fmt.Sprintf("%s %s: %v", e.Method, e.URL, e.Cause)
		}
		return fmt.Sprintf("%s %s: transport error", e.Method, e.URL)
	}
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.StatusCode, body)
}

// Unwrap exposes Cause so errors.Is/errors.As reach the underlying
// transport error through this APIError, same as any other %w link.
func (e *APIError) Unwrap() error { return e.Cause }

// Unauthorized reports 401/403.
func (e *APIError) Unauthorized() bool { return e.StatusCode == 401 || e.StatusCode == 403 }

// NotFound reports 404.
func (e *APIError) NotFound() bool { return e.StatusCode == 404 }

// ExitCode delegates to the fleet-canonical table (auth=2, validation=3,
// not_found=4, rate_limited=5, else 1). Previously absent — every failure exited
// 1; main.go dispatches this via the ExitCode() interface.
func (e *APIError) ExitCode() int {
	return cierrors.ExitCodeFor(e.StatusCode, "")
}

// Kind classifies the error for the structured A3 envelope. Drives
// agent-side dispatch on retry strategy.
//
// Taxonomy: auth_failed, not_found, rate_limited, server_error, timeout,
// network, unknown_column, unknown_table, client_error. The taxonomy doc
// comment promised timeout/network since v0.1; W7 is what actually makes
// classifyTransportErr produce them — before it, every dial failure, DNS
// miss, or client timeout fell through client.go's do() as a bare
// "max retries exceeded: %w" with no Kind() at all, landing here as the
// "client_error" default via the caller's own fallback.
func (e *APIError) Kind() string {
	switch {
	case e.StatusCode == 401, e.StatusCode == 403:
		return "auth_failed"
	case e.StatusCode == 404:
		return "not_found"
	case e.StatusCode == 429:
		return "rate_limited"
	case e.StatusCode >= 500:
		return "server_error"
	case e.StatusCode == 0:
		return classifyTransportErr(e.Cause)
	case strings.Contains(e.Body, "Unknown column"):
		return "unknown_column"
	case strings.Contains(e.Body, "doesn't exist") || strings.Contains(e.Body, "Table"):
		return "unknown_table"
	default:
		return "client_error"
	}
}

// classifyTransportErr maps a transport-layer failure to "timeout" or
// "network" — falling back to "client_error" for anything unrecognized
// (kept as the residual, not a catch-all name change, so callers don't
// need a third bucket).
func classifyTransportErr(cause error) string {
	if cause == nil {
		return "client_error"
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(cause, syscall.ECONNREFUSED) {
		return "network"
	}
	var dnsErr *net.DNSError
	if errors.As(cause, &dnsErr) {
		return "network"
	}
	var opErr *net.OpError
	if errors.As(cause, &opErr) {
		return "network"
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(cause, &tlsErr) {
		return "network"
	}
	if errors.Is(cause, io.ErrUnexpectedEOF) {
		// The connection dropped mid-body (io.ReadAll got fewer bytes than
		// Content-Length promised, or the chunked stream ended early) —
		// same "the network, not the server, failed" bucket as a dial
		// error or a reset connection.
		return "network"
	}
	return "client_error"
}

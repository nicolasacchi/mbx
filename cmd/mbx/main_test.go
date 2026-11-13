package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/mbx/internal/client"
)

// TestExitCode_UnwrapsWrappedAPIError pins the dispatch end-to-end: a *APIError
// yields its canonical exit code whether returned bare or %w-wrapped (the
// retry-exhausted and "fetch …" command paths wrap it), and non-API errors → 1.
func TestExitCode_UnwrapsWrappedAPIError(t *testing.T) {
	base := &client.APIError{StatusCode: 429}
	if got := exitCode(base); got != 5 {
		t.Errorf("bare 429: got %d, want 5", got)
	}
	if got := exitCode(fmt.Errorf("max retries exceeded: %w", base)); got != 5 {
		t.Errorf("wrapped 429: got %d, want 5", got)
	}
	if got := exitCode(&client.APIError{StatusCode: 404}); got != 4 {
		t.Errorf("404: got %d, want 4", got)
	}
	if got := exitCode(errors.New("plain")); got != 1 {
		t.Errorf("plain error: got %d, want 1", got)
	}
}

// TestExitCode_WriteLocked pins Wave 4's confirm-gate migration: a refused
// write (clicore/confirm.Require/Gate) returns a *cierrors.APIError{Kind:
// "write_locked"} — a different concrete type than mbx's own
// *client.APIError — and it must still map to exit 6 through the SAME
// errors.As probe, bare or wrapped, with no main.go changes required.
func TestExitCode_WriteLocked(t *testing.T) {
	base := &cierrors.APIError{Kind: "write_locked", Detail: "dashboards create requires confirmation"}
	if got := exitCode(base); got != 6 {
		t.Errorf("bare write_locked: got %d, want 6", got)
	}
	if got := exitCode(fmt.Errorf("gate: %w", base)); got != 6 {
		t.Errorf("wrapped write_locked: got %d, want 6", got)
	}
}

package commands

// Wave 4 — confirm gate upgrade. gate() (root.go) delegates to
// clicore/confirm, so a refusal is a *cierrors.APIError{Kind:"write_locked"}
// (exit 6 via main.go's existing errors.As probe) instead of the old bare
// fmt.Errorf (exit 1) the now-deleted requireConfirm used to return. These
// tests pin the gate logic itself (via dashboardsCreateCmd, the simplest
// write command) plus --dry-run's zero-network-writes contract.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nicolasacchi/clicore/cierrors"
)

// captureStdout redirects os.Stdout for the duration of a test — several
// write RunE paths end in output.PrintData, which writes directly to
// os.Stdout rather than cmd.OutOrStdout(), so a passing write test would
// otherwise leak its JSON response into the test log. Returns a stop
// function callers can invoke mid-test to flush the pipe and read what's
// been written so far (the copy runs on a separate goroutine, so reading the
// buffer directly without stopping first is a data race). Idempotent —
// safe to call once explicitly and let the auto-registered t.Cleanup call it
// again at teardown.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, r)
		close(done)
	}()
	var stopped bool
	stop := func() string {
		if !stopped {
			stopped = true
			os.Stdout = orig
			w.Close()
			<-done
		}
		return buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

// resetGateFlags restores the confirm-gate globals and dashboardsCreateCmd's
// flag vars after a test mutates them directly (same pattern as
// resetCardsExportFlags).
func resetGateFlags(t *testing.T) {
	t.Helper()
	origYes, origDryRun := yesFlag, dryRunFlag
	origName, origDesc, origColl := dashboardCreateName, dashboardCreateDescription, dashboardCreateCollection
	t.Cleanup(func() {
		yesFlag, dryRunFlag = origYes, origDryRun
		dashboardCreateName, dashboardCreateDescription, dashboardCreateCollection = origName, origDesc, origColl
	})
}

func writeCounterServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			writes++
		}
		w.Write([]byte(`{"id":1,"name":"x"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

// TestGate_RefusalExitsSix pins the core migration: no --yes, no --dry-run,
// no MBX_CONFIRM → refusal, and the refusal's ExitCode() is 6 (write_locked)
// via clicore's cierrors.APIError, not the old bare-error exit 1. Zero
// writes must reach the server — the gate runs before any network call.
func TestGate_RefusalExitsSix(t *testing.T) {
	resetGateFlags(t)
	srv, writes := writeCounterServer(t)
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	t.Cleanup(func() { urlFlag, apiKeyFlag = origURL, origKey })

	yesFlag, dryRunFlag = false, false
	dashboardCreateName = "mbx-smoke-gate-test"

	err := dashboardsCreateCmd.RunE(dashboardsCreateCmd, nil)
	if err == nil {
		t.Fatal("expected a refusal error without --yes")
	}
	var apiErr *cierrors.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected a *cierrors.APIError, got %T: %v", err, err)
	}
	if apiErr.Kind != "write_locked" {
		t.Errorf("Kind = %q, want write_locked", apiErr.Kind)
	}
	if got := apiErr.ExitCode(); got != 6 {
		t.Errorf("ExitCode() = %d, want 6", got)
	}
	if *writes != 0 {
		t.Errorf("expected 0 write requests on refusal, got %d", *writes)
	}
}

// TestGate_YesFlagProceeds pins that --yes lets the write through.
func TestGate_YesFlagProceeds(t *testing.T) {
	resetGateFlags(t)
	srv, writes := writeCounterServer(t)
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	t.Cleanup(func() { urlFlag, apiKeyFlag = origURL, origKey })

	yesFlag, dryRunFlag = true, false
	dashboardCreateName = "mbx-smoke-gate-test"
	captureStdout(t)

	if err := dashboardsCreateCmd.RunE(dashboardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE with --yes: %v", err)
	}
	if *writes != 1 {
		t.Errorf("expected exactly 1 write request with --yes, got %d", *writes)
	}
}

// TestGate_MBXConfirmEnvProceeds pins the MBX_CONFIRM=1 env fallback,
// preserved from the pre-Wave-4 behavior.
func TestGate_MBXConfirmEnvProceeds(t *testing.T) {
	resetGateFlags(t)
	srv, writes := writeCounterServer(t)
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	t.Cleanup(func() { urlFlag, apiKeyFlag = origURL, origKey })

	yesFlag, dryRunFlag = false, false
	dashboardCreateName = "mbx-smoke-gate-test"
	t.Setenv("MBX_CONFIRM", "1")
	captureStdout(t)

	if err := dashboardsCreateCmd.RunE(dashboardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE with MBX_CONFIRM=1: %v", err)
	}
	if *writes != 1 {
		t.Errorf("expected exactly 1 write request with MBX_CONFIRM=1, got %d", *writes)
	}
}

// TestGate_DryRunPrintsAndSkipsNetwork pins --dry-run's contract: it prints
// the would-be request to the command's output and makes ZERO network
// requests at all (not even a GET) for a create command that needs no
// preliminary read.
func TestGate_DryRunPrintsAndSkipsNetwork(t *testing.T) {
	resetGateFlags(t)
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	t.Cleanup(func() { urlFlag, apiKeyFlag = origURL, origKey })

	yesFlag, dryRunFlag = false, true
	dashboardCreateName = "mbx-smoke-gate-test"

	var buf bytes.Buffer
	dashboardsCreateCmd.SetOut(&buf)
	t.Cleanup(func() { dashboardsCreateCmd.SetOut(nil) })

	if err := dashboardsCreateCmd.RunE(dashboardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE with --dry-run: %v", err)
	}
	if writes != 0 {
		t.Errorf("expected 0 network requests with --dry-run, got %d", writes)
	}
	out := buf.String()
	for _, want := range []string{"--dry-run", "POST /api/dashboard", "mbx-smoke-gate-test"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the dry-run preview to contain %q, got: %q", want, out)
		}
	}
}

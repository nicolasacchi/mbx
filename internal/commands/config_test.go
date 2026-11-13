package commands

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/config"
)

// hijackAndReset forcibly resets the TCP connection for the current request
// (RST via SetLinger(0), not a graceful FIN) — the shape client.go's do()
// classifies as "network" (a *net.OpError), unlike a plain closed connection
// which surfaces as an unclassified io.EOF. Used to simulate a network-class
// failure for exactly ONE endpoint on an otherwise-healthy httptest server,
// where dial-refused/DNS tricks can't be scoped to a single path.
func hijackAndReset(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// isolateConfigEnv points config resolution at a throwaway dir and clears
// ambient METABASE_*/flag state so a developer's real config/shell can't
// leak into a test. Mirrors internal/config's unexported isolate() helper —
// commands-package tests need their own copy since it's not exported.
func isolateConfigEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvAPIKey, "")
	t.Setenv(config.EnvWriteAPIKey, "")
	t.Setenv(config.EnvDefaultDB, "")

	origURL, origKey, origProject := urlFlag, apiKeyFlag, projectFlag
	urlFlag, apiKeyFlag, projectFlag = "", "", ""
	t.Cleanup(func() { urlFlag, apiKeyFlag, projectFlag = origURL, origKey, origProject })
}

func captureConfigStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestAddProjectToConfig_RoundTrip pins W6's `config add` core: writing via
// the already-atomic Save/Load, and the "first project becomes default"
// rule (only the first — a second add must not steal the default).
func TestAddProjectToConfig_RoundTrip(t *testing.T) {
	isolateConfigEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	isFirst, replaced := addProjectToConfig(cfg, "staging", config.Project{
		URL:    "https://metabase-staging.example.com",
		APIKey: "mb_staging",
	})
	if !isFirst {
		t.Error("the first project added must report isFirst=true")
	}
	if replaced {
		t.Error("a brand-new project must not report replaced=true")
	}
	if cfg.DefaultProject != "staging" {
		t.Errorf("DefaultProject = %q, want staging (first project auto-defaults)", cfg.DefaultProject)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	proj, ok := reloaded.Projects["staging"]
	if !ok {
		t.Fatal("project 'staging' did not round-trip through Save/Load")
	}
	if proj.URL != "https://metabase-staging.example.com" || proj.APIKey != "mb_staging" {
		t.Errorf("round-tripped project = %+v", proj)
	}

	// A second, differently-named project must NOT steal the default.
	isFirstSecond, replacedSecond := addProjectToConfig(reloaded, "prod2", config.Project{URL: "https://x", APIKey: "k"})
	if isFirstSecond {
		t.Error("the second project added must report isFirst=false")
	}
	if replacedSecond {
		t.Error("a brand-new second project must not report replaced=true")
	}
	if reloaded.DefaultProject != "staging" {
		t.Errorf("DefaultProject changed to %q on second add, want staging preserved", reloaded.DefaultProject)
	}
}

// TestAddProjectToConfig_ReplacesExisting pins the review fix: re-adding an
// already-present project name used to silently overwrite it while still
// reporting "added" — the caller must be able to tell replaced from added
// so it can print an honest message.
func TestAddProjectToConfig_ReplacesExisting(t *testing.T) {
	cfg := &config.Config{}

	_, replaced := addProjectToConfig(cfg, "staging", config.Project{URL: "https://old.example.com", APIKey: "old-key"})
	if replaced {
		t.Fatal("the first add of 'staging' must not report replaced=true")
	}

	_, replaced = addProjectToConfig(cfg, "staging", config.Project{URL: "https://new.example.com", APIKey: "new-key"})
	if !replaced {
		t.Error("re-adding an existing project name must report replaced=true")
	}
	if got := cfg.Projects["staging"]; got.URL != "https://new.example.com" || got.APIKey != "new-key" {
		t.Errorf("the second add must overwrite the entry, got %+v", got)
	}
}

func TestValidateConfigURLScheme(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://metabase.example.com", false},
		{"http://localhost:3000", false},
		{"http://LOCALHOST:3000", false}, // hostname compare must be case-insensitive
		{"http://127.0.0.1:3000", false},
		{"http://metabase.example.com", true}, // http on a non-local host — SEC-001
		{"ftp://metabase.example.com", true},
		{"metabase.example.com", true}, // no scheme at all
	}
	for _, tc := range cases {
		err := validateConfigURLScheme(tc.url)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateConfigURLScheme(%q) err=%v, wantErr=%v", tc.url, err, tc.wantErr)
		}
	}
}

// TestValidateConfigURLScheme_ExitCodeMatchesLoadCredentials is a live-smoke-
// caught regression test: `config add`'s scheme check (this file, via
// validateConfigURLScheme) and LoadCredentials's scheme check (config
// package) enforce the identical rule but used to return DIFFERENT error
// shapes — validateConfigURLScheme returned a plain fmt.Errorf (exit 1),
// LoadCredentials wrapped the same failure in a typed
// *config.InvalidURLSchemeError (exit 3) — so `mbx config add x --url
// ftp://...` and a `METABASE_URL=ftp://...` env var hitting the same
// violation exited with DIFFERENT codes for the same mistake. Both now
// delegate to config.ValidateURLScheme, which returns the typed error
// itself, so both paths agree.
func TestValidateConfigURLScheme_ExitCodeMatchesLoadCredentials(t *testing.T) {
	err := validateConfigURLScheme("ftp://metabase.example")
	if err == nil {
		t.Fatal("expected an error for a non-https/non-local scheme")
	}
	ec, ok := err.(interface{ ExitCode() int })
	if !ok {
		t.Fatalf("validateConfigURLScheme's error is not typed with ExitCode(): %T", err)
	}
	if ec.ExitCode() != 3 {
		t.Errorf("ExitCode() = %d, want 3 (same as LoadCredentials's equivalent rejection)", ec.ExitCode())
	}
	if kindOf(err, "") != "validation" {
		t.Errorf("expected kind=validation, got %v", err)
	}
}

// TestRunConfigDoctor_JSONAuthMissing pins the doctor --json golden path:
// unset credentials must yield a single JSON object on stdout with
// kind:"auth_missing" and ok:false, plus a typed error whose ExitCode()==2
// (the fleet auth exit code) rather than the generic 1 the bare
// ErrNoCredentials sentinel used to produce.
func TestRunConfigDoctor_JSONAuthMissing(t *testing.T) {
	isolateConfigEnv(t)

	var runErr error
	stdout := captureConfigStdout(t, func() {
		runErr = runConfigDoctor(true, false)
	})

	if runErr == nil {
		t.Fatal("expected an error for missing credentials")
	}
	ec, ok := runErr.(interface{ ExitCode() int })
	if !ok || ec.ExitCode() != 2 {
		t.Errorf("expected a typed error with ExitCode()==2, got %v (%T)", runErr, runErr)
	}

	if gjson.Get(stdout, "kind").String() != "auth_missing" {
		t.Errorf("expected kind=auth_missing in JSON output, got: %s", stdout)
	}
	if gjson.Get(stdout, "ok").Bool() {
		t.Errorf("expected ok=false in JSON output, got: %s", stdout)
	}
	if gjson.Get(stdout, "remediation").String() == "" {
		t.Errorf("expected a non-empty remediation, got: %s", stdout)
	}
}

// TestRunConfigDoctor_JSONGenericGetClientErrorStillEmitsJSON pins the
// review fix: ANY getClient() failure — not just the missing-credentials
// one — must still emit exactly one JSON object to stdout. Before the fix,
// an unknown --project (or a malformed --timeout) hit a bare `return err`
// with nothing printed, breaking the "config doctor --json always emits
// one JSON object" contract downstream scripts rely on.
func TestRunConfigDoctor_JSONGenericGetClientErrorStillEmitsJSON(t *testing.T) {
	isolateConfigEnv(t)
	// LoadCredentials only validates --project against a config file that
	// exists — with no file at all it's a silent no-op — so write one with
	// a DIFFERENT project to actually exercise the unknown-project branch.
	if err := config.Save(&config.Config{Projects: map[string]config.Project{"other": {URL: "https://x", APIKey: "k"}}}); err != nil {
		t.Fatal(err)
	}
	origProject := projectFlag
	projectFlag = "does-not-exist"
	t.Cleanup(func() { projectFlag = origProject })

	var runErr error
	stdout := captureConfigStdout(t, func() {
		runErr = runConfigDoctor(true, false)
	})

	if runErr == nil {
		t.Fatal("expected an error for an unknown --project")
	}
	if !gjson.Valid(stdout) {
		t.Fatalf("expected exactly one valid JSON object on stdout, got: %q", stdout)
	}
	if gjson.Get(stdout, "ok").Bool() {
		t.Errorf("expected ok=false, got: %s", stdout)
	}
	if !strings.Contains(gjson.Get(stdout, "message").String(), "not found") {
		t.Errorf("expected the unknown-project message, got: %s", stdout)
	}
	if gjson.Get(stdout, "kind").String() == "" {
		t.Errorf("expected a non-empty kind, got: %s", stdout)
	}
}

// TestRunConfigDoctor_RequireGatesOnPermissionProbe pins --require's one new
// behavior: a permission-group-probe failure (403) is advisory without
// --require (doctor still exits 0) but fatal with it, via a typed
// *client.APIError so the exit code is the fleet-standard 2.
func TestRunConfigDoctor_RequireGatesOnPermissionProbe(t *testing.T) {
	isolateConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/current":
			_, _ = w.Write([]byte(`{"id":1,"email":"x@example.com"}`))
		case "/api/permissions/group":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	urlFlag, apiKeyFlag = srv.URL, "test-key"

	// Without --require: a permission-probe failure is advisory, doctor succeeds.
	_ = captureConfigStdout(t, func() {
		if err := runConfigDoctor(true, false); err != nil {
			t.Errorf("without --require, a permission-probe failure must not be fatal: %v", err)
		}
	})

	// With --require: the same failure becomes fatal with the typed exit code.
	var requireErr error
	requireOut := captureConfigStdout(t, func() {
		requireErr = runConfigDoctor(true, true)
	})
	if requireErr == nil {
		t.Fatal("expected --require to fail on a permission-probe error")
	}
	var apiErr *client.APIError
	if !errors.As(requireErr, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", requireErr, requireErr)
	}
	if apiErr.ExitCode() != 2 {
		t.Errorf("ExitCode() = %d, want 2", apiErr.ExitCode())
	}
	if gjson.Get(requireOut, "ok").Bool() {
		t.Errorf("expected ok=false with --require, got: %s", requireOut)
	}
}

// TestRunConfigDoctor_RequireGatesOnPermissionProbe_NetworkFailure is the
// carried-finding-A regression test: before W7, a permission-group-probe
// failure that was NOT an *APIError (e.g. a raw transport error) fell
// through to a fabricated &client.APIError{StatusCode:403} — misreporting a
// connectivity blip as kind "not_authorized"/exit 2. Post-W7, client.go
// types transport failures as *APIError too, so the same probe failure must
// now surface honestly as kind "network"/exit 1 with the real error text.
func TestRunConfigDoctor_RequireGatesOnPermissionProbe_NetworkFailure(t *testing.T) {
	isolateConfigEnv(t)
	// hijackAndReset is a connection-RESET, which do() retries up to
	// maxRetries (same as connection-refused) before giving up — production
	// pays the full ~7.5s exponential backoff for that. Same fast tuning as
	// client_test.go's connection-refused test (W5 carried review item):
	// Kind()/ExitCode() below are computed from the SAME classifyTransportErr
	// path regardless of how many attempts it took to get there.
	defer client.SetRetryTuningForTests(1, time.Millisecond)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/user/current":
			_, _ = w.Write([]byte(`{"id":1,"email":"x@example.com"}`))
		case "/api/permissions/group":
			hijackAndReset(w) // network-class failure, NOT a 403
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	urlFlag, apiKeyFlag = srv.URL, "test-key"

	var requireErr error
	requireOut := captureConfigStdout(t, func() {
		requireErr = runConfigDoctor(true, true)
	})
	if requireErr == nil {
		t.Fatal("expected --require to fail on a network-class permission-probe error")
	}
	var apiErr *client.APIError
	if !errors.As(requireErr, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", requireErr, requireErr)
	}
	if apiErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (the REAL transport error, not a fabricated 403)", apiErr.StatusCode)
	}
	if apiErr.ExitCode() != 1 {
		t.Errorf("ExitCode() = %d, want 1 (network-class failures don't get the auth exit code)", apiErr.ExitCode())
	}
	if gjson.Get(requireOut, "kind").String() != "network" {
		t.Errorf("expected kind=network, got: %s", requireOut)
	}
	if gjson.Get(requireOut, "kind").String() == "not_authorized" {
		t.Error("a connectivity blip must not be misreported as not_authorized")
	}
}

// TestRunConfigDoctor_HealthProbeUnreachable pins W7's doctor triage: when
// creds are present and valid-shaped but the unauthenticated /api/health
// probe itself can't be reached, doctor must report kind "network" (not
// "auth_failed") — before this, a pure connectivity failure (sandbox block,
// DNS miss) fell through to the /api/user/current check and got
// misdiagnosed as an auth problem when the credentials were never even the
// issue.
func TestRunConfigDoctor_HealthProbeUnreachable(t *testing.T) {
	isolateConfigEnv(t)
	// Same fast retry tuning as the two tests above (W5 carried review item)
	// — connection-refused against /api/health is otherwise retried the full
	// ~7.5s before doctor reports the network-class failure this test pins.
	defer client.SetRetryTuningForTests(1, time.Millisecond)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // nothing listens here anymore -> connection refused
	urlFlag, apiKeyFlag = deadURL, "test-key"

	var runErr error
	stdout := captureConfigStdout(t, func() {
		runErr = runConfigDoctor(true, false)
	})

	if runErr == nil {
		t.Fatal("expected an error when /api/health is unreachable")
	}
	var apiErr *client.APIError
	if !errors.As(runErr, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", runErr, runErr)
	}
	if apiErr.ExitCode() != 1 {
		t.Errorf("ExitCode() = %d, want 1", apiErr.ExitCode())
	}
	if gjson.Get(stdout, "kind").String() != "network" {
		t.Errorf("expected kind=network, got: %s", stdout)
	}
	if gjson.Get(stdout, "ok").Bool() {
		t.Errorf("expected ok=false, got: %s", stdout)
	}
	if gjson.Get(stdout, "remediation").String() == "" {
		t.Errorf("expected a non-empty remediation, got: %s", stdout)
	}
}

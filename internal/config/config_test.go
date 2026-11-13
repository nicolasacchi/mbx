package config

import (
	"errors"
	"os"
	"testing"
)

// isolate points config resolution at a throwaway dir and neutralizes ambient
// METABASE_* env vars, so a developer's real shell/config can't taint the test.
func isolate(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv(EnvURL, "")
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvWriteAPIKey, "")
	t.Setenv(EnvDefaultDB, "")
}

func TestLoadCredentials_FlagBeatsEnv(t *testing.T) {
	isolate(t)
	t.Setenv(EnvURL, "https://env.example")
	t.Setenv(EnvAPIKey, "env-key")
	t.Setenv(EnvWriteAPIKey, "env-write")

	c, err := LoadCredentials("https://flag.example/", "flag-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://flag.example" { // flag wins; trailing slash trimmed
		t.Errorf("URL = %q, want flag value (slash-trimmed)", c.URL)
	}
	if c.APIKey != "flag-key" {
		t.Errorf("APIKey = %q, want flag-key", c.APIKey)
	}
	if c.WriteAPIKey != "env-write" { // no write flag exists → env supplies it
		t.Errorf("WriteAPIKey = %q, want env-write", c.WriteAPIKey)
	}
}

func TestLoadCredentials_EnvWhenNoFlag(t *testing.T) {
	isolate(t)
	t.Setenv(EnvURL, "https://env.example")
	t.Setenv(EnvAPIKey, "env-key")

	c, err := LoadCredentials("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://env.example" || c.APIKey != "env-key" {
		t.Errorf("env values expected, got %+v", c)
	}
	if c.WriteAPIKey != "" { // unset write key stays empty (client falls back to APIKey)
		t.Errorf("WriteAPIKey = %q, want empty", c.WriteAPIKey)
	}
}

func TestLoadCredentials_MissingErrors(t *testing.T) {
	isolate(t)
	if _, err := LoadCredentials("", "", ""); err != ErrNoCredentials {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
}

// TestNoCredentialsError_Typed pins the W6 contract: NoCredentialsError
// implements Kind()/ExitCode() so main.go's errors.As probe maps a missing-
// credentials failure to the fleet auth exit code (2), and the shared
// ErrNoCredentials sentinel still satisfies errors.Is for any call site that
// predates the typed error.
func TestNoCredentialsError_Typed(t *testing.T) {
	isolate(t)
	_, err := LoadCredentials("", "", "")

	var nc *NoCredentialsError
	if !errors.As(err, &nc) {
		t.Fatalf("expected *NoCredentialsError, got %T", err)
	}
	if nc.Kind() != "auth_missing" {
		t.Errorf("Kind() = %q, want auth_missing", nc.Kind())
	}
	if nc.ExitCode() != 2 {
		t.Errorf("ExitCode() = %d, want 2", nc.ExitCode())
	}
	if !errors.Is(err, ErrNoCredentials) {
		t.Error("errors.Is(err, ErrNoCredentials) must still hold for the typed error")
	}

	var ec interface{ ExitCode() int }
	if !errors.As(err, &ec) || ec.ExitCode() != 2 {
		t.Error("errors.As against the generic ExitCode() interface must yield 2 (main.go's dispatch mechanism)")
	}
}

func TestLoadCredentials_UnknownProjectErrors(t *testing.T) {
	isolate(t)
	// The unknown-project error only fires when a config file exists but lacks
	// the named project, so write one with a different project first.
	if err := Save(&Config{Projects: map[string]Project{"p": {URL: "https://x", APIKey: "k"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials("", "", "does-not-exist"); err == nil {
		t.Fatal("an explicit --project that is absent from config must error")
	}
}

// TestLoadCredentials_SchemeGuard pins W14's extension of SEC-001 (originally
// `config add`-only, Wave 2) to LoadCredentials itself — every credential-
// resolution path (flag, env, project block) now enforces the scheme guard,
// not just the one that writes a new config entry.
func TestLoadCredentials_SchemeGuard(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https ok", "https://metabase.example.com", false},
		{"http+localhost ok", "http://localhost:3000", false},
		{"http+localhost mixed case ok", "http://LOCALHOST:3000", false},
		{"http+127.0.0.1 ok", "http://127.0.0.1:3000", false},
		{"http+remote rejected", "http://metabase.example.com", true},
		{"ftp rejected", "ftp://metabase.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			c, err := LoadCredentials(tc.url, "key", "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadCredentials(%q) err=%v, wantErr=%v", tc.url, err, tc.wantErr)
			}
			if tc.wantErr {
				var schemeErr *InvalidURLSchemeError
				if !errors.As(err, &schemeErr) {
					t.Fatalf("expected *InvalidURLSchemeError, got %T: %v", err, err)
				}
				if schemeErr.Kind() != "validation" {
					t.Errorf("Kind() = %q, want validation", schemeErr.Kind())
				}
				if schemeErr.ExitCode() != 3 {
					t.Errorf("ExitCode() = %d, want 3", schemeErr.ExitCode())
				}
			} else if c == nil {
				t.Fatal("expected non-nil Credentials on success")
			}
		})
	}
}

// TestSave_FileIs0600 pins the secret-at-rest perm: the config holds API keys.
func TestSave_FileIs0600(t *testing.T) {
	isolate(t)
	cfg := &Config{
		DefaultProject: "p",
		Projects:       map[string]Project{"p": {URL: "https://x", APIKey: "k"}},
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perms = %o, want 600", perm)
	}
}

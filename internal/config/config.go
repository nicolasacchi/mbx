// Package config loads multi-instance Metabase credentials from
// ~/.config/mbx/config.toml and resolves them per-field
// (flag > env > project block > default_project).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	cliconfig "github.com/nicolasacchi/clicore/config"
)

const (
	EnvURL          = "METABASE_URL"
	EnvAPIKey       = "METABASE_API_KEY"
	EnvWriteAPIKey  = "MBX_WRITE_API_KEY" // optional separate key for write commands
	EnvDefaultDB    = "MBX_DEFAULT_DB"
	DefaultDatabase = 2 // default --database id when neither flag, env nor project default_db is set
)

// Credentials is the resolved set used by the HTTP client.
type Credentials struct {
	URL         string
	APIKey      string
	WriteAPIKey string // optional; falls back to APIKey if empty
	DefaultDB   int
}

// Project is one entry in config.toml under [projects.<name>].
type Project struct {
	URL         string `toml:"url"`
	APIKey      string `toml:"api_key"`
	WriteAPIKey string `toml:"write_api_key"`
	DefaultDB   int    `toml:"default_db"`
}

// Config is the on-disk shape of ~/.config/mbx/config.toml.
type Config struct {
	DefaultProject string             `toml:"default_project"`
	Projects       map[string]Project `toml:"projects"`
}

// NoCredentialsError is returned when neither flag, env, nor config provides
// a URL or API key. Typed (W6) so main.go's errors.As probe
// (interface{ ExitCode() int }) maps it to the fleet-standard auth exit code
// (2) instead of the generic 1 every untyped error gets, and so
// `config doctor --json` can report kind:"auth_missing" without string-
// matching stderr.
type NoCredentialsError struct{}

func (e *NoCredentialsError) Error() string {
	return "METABASE_URL and METABASE_API_KEY required: set env vars or run 'mbx config add <name> --url <url> --api-key <key>'"
}

// Kind classifies the error for the structured A3 envelope — matches
// client.APIError's taxonomy (auth_failed, not_found, …) with a distinct
// value since this fires before any HTTP call is even attempted.
func (e *NoCredentialsError) Kind() string { return "auth_missing" }

// ExitCode maps to the fleet auth exit code (2) — same bucket as a live
// 401/403, since "no credentials" and "credentials rejected" are both auth
// failures from the operator's perspective.
func (e *NoCredentialsError) ExitCode() int { return 2 }

// InvalidURLSchemeError wraps a ValidateURLScheme failure surfaced from
// LoadCredentials (W14 — the scheme guard commands/config.go's `config add`
// already enforced now also covers every OTHER way a URL reaches the
// client: a hand-edited config.toml, METABASE_URL, --url). Typed so main.go's
// errors.As(interface{ ExitCode() int }) probe maps it to the fleet
// validation exit code (3) instead of the generic 1 an untyped error gets.
type InvalidURLSchemeError struct {
	msg string
}

func (e *InvalidURLSchemeError) Error() string { return e.msg }
func (e *InvalidURLSchemeError) Kind() string  { return "validation" }
func (e *InvalidURLSchemeError) ExitCode() int { return 3 }

// ValidateURLScheme enforces SEC-001: only https:// is accepted for a
// remote host; http:// is allowed exclusively for localhost/127.0.0.1/::1
// (local Metabase dev instances / httptest-backed tests), hostname compared
// case-insensitively. Originally landed early (Wave 2) as a commands-package
// helper used only by `config add`; moved here (W14) as the single shared
// implementation so LoadCredentials can enforce it on every credential-
// resolution path, not just the one that writes a new config entry — a
// hand-edited config.toml or an env var pointing at a non-https host used
// to sail through unchecked.
//
// Returns a typed *InvalidURLSchemeError (not a plain error) so every
// caller — `config add`, LoadCredentials, any future one — gets the fleet
// validation exit code (3) for free instead of each needing its own wrap
// (a review-caught gap: `config add` originally called this, got a PLAIN
// error back, and exited 1 while LoadCredentials's separately-wrapped
// version of the identical failure exited 3 for the same underlying cause).
func ValidateURLScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &InvalidURLSchemeError{msg: fmt.Sprintf("invalid url %q: %v", raw, err)}
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return &InvalidURLSchemeError{msg: fmt.Sprintf("url %q uses http:// for a non-local host %q — use https:// (http:// is only allowed for localhost/127.0.0.1)", raw, u.Hostname())}
	default:
		return &InvalidURLSchemeError{msg: fmt.Sprintf("url %q must use https:// (or http:// for localhost) — got scheme %q", raw, u.Scheme)}
	}
}

// ErrNoCredentials is the single shared *NoCredentialsError instance,
// returned unchanged by LoadCredentials — kept as a package var (rather than
// constructing a fresh error each time) so both direct equality checks
// (err == ErrNoCredentials, used by existing tests) and errors.Is/errors.As
// keep working against the one pre-typed sentinel.
var ErrNoCredentials error = &NoCredentialsError{}

// LoadCredentials resolves credentials per-field from flag > env > project > default_project.
func LoadCredentials(urlFlag, apiKeyFlag, projectFlag string) (*Credentials, error) {
	creds := &Credentials{}

	cfg, _ := Load()

	var proj *Project
	if cfg != nil {
		name := projectFlag
		if name == "" {
			name = cfg.DefaultProject
		}
		if name != "" {
			if p, ok := cfg.Projects[name]; ok {
				proj = &p
			} else if projectFlag != "" {
				return nil, fmt.Errorf("project %q not found in config; available: %v", projectFlag, projectNames(cfg.Projects))
			}
		}
	}

	creds.URL = firstNonEmpty(urlFlag, os.Getenv(EnvURL), projectField(proj, func(p *Project) string { return p.URL }))
	creds.APIKey = firstNonEmpty(apiKeyFlag, os.Getenv(EnvAPIKey), projectField(proj, func(p *Project) string { return p.APIKey }))
	creds.WriteAPIKey = firstNonEmpty(os.Getenv(EnvWriteAPIKey), projectField(proj, func(p *Project) string { return p.WriteAPIKey }))

	creds.DefaultDB = DefaultDatabase
	if v := os.Getenv(EnvDefaultDB); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			creds.DefaultDB = n
		}
	} else if proj != nil && proj.DefaultDB > 0 {
		creds.DefaultDB = proj.DefaultDB
	}

	if creds.URL == "" || creds.APIKey == "" {
		return nil, ErrNoCredentials
	}
	creds.URL = strings.TrimRight(creds.URL, "/")

	// W14 — enforce the scheme guard on every resolution path (flag, env,
	// project block), not just `config add`'s write path. ValidateURLScheme
	// already returns a typed *InvalidURLSchemeError, so no extra wrap here.
	if err := ValidateURLScheme(creds.URL); err != nil {
		return nil, err
	}

	return creds, nil
}

// Load reads ~/.config/mbx/config.toml. Missing file returns (nil, nil).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Projects == nil {
		cfg.Projects = map[string]Project{}
	}
	return &cfg, nil
}

// Save writes the config to ~/.config/mbx/config.toml (mode 0600) atomically
// (temp+rename via clicore) so an interrupted encode can't corrupt a config
// that already holds credentials — replaces the prior O_TRUNC write.
func Save(cfg *Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return cliconfig.SaveTOML(path, cfg)
}

// Path returns the config file path: $XDG_CONFIG_HOME/mbx/config.toml or ~/.config/mbx/config.toml.
func Path() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "mbx", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mbx", "config.toml"), nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func projectField(p *Project, get func(*Project) string) string {
	if p == nil {
		return ""
	}
	return get(p)
}

func projectNames(m map[string]Project) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

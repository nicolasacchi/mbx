package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage mbx configuration",
}

var configDoctorRequireFlag bool

var configDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Verify METABASE_URL + METABASE_API_KEY work; report key permissions",
	Long: `Verifies credentials resolve and the API key authenticates against Metabase.

Always prints one JSON object to stdout summarizing the result — on failure
this now includes cases that used to print nothing on stdout (missing
credentials, a failed auth check). --json additionally suppresses the
human-readable stderr preamble (URL / key / default DB lines), for clean
piping into scripts.

The object carries "ok": true/false and, on failure, "kind" (e.g.
"auth_missing", "auth_failed", "not_authorized") plus a one-line
"remediation" or "message".

--require promotes the permission-group probe (normally advisory — reported
but not fatal, since not every key needs group-list access) to a hard
failure alongside the pre-existing missing-credentials / auth-check-failure
cases, so scripts can gate on exit code alone: "mbx config doctor --require"
exits 2 for any auth-related problem, 0 only when everything checks out.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigDoctor(jsonFlag, configDoctorRequireFlag)
	},
}

// doctorResult is the single JSON object `config doctor` always emits to
// stdout. omitempty keeps success/failure envelopes lean in either direction.
type doctorResult struct {
	OK                      bool     `json:"ok"`
	Kind                    string   `json:"kind,omitempty"`
	Message                 string   `json:"message,omitempty"`
	Remediation             string   `json:"remediation,omitempty"`
	ConfigPath              string   `json:"config_path,omitempty"`
	URL                     string   `json:"url,omitempty"`
	DefaultDB               int      `json:"default_db,omitempty"`
	HasWriteKey             bool     `json:"has_write_key,omitempty"`
	User                    any      `json:"user,omitempty"`
	CanListPermissionGroups *bool    `json:"can_list_permission_groups,omitempty"`
	PermissionGroups        []string `json:"permission_groups,omitempty"`
}

func printDoctorResult(res doctorResult) {
	marshaled, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(marshaled))
}

// kindOf extracts a Kind() classification from err's chain (unwrapping %w),
// falling back to a caller-supplied default when nothing in the chain
// implements it — e.g. a transport error before W7 types network/timeout
// kinds.
func kindOf(err error, fallback string) string {
	var k interface{ Kind() string }
	if errors.As(err, &k) {
		return k.Kind()
	}
	return fallback
}

func runConfigDoctor(asJSON, require bool) error {
	c, creds, err := getClient()
	if err != nil {
		var nc *config.NoCredentialsError
		if errors.As(err, &nc) {
			printDoctorResult(doctorResult{
				Kind:        "auth_missing",
				Message:     nc.Error(),
				Remediation: "run 'mbx config add <name> --url <url> --api-key <key>' or export METABASE_URL / METABASE_API_KEY",
				ConfigPath:  mustConfigPath(),
			})
			return nc
		}
		// Any other getClient() failure (unknown --project, malformed
		// --timeout, …) used to `return err` here with nothing printed to
		// stdout — the "always exactly one JSON object" contract must hold
		// on every reachable path, not just the credentials-missing one.
		printDoctorResult(doctorResult{
			Kind:        kindOf(err, "config_error"),
			Message:     err.Error(),
			Remediation: "check --project/--timeout/--url/--api-key and the config file",
			ConfigPath:  mustConfigPath(),
		})
		return err
	}

	if !asJSON {
		fmt.Fprintf(os.Stderr, "URL:        %s\n", creds.URL)
		fmt.Fprintf(os.Stderr, "API key:    %s (read)\n", redact(creds.APIKey))
		if creds.WriteAPIKey != "" {
			fmt.Fprintf(os.Stderr, "Write key:  %s\n", redact(creds.WriteAPIKey))
		} else {
			fmt.Fprintln(os.Stderr, "Write key:  (using read key for writes)")
		}
		fmt.Fprintf(os.Stderr, "Default DB: %d\n", creds.DefaultDB)
	}

	// W7: probe the unauthenticated /api/health endpoint FIRST. Without
	// this, a pure connectivity failure (sandbox block, DNS miss, timeout)
	// hit the /api/user/current check below and got misreported as
	// "auth_failed" — creds are fine, the host was simply unreachable.
	// Triaging network/timeout here first keeps that distinction honest.
	if _, healthErr := c.Get(ctxBg(), "/api/health", nil); healthErr != nil {
		remediation := "verify network connectivity to the Metabase host"
		var apiErr *client.APIError
		if errors.As(healthErr, &apiErr) {
			if hint := networkFailureHint(apiErr.URL); hint != "" {
				remediation = hint
			}
		}
		printDoctorResult(doctorResult{
			Kind:        kindOf(healthErr, "network"),
			Message:     healthErr.Error(),
			Remediation: remediation,
			ConfigPath:  mustConfigPath(),
			URL:         creds.URL,
		})
		return healthErr
	}

	// Verify auth via /api/user/current
	body, err := c.Get(ctxBg(), "/api/user/current", nil)
	if err != nil {
		printDoctorResult(doctorResult{
			Kind:        kindOf(err, "auth_failed"),
			Message:     err.Error(),
			Remediation: "verify the API key is valid (not revoked/rotated) and --url points at the right Metabase instance",
			ConfigPath:  mustConfigPath(),
			URL:         creds.URL,
		})
		return fmt.Errorf("auth check failed: %w", err)
	}

	out := doctorResult{OK: true, URL: creds.URL, DefaultDB: creds.DefaultDB, HasWriteKey: creds.WriteAPIKey != ""}
	var user map[string]any
	_ = json.Unmarshal(body, &user)
	if user != nil {
		out.User = map[string]any{
			"id":           user["id"],
			"email":        user["email"],
			"common_name":  user["common_name"],
			"is_superuser": user["is_superuser"],
			"last_login":   user["last_login"],
		}
	}

	// Probe write permission with a HEAD-equivalent: try to GET /api/permissions/group
	// which superusers have access to.
	permsBody, permsErr := c.Get(ctxBg(), "/api/permissions/group", nil)
	canRead := permsErr == nil
	out.CanListPermissionGroups = &canRead
	if canRead {
		groups := gjson.GetBytes(permsBody, "#.name").Array()
		names := make([]string, 0, len(groups))
		for _, g := range groups {
			names = append(names, g.String())
		}
		out.PermissionGroups = names
	}

	if !canRead && require {
		out.OK = false
		out.Kind = kindOf(permsErr, "not_authorized")
		out.Message = fmt.Sprintf("cannot list permission groups: %v", permsErr)
		// Carried finding A (Wave 2 review): a probe failure isn't always a
		// permission problem — a network blip surfaces here too. W7 types
		// transport failures as *APIError (StatusCode 0), so kindOf above
		// already resolves those to "network"/"timeout" instead of the
		// "not_authorized" default; match the remediation to what actually
		// happened rather than always blaming key scope.
		if out.Kind == "network" || out.Kind == "timeout" {
			out.Remediation = "the permission-group probe failed to reach Metabase (not a permission problem) — check network connectivity"
			if hint := networkFailureHint(creds.URL); out.Kind == "network" && hint != "" {
				out.Remediation = hint
			}
		} else {
			out.Remediation = "the key likely lacks group-list (superuser/admin) permission — fine for read-only usage; use a superuser-scoped key if group-list access is required"
		}
		out.ConfigPath = mustConfigPath()
		printDoctorResult(out)
		// Post-W7, permsErr for a transport-layer failure is ALREADY a
		// typed *APIError (StatusCode 0) — this errors.As always succeeds
		// now; the previous fabricated-403 fallback (misreporting a
		// connectivity blip as kind "not_authorized"/exit 2) is dead code,
		// kept only as a defensive last resort if some future permsErr
		// somehow isn't wrapped at all.
		var apiErr *client.APIError
		if errors.As(permsErr, &apiErr) {
			return apiErr
		}
		return &client.APIError{StatusCode: 403} // defensive fallback → exit 2
	}

	printDoctorResult(out)
	return nil
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print resolved credentials (key redacted)",
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, err := config.LoadCredentials(urlFlag, apiKeyFlag, projectFlag)
		if err != nil {
			return err
		}
		out := map[string]any{
			"url":           creds.URL,
			"api_key":       redact(creds.APIKey),
			"write_api_key": redact(creds.WriteAPIKey),
			"default_db":    creds.DefaultDB,
			"has_write_key": creds.WriteAPIKey != "",
			"config_path":   mustConfigPath(),
		}
		marshaled, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(marshaled))
		return nil
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the resolved config file path",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(mustConfigPath())
		return nil
	},
}

var (
	configAddURLFlag         string
	configAddAPIKeyFlag      string
	configAddWriteAPIKeyFlag string
	configAddDefaultDBFlag   int
)

var configAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a named project to ~/.config/mbx/config.toml",
	Long: `Writes a [projects.<name>] block to the config file (atomic save, mode
0600). The first project added becomes default_project automatically.

NoCredentialsError's remediation has pointed at this command since v0.1;
this is what makes that message true.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if configAddURLFlag == "" || configAddAPIKeyFlag == "" {
			return fmt.Errorf("--url and --api-key are required")
		}
		if err := validateConfigURLScheme(configAddURLFlag); err != nil {
			return err
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg == nil {
			cfg = &config.Config{}
		}
		isFirst, replaced := addProjectToConfig(cfg, name, config.Project{
			URL:         strings.TrimRight(configAddURLFlag, "/"),
			APIKey:      configAddAPIKeyFlag,
			WriteAPIKey: configAddWriteAPIKeyFlag,
			DefaultDB:   configAddDefaultDBFlag,
		})
		if err := config.Save(cfg); err != nil {
			return err
		}

		path, _ := config.Path()
		switch {
		case replaced:
			fmt.Fprintf(os.Stderr, "wrote %s — replaced existing project %q\n", path, name)
		case isFirst:
			fmt.Fprintf(os.Stderr, "wrote %s — project %q added and set as default\n", path, name)
		default:
			fmt.Fprintf(os.Stderr, "wrote %s — project %q added\n", path, name)
		}
		return nil
	},
}

// addProjectToConfig inserts/replaces a named project entry, setting it as
// the default project when the config had none yet. Split out from RunE so
// it's testable without cobra flag plumbing. Returns isFirst (this was the
// first project ever added) and replaced (name already existed — the
// existing entry was silently overwritten before; the caller now reports
// this honestly instead of always saying "added"). The two are mutually
// exclusive: a first-ever project can't already exist.
func addProjectToConfig(cfg *config.Config, name string, proj config.Project) (isFirst, replaced bool) {
	if cfg.Projects == nil {
		cfg.Projects = map[string]config.Project{}
	}
	isFirst = len(cfg.Projects) == 0
	_, replaced = cfg.Projects[name]
	cfg.Projects[name] = proj
	if isFirst {
		cfg.DefaultProject = name
	}
	return isFirst, replaced
}

// validateConfigURLScheme is SEC-001, originally landed early (Wave 2) as a
// standalone helper for `config add`. W14 moved the canonical implementation
// to the config package (config.ValidateURLScheme) so LoadCredentials can
// share it without an import cycle (config can't import commands); this is
// now a thin wrapper kept so this file's existing call site and test don't
// need to change.
func validateConfigURLScheme(raw string) error {
	return config.ValidateURLScheme(raw)
}

func init() {
	configDoctorCmd.Flags().BoolVar(&configDoctorRequireFlag, "require", false, "Fail (typed error, non-zero exit) on any non-OK doctor result, including a probe that --json alone only reports")
	configAddCmd.Flags().StringVar(&configAddURLFlag, "url", "", "Metabase base URL (required; https://, or http:// for localhost)")
	configAddCmd.Flags().StringVar(&configAddAPIKeyFlag, "api-key", "", "Metabase API key (required)")
	configAddCmd.Flags().StringVar(&configAddWriteAPIKeyFlag, "write-api-key", "", "Optional separate write key")
	configAddCmd.Flags().IntVar(&configAddDefaultDBFlag, "default-db", 0, "Default database ID for this project (0 = fleet default, DB 2)")
	configCmd.AddCommand(configDoctorCmd, configShowCmd, configPathCmd, configAddCmd)
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func mustConfigPath() string {
	p, err := config.Path()
	if err != nil {
		return ""
	}
	return p
}

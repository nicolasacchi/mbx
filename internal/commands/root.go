package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nicolasacchi/clicore/confirm"
	"github.com/nicolasacchi/mbx/internal/aliases"
	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/config"
	"github.com/nicolasacchi/mbx/internal/output"
)

var (
	urlFlag      string
	apiKeyFlag   string
	projectFlag  string
	jsonFlag     bool
	jqFlag       string
	verboseFlag  bool
	timeoutFlag  string
	yesFlag      bool
	dryRunFlag   bool
	databaseFlag string
	compactFlag  bool
	timingFlag   bool
)

// agentMode reports whether the CLI is being invoked under Claude Code or
// any caller that sets CLAUDECODE=1 / MBX_AGENT_MODE=1. Drives compact
// JSON output and the default --rows 100 cap.
func agentMode() bool {
	return os.Getenv("CLAUDECODE") == "1" || os.Getenv("MBX_AGENT_MODE") == "1"
}

var rootCmd = &cobra.Command{
	Use:           "mbx",
	Short:         "mbx — Metabase Explorer CLI",
	Long:          "Agentic-first CLI for Metabase. JSON output, gjson --jq filters, opinionated --database 2 default for ordini queries, dynamic card aliases.",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Skip prewarm for commands that don't need network at all.
		switch cmd.Name() {
		case "config", "show", "path", "completion", "help", "list":
			// Note: "list" inside config is intentional — the only "list"
			// subcommand that DOES need network is e.g. databases list, but
			// those have a different parent. Cobra's Name() returns the
			// leaf — we want to skip when leaf is one of the offline ones.
		}
		// Use the full command path for finer control.
		path := cmd.CommandPath()
		for _, skip := range []string{"mbx config show", "mbx config path", "mbx config add", "mbx help", "mbx completion", "mbx aliases list"} {
			if path == skip {
				return
			}
		}
		// Async best-effort prewarm. Don't block the actual command.
		go func() {
			c, _, err := getClient()
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c.Prewarm(ctx)
		}()
	},
}

func SetVersion(v string) { rootCmd.Version = v }

func Execute() error { return rootCmd.Execute() }

func init() {
	rootCmd.PersistentFlags().StringVar(&urlFlag, "url", "", "Metabase URL (overrides METABASE_URL env / config)")
	rootCmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "Metabase API key (overrides METABASE_API_KEY env / config)")
	rootCmd.PersistentFlags().StringVar(&projectFlag, "project", "", "Named project block from ~/.config/mbx/config.toml")
	rootCmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "Force JSON output even on TTY")
	rootCmd.PersistentFlags().StringVar(&jqFlag, "jq", "", "gjson filter expression (NOT real jq)")
	rootCmd.PersistentFlags().BoolVarP(&verboseFlag, "verbose", "v", false, "Log HTTP requests to stderr")
	rootCmd.PersistentFlags().StringVar(&timeoutFlag, "timeout", "60s", "HTTP timeout (e.g. 60s, 2m)")
	rootCmd.PersistentFlags().BoolVar(&yesFlag, "yes", false, "Confirm destructive/write commands")
	rootCmd.PersistentFlags().BoolVar(&yesFlag, "confirm", false, "Alias for --yes (confirm destructive/write commands)")
	rootCmd.PersistentFlags().BoolVar(&dryRunFlag, "dry-run", false, "Preview a write command's request without sending it")
	rootCmd.PersistentFlags().StringVar(&databaseFlag, "database", "", "Metabase database: prod|clickhouse|ops|internal|<numeric id> (default: project default, DB 2). Named aliases resolve live against GET /api/database.")
	rootCmd.PersistentFlags().BoolVar(&compactFlag, "compact", false, "Force compact JSON output (auto when CLAUDECODE=1)")
	rootCmd.PersistentFlags().BoolVar(&timingFlag, "timing", false, "Log HTTP wall + server timing to stderr")

	rootCmd.AddCommand(
		configCmd,
		queryCmd,
		schemaCmd,
		databasesCmd,
		tablesCmd,
		cardsCmd,
		dashboardsCmd,
		collectionsCmd,
		searchCmd,
		usersCmd,
		aliasesCmd,
		playgroundCmd,
		overviewCmd,
		activityCmd,
		ordiniKpiCmd,
	)
}

// getClient resolves credentials and constructs the HTTP client.
func getClient() (*client.Client, *config.Credentials, error) {
	creds, err := config.LoadCredentials(urlFlag, apiKeyFlag, projectFlag)
	if err != nil {
		return nil, nil, err
	}
	if verboseFlag {
		client.SetVerboseDest(os.Stderr)
	}
	if timingFlag {
		client.SetTimingDest(os.Stderr)
	}
	output.SetCompact(compactFlag || agentMode())
	timeout, err := time.ParseDuration(timeoutFlag)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid --timeout %q: %w", timeoutFlag, err)
	}
	c := client.New(creds.URL, creds.APIKey, creds.WriteAPIKey, verboseFlag, timeout)
	if timingFlag {
		c.SetTiming(true)
	}
	return c, creds, nil
}

// resolveDB resolves --database (numeric id, named alias, or unset →
// creds.DefaultDB) and echoes the result to stderr. Sole resolution point
// for query/schema/tables/playground/ordini-kpi (W3) — running a query
// against the wrong DB is otherwise indistinguishable from "no data".
//
// Numeric ids — including the unset→default case and the legacy "0" spelling
// of "use the default" — resolve without a network round-trip and echo bare
// ("database: 2"). Named aliases (prod/clickhouse/ops/internal) fetch
// GET /api/database live (cached for the process, see dbalias.go) to match
// on engine+name rather than a hardcoded id, so a retired/renamed DB fails
// closed with "alias not found"
// instead of silently resolving to a stale id.
func resolveDB(c *client.Client, creds *config.Credentials) (int, error) {
	id, name, err := resolveDBValue(c, creds, databaseFlag)
	if err != nil {
		return 0, err
	}
	echoResolvedDB(id, name)
	return id, nil
}

// resolveDBValue is resolveDB's core, split out so callers with special
// handling for the raw flag value (tables.go's "--database 0 shows every
// DB" behavior) and tests can drive it directly without touching the
// package-level databaseFlag var. name is "" when resolution didn't need
// (or couldn't cheaply get) a live listing — callers echo bare in that case.
func resolveDBValue(c *client.Client, creds *config.Credentials, raw string) (id int, name string, err error) {
	if raw == "" || raw == "0" {
		return creds.DefaultDB, "", nil
	}
	if n, ok := aliases.ParseInt(raw); ok {
		return n, "", nil
	}
	entry, err := resolveDBAliasLive(c, raw)
	if err != nil {
		return 0, "", err
	}
	return entry.ID, entry.Name, nil
}

// echoResolvedDB prints "database: <id> (<name>)" to stderr — bare id when
// the name isn't cheaply known. Shared by resolveDB (flag resolution) and
// `cards execute` (echoing the card's authoritative database_id).
func echoResolvedDB(id int, name string) {
	if name != "" {
		fmt.Fprintf(os.Stderr, "database: %d (%s)\n", id, name)
		return
	}
	fmt.Fprintf(os.Stderr, "database: %d\n", id)
}

// wantsWrite reports whether the operator has confirmed a write: --yes (or
// its --confirm alias — same underlying var), or the MBX_CONFIRM=1/true env
// fallback.
func wantsWrite() bool {
	if yesFlag {
		return true
	}
	v := os.Getenv("MBX_CONFIRM")
	return v == "1" || v == "true"
}

// gate wraps clicore's confirm.Gate with mbx's --yes/--dry-run/MBX_CONFIRM
// wiring for write commands that support --dry-run. handled=true means the
// caller must return immediately: --dry-run printed action to cmd's output
// (err nil, exit 0), or the gate refused (err is a write_locked APIError,
// exit 6). Callers build `action` themselves with the concrete method/path/
// body they're about to send — that's what gets echoed on --dry-run — rather
// than relying on Gate's generic one-line wording alone.
func gate(cmd *cobra.Command, action string) (bool, error) {
	return confirm.Gate(cmd.OutOrStdout(), wantsWrite(), dryRunFlag, action)
}

func ctxBg() context.Context { return context.Background() }

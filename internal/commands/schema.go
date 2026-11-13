package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/nicolasacchi/mbx/internal/output"
)

var schemaGotchasFlag bool

var schemaCmd = &cobra.Command{
	Use:   "schema [table]",
	Short: "Print column list for a table (INFORMATION_SCHEMA shortcut), or the curated gotcha table (--gotchas)",
	Long: `Prints the column list for a MySQL/Postgres table by querying INFORMATION_SCHEMA.COLUMNS.

When --database 2 (default) and table is "ordini", an extra header note flags the
documented gotcha: 'stato' and 'totale_ordine' are NOT columns on this table.

For databases without INFORMATION_SCHEMA, use 'mbx tables list --database <id>'
and 'mbx tables get <id>'. (The ClickHouse DBs 35/166 are retired and unreachable.)

--gotchas prints the curated (db, table, column) -> hint table instead of
running any query — no <table> positional arg required, or pass one to
filter to that table's entries. Same source of truth the SQL lint warnings
and the A3 error-envelope hints read from (W9) — see 'mbx query --no-lint'
for the lint side.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if schemaGotchasFlag {
			return runSchemaGotchas(cmd, args)
		}
		if len(args) != 1 {
			return fmt.Errorf("table argument required (or pass --gotchas to print the curated gotcha table)")
		}
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		db, err := resolveDB(c, creds)
		if err != nil {
			return err
		}
		table := args[0]

		if db == 35 || db == 133 {
			return fmt.Errorf("ClickHouse schema lookup not supported via INFORMATION_SCHEMA — use 'mbx tables list --database %d' instead", db)
		}

		if db == 2 && table == "ordini" {
			fmt.Fprintln(os.Stderr, "note: ordini does not have 'stato' or 'totale_ordine' columns on DB 2 (verified 2026-05-08) — use 'created_at' and COUNT(*). Run 'mbx schema --gotchas ordini' for the full curated list.")
		}

		sanitized, err := sanitizeTableName(table)
		if err != nil {
			return err
		}
		body := map[string]any{
			"database": db,
			"type":     "native",
			"native": map[string]any{
				"query": fmt.Sprintf(
					`SELECT COLUMN_NAME, DATA_TYPE, IS_NULLABLE, COLUMN_KEY, COLUMN_DEFAULT
					 FROM INFORMATION_SCHEMA.COLUMNS
					 WHERE TABLE_NAME = '%s'
					 ORDER BY ORDINAL_POSITION`,
					sanitized,
				),
			},
		}
		resp, err := c.PostQuery(ctxBg(), "/api/dataset", body)
		if err != nil {
			return err
		}
		simplified := simplifyQueryResponse(resp, 0)
		return output.PrintData("schema.show", simplified, jsonFlag, jqFlag)
	},
}

// runSchemaGotchas implements `mbx schema --gotchas [table]`: no network
// call, just a filtered/unfiltered render of the in-binary gotchaTable
// (gotchas.go) scoped to DB 2 (the only database with curated entries
// today — this command doesn't take --database, matching the fact that
// every seeded entry currently lives on the MySQL production replica).
func runSchemaGotchas(cmd *cobra.Command, args []string) error {
	entries := gotchaTable
	if len(args) == 1 {
		entries = gotchasForTable(2, args[0])
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"db":     e.DB,
			"table":  e.Table,
			"column": e.Column,
			"hint":   e.Hint,
		})
	}
	marshaled, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return output.PrintData("schema.gotchas", marshaled, jsonFlag, jqFlag)
}

// tableNameRe is the allowlist for a bare SQL identifier: letters, digits,
// underscore, not starting with a digit — matches every real MySQL/Postgres
// table name in this schema and rejects everything else outright (W14).
// Replaces the pre-W14 sanitizeTableName, which STRIPPED disallowed
// characters (quotes/backslash/semicolon) instead of rejecting the input —
// stripping can silently change meaning (e.g. "ordini--comment" strips to
// "ordini--comment" unchanged, since '-' was never stripped, but a smuggled
// quote/backslash sequence could still be reshaped into something that
// parses differently than what the caller typed). Reject-not-strip means a
// malformed table name surfaces as a clear "invalid table name" error
// instead of a silently-mutated query.
var tableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sanitizeTableName validates a table name against the identifier allowlist
// before it's spliced into the INFORMATION_SCHEMA query, returning it
// unchanged on success or an error on any disallowed character (W14 —
// allowlist regex, reject not strip; see tableNameRe's doc comment).
func sanitizeTableName(s string) (string, error) {
	if !tableNameRe.MatchString(s) {
		return "", fmt.Errorf("invalid table name %q: must match %s", s, tableNameRe.String())
	}
	return s, nil
}

func init() {
	schemaCmd.Flags().BoolVar(&schemaGotchasFlag, "gotchas", false, "Print the curated (db, table, column) gotcha table instead of running a query; optionally filter to [table]")
}

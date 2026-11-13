package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/output"
)

var (
	queryCSVFlag    bool
	queryRawFlag    bool
	queryNoLintFlag bool
	queryRowsFlag   int
)

const agentDefaultRows = 100

// metabaseServerRowCap is Metabase's own display-time truncation for
// unconstrained POST /api/dataset queries (qp/userland-query-with-default-constraints).
// It fires independently of mbx's --rows cap — see "row_cap_source" in the
// simplified output and simplifyQueryResponse below.
const metabaseServerRowCap = 2000

var queryCmd = &cobra.Command{
	Use:   "query <SQL>",
	Short: "Run a native SQL query (default --database 2)",
	Long: `Execute a native SQL query against a Metabase database.

Default --database 2 (Production 1000farmacie Replica). Prefer the named alias
--database prod, because numeric ids can change. The ClickHouse DBs (35, 166)
are retired and unreachable. Run "mbx databases list" to see all options.

Output by default is a simplified shape {columns, rows, row_count, running_ms,
database_id}. Use --raw for the full Metabase response. Use --jq to filter,
e.g. --jq 'rows.0.0' for the first column of the first row.

When invoked under Claude Code (CLAUDECODE=1), defaults --rows 100 to keep
the agent context bounded. Pass --rows 0 to disable the cap, or --rows N
to override. When the result is capped, the JSON output gets a
"truncated": true marker with "row_cap_source": "client" (pipe-safe;
survives | jq).

Metabase ALSO caps unconstrained queries at 2,000 rows server-side. When
no client --rows cap applied and the result comes back at exactly 2,000
rows, mbx emits the same truncated marker with "row_cap_source": "server"
— this is a heuristic (a genuine 2,000-row result is indistinguishable),
so treat it as "may be truncated". Escape hatches: --csv (uncapped export
via the real Metabase export endpoint), LIMIT/OFFSET pagination, or
aggregation.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		db, err := resolveDB(c, creds)
		if err != nil {
			return err
		}
		sql := args[0]

		if !queryNoLintFlag {
			lintSQLForGotchas(db, sql)
		}
		if db == 1 {
			fmt.Fprintln(os.Stderr, "warning: --database 1 does not exist on this Metabase instance (10 DBs live; run 'mbx databases list')")
		}

		// --csv routes through Metabase's real export endpoint (bypasses both
		// mbx's --rows cap and the server's 2,000-row display cap), so it's
		// handled entirely separately from the constrained /api/dataset path
		// below (W11).
		if queryCSVFlag {
			if cmd.Flags().Changed("rows") && queryRowsFlag > 0 {
				fmt.Fprintln(os.Stderr, "note: --csv exports are uncapped (bypasses mbx's --rows and Metabase's 2,000-row display cap); --rows is ignored")
			}
			csvBody := map[string]any{
				"query": map[string]any{
					"database": db,
					"type":     "native",
					"native": map[string]any{
						"query": sql,
					},
				},
				"format_rows": false,
			}
			rc, err := c.PostStream(ctxBg(), "/api/dataset/csv", csvBody)
			if err != nil {
				// Carried finding B: the CSV path used to bare-return here
				// while the JSON path below emits the A3 envelope — agent
				// loops parsing stderr for {"error":{kind,...}} got nothing
				// on a --csv failure. Route through the same envelope.
				emitErrorEnvelope(err, db, sql)
				return err
			}
			return streamOrEnvelope(rc, db, sql)
		}

		// T2': resolve the row cap. Agent mode → 100, human mode → no cap,
		// explicit --rows N always wins, --rows 0 always disables.
		rowCap := queryRowsFlag
		if !cmd.Flags().Changed("rows") && agentMode() {
			rowCap = agentDefaultRows
		}

		body := map[string]any{
			"database": db,
			"type":     "native",
			"native": map[string]any{
				"query": sql,
			},
		}
		if rowCap > 0 {
			body["constraints"] = map[string]any{
				"max-results":           rowCap,
				"max-results-bare-rows": rowCap,
			}
		}

		resp, err := c.PostQuery(ctxBg(), "/api/dataset", body)
		if err != nil {
			emitErrorEnvelope(err, db, sql)
			return err
		}

		// Surface query-level errors (Metabase returns 200 with error in body).
		if errStr := gjson.GetBytes(resp, "error").String(); errStr != "" {
			emitErrorEnvelopeFromBody(errStr, gjson.GetBytes(resp, "error_type").String(), db, sql)
			return fmt.Errorf("query error: %s", errStr)
		}

		if queryRawFlag {
			return output.PrintData("query.raw", resp, jsonFlag, jqFlag)
		}

		// Default: extract and pretty-print just the rows + cols.
		simplified := simplifyQueryResponse(resp, rowCap)
		return output.PrintData("query.simplified", simplified, jsonFlag, jqFlag)
	},
}

func init() {
	queryCmd.Flags().BoolVar(&queryCSVFlag, "csv", false, "Output as CSV (RFC 4180; NULL → empty)")
	queryCmd.Flags().BoolVar(&queryRawFlag, "raw", false, "Print the raw Metabase response (default: simplified rows+cols)")
	queryCmd.Flags().BoolVar(&queryNoLintFlag, "no-lint", false, "Suppress schema-gotcha warnings (run 'mbx schema --gotchas' for the curated list)")
	queryCmd.Flags().IntVar(&queryRowsFlag, "rows", 0, "Agent/client row cap (0 = unlimited; agent mode auto-defaults to 100). Separate from Metabase's own 2,000-row server display cap, which is detected heuristically and bypassed by --csv")
}

// simplifyQueryResponse reshapes the Metabase /api/dataset response into a
// flatter form. Two independent truncation signals can fire, distinguished
// by "row_cap_source" so agents can tell them apart:
//   - "client": mbx's own --rows cap was hit (rowCap > 0 && rowCount >= rowCap).
//   - "server": no client cap was requested, but the result came back at
//     exactly Metabase's server-side display cap (metabaseServerRowCap).
//     This is a heuristic — a genuine 2,000-row result is indistinguishable
//     from a truncated one — so it's intentionally pessimistic ("may be").
//
// Both markers survive `| jq` (the stderr warning may not).
func simplifyQueryResponse(resp []byte, rowCap int) []byte {
	cols := gjson.GetBytes(resp, "data.cols.#.name").Array()
	rowCount := gjson.GetBytes(resp, "row_count").Int()
	runMS := gjson.GetBytes(resp, "running_time").Int()

	out := map[string]any{
		"columns":     gjsonResultStrings(cols),
		"rows":        json.RawMessage(gjson.GetBytes(resp, "data.rows").Raw),
		"row_count":   rowCount,
		"running_ms":  runMS,
		"database_id": gjson.GetBytes(resp, "database_id").Int(),
	}
	if !gjson.GetBytes(resp, "data.rows").Exists() {
		out["rows"] = json.RawMessage("[]")
	}
	switch {
	case rowCap > 0 && rowCount >= int64(rowCap):
		out["truncated"] = true
		out["row_cap"] = rowCap
		out["row_cap_source"] = "client"
		fmt.Fprintf(os.Stderr, "warning: result capped at --rows %d; pass --rows 0 for unlimited\n", rowCap)
	case rowCap <= 0 && rowCount == metabaseServerRowCap:
		out["truncated"] = true
		out["row_cap"] = metabaseServerRowCap
		out["row_cap_source"] = "server"
		fmt.Fprintf(os.Stderr, "warning: result may be server-truncated at Metabase's %d-row display cap (a genuine %d-row result looks identical, so this is a heuristic) — escape hatches: --csv (uncapped export), LIMIT/OFFSET pagination, or aggregation\n", metabaseServerRowCap, metabaseServerRowCap)
	}

	marshaled, err := json.Marshal(out)
	if err != nil {
		return resp
	}
	return marshaled
}

func gjsonResultStrings(rs []gjson.Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.String()
	}
	return out
}

// A3 — structured error envelope. Emits one JSON object per error to stderr
// before the human-readable error line. Agent loops can reliably grep the
// first line that starts with `{"error":` and dispatch on `kind`.
type errorEnvelope struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Table   string `json:"table,omitempty"`
	Column  string `json:"column,omitempty"`
	Hint    string `json:"hint,omitempty"`
	DB      int    `json:"db,omitempty"`
}

var (
	reUnknownColumn = regexp.MustCompile(`Unknown column ['"]([^'"]+)['"]`)
	reUnknownTable  = regexp.MustCompile(`Table ['"]?([^'" ]+)['"]? doesn't exist`)
	reFromTable     = regexp.MustCompile(`(?i)\bfrom\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
)

func emitErrorEnvelope(err error, db int, sql string) {
	env := errorEnvelope{Kind: "client_error", Message: err.Error(), DB: db}
	// errors.As (not a bare type assertion) so a wrapped error — e.g. the
	// client's "max retries exceeded: %w" path — still yields its inner
	// *client.APIError's Kind() instead of silently falling through to the
	// "client_error" default (this is what let network/timeout failures
	// (W7) go unclassified here even after client.go started typing them).
	var k interface{ Kind() string }
	if errors.As(err, &k) {
		env.Kind = k.Kind()
	}
	if env.Kind == "network" {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			if hint := networkFailureHint(apiErr.URL); hint != "" {
				env.Hint = hint
			}
		}
	}
	emitEnvelope(env)
}

func emitErrorEnvelopeFromBody(errMsg, errType string, db int, sql string) {
	env := errorEnvelope{Message: errMsg, DB: db}
	switch {
	case strings.Contains(errMsg, "Unknown column"):
		env.Kind = "unknown_column"
		if m := reUnknownColumn.FindStringSubmatch(errMsg); len(m) > 1 {
			env.Column = m[1]
		}
		env.Table = guessTableFromSQL(sql)
		env.Hint = gotchaHint(db, env.Table, env.Column)
	case strings.Contains(errMsg, "doesn't exist"):
		env.Kind = "unknown_table"
		if m := reUnknownTable.FindStringSubmatch(errMsg); len(m) > 1 {
			env.Table = m[1]
		}
	case errType == "invalid-query":
		env.Kind = "invalid_query"
	default:
		env.Kind = "query_error"
	}
	emitEnvelope(env)
}

func emitEnvelope(env errorEnvelope) {
	wrapped := map[string]errorEnvelope{"error": env}
	b, err := json.Marshal(wrapped)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, string(b))
}

// streamOrEnvelope drains an already-successful (2xx) PostStream response
// and either pipes it to stdout unchanged, or — when it turns out to be an
// error dressed up as a 200 — routes it through the A3 envelope with a
// non-nil return. Shared by query --csv and cards export: Metabase's
// export endpoints (/api/dataset/csv, /api/card/:id/query/:format)
// respond 200 OK with a JSON error body instead of real export data when
// the underlying query fails (e.g. a bad column) — PostStream only
// classifies non-2xx as an error, so without this check the JSON error
// payload would silently stream to stdout as if it were export data, exit
// 0. Always closes rc.
//
// A genuine export body CAN legitimately start with '{' — a CSV column
// literally named "{x}", or a json-format export shape that happens to
// open with an object — so a '{'-leading body is inspected for gjson's
// "error" key specifically rather than assumed to be an error outright;
// anything '{'-leading without that key streams through byte-identical via
// the same path a non-'{' body takes. Buffers at most one response body in
// memory (only on the '{'-leading path, only until the error check runs);
// the common non-error case never buffers.
func streamOrEnvelope(rc io.ReadCloser, db int, sql string) error {
	defer rc.Close()
	br := bufio.NewReader(rc)
	peek, _ := br.Peek(1)
	if len(peek) == 0 || peek[0] != '{' {
		_, err := io.Copy(os.Stdout, br)
		return err
	}
	body, _ := io.ReadAll(br)
	if errStr := gjson.GetBytes(body, "error").String(); errStr != "" {
		emitErrorEnvelopeFromBody(errStr, gjson.GetBytes(body, "error_type").String(), db, sql)
		return fmt.Errorf("query error: %s", errStr)
	}
	_, err := os.Stdout.Write(body)
	return err
}

func guessTableFromSQL(sql string) string {
	if m := reFromTable.FindStringSubmatch(sql); len(m) > 1 {
		return strings.ToLower(m[1])
	}
	return ""
}

// quiet helpers
var _ = url.Values{}
var _ = http.MethodGet

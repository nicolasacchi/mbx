package commands

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var ordiniKpiWindowFlag string

// ordiniKpiCmd — E6. Encodes the canonical "business metrics" SQL that
// agent workflows used to hand-code against ordini. By keeping the
// schema-correct SQL inside the binary, we eliminate the
// `stato`/`totale_ordine` schema-gotcha attack surface from prompt templates
// and cut ~400 prompt tokens per workflow.
var ordiniKpiCmd = &cobra.Command{
	Use:   "ordini-kpi",
	Short: "Canonical ordini KPIs (count, unique buyers, cancelled, GMV) for a time window",
	Long: `Returns the canonical ordini KPIs over a time window:

  - order_count: COUNT(*) over orders created in the window
  - unique_buyers: COUNT(DISTINCT user_id) over the same
  - cancelled: COUNT(*) WHERE state IN ('annullato','rifiutato')
                 (the real status column is 'state', NOT 'stato')
  - latest: MAX(created_at) of returned orders
  - as_of: server NOW() at query time
  - gmv: SUM(op.prezzo * op.quantita) over PLACED orders in the window
                 (windowed on customer_confirm_at, NOT created_at — a
                 created_at window over-counts ~2x by including unconfirmed
                 carts. Product-line GMV, EUR, excludes shipping/fees.
                 Replaces the dead google_analytics_measures JSON_EXTRACT
                 recipe — that table was dropped by PR #7148.)
  - window: the input window string

Default --window 24h. Other examples: 1h, 6h, 3d, 7d, 30d.

Runs against DB 2 (Production 1000farmacie Replica) by default — that's
where the ordini table lives — overridable via --database (e.g. "staging")
for a non-prod replica with the same schema. The SQL is hard-coded and
schema-correct so slash commands can stop hand-coding it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		db, err := resolveDB(c, creds)
		if err != nil {
			return err
		}
		interval, err := windowToMySQLInterval(ordiniKpiWindowFlag)
		if err != nil {
			return err
		}

		sql := buildOrdiniKPISQL(interval)

		body := map[string]any{
			"database": db,
			"type":     "native",
			"native":   map[string]any{"query": sql},
		}
		resp, err := c.PostQuery(ctxBg(), "/api/dataset", body)
		if err != nil {
			emitErrorEnvelope(err, db, sql)
			return err
		}
		if errStr := gjson.GetBytes(resp, "error").String(); errStr != "" {
			emitErrorEnvelopeFromBody(errStr, gjson.GetBytes(resp, "error_type").String(), db, sql)
			return fmt.Errorf("query error: %s", errStr)
		}

		out, err := decodeOrdiniKPIRow(resp, ordiniKpiWindowFlag)
		if err != nil {
			return err
		}
		marshaled, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return output.PrintData("ordini-kpi", marshaled, jsonFlag, jqFlag)
	},
}

// buildOrdiniKPISQL builds the single-round-trip KPI query. The gmv subquery
// deliberately windows on customer_confirm_at (placed orders) while the
// outer query stays on created_at (all orders, matching order_count /
// unique_buyers / cancelled semantics) — see the Long help text above for
// why a shared created_at window would over-count GMV ~2x.
func buildOrdiniKPISQL(interval string) string {
	return fmt.Sprintf(`
		SELECT
		  COUNT(*)                                                       AS order_count,
		  COUNT(DISTINCT user_id)                                        AS unique_buyers,
		  SUM(CASE WHEN state IN ('annullato','rifiutato') THEN 1 ELSE 0 END) AS cancelled,
		  MAX(created_at)                                                AS latest,
		  NOW()                                                          AS as_of,
		  (SELECT COALESCE(SUM(op.prezzo * op.quantita), 0)
		     FROM ordini o2
		     JOIN ordine_prodotti op ON op.ordine_id = o2.id
		    WHERE o2.customer_confirm_at >= NOW() - INTERVAL %s)         AS gmv
		FROM ordini
		WHERE created_at >= NOW() - INTERVAL %s`, interval, interval)
}

// decodeOrdiniKPIRow unpacks the 6-cell result row into the ordini-kpi
// output shape. Split out from RunE so the decode logic is directly
// testable against a fixture without a live round trip.
//
// ARITY LOCKSTEP TRAP: cells[0..5] below are positional, not named — they
// MUST stay in exactly the same order as buildOrdiniKPISQL's SELECT list
// above (order_count, unique_buyers, cancelled, latest, as_of, gmv).
// Reordering, inserting, or removing a column in one place without the
// other silently shifts every cell after it into the wrong output field —
// gjson has no column-name-based access here (a plain SELECT gives Metabase
// positional cols, not named ones in data.rows), so nothing catches this at
// compile time or even at parse time; it would just produce plausible-
// looking but wrong numbers. If you touch either the SELECT list or this
// function, touch both, and update TestDecodeOrdiniKPIRow.
func decodeOrdiniKPIRow(resp []byte, window string) (map[string]any, error) {
	row := gjson.GetBytes(resp, "data.rows.0")
	if !row.IsArray() || len(row.Array()) < 6 {
		return nil, fmt.Errorf("ordini-kpi: unexpected response shape")
	}
	cells := row.Array()
	return map[string]any{
		"order_count":   cells[0].Int(),    // buildOrdiniKPISQL SELECT position 0
		"unique_buyers": cells[1].Int(),    // position 1
		"cancelled":     cells[2].Int(),    // position 2
		"latest":        cells[3].String(), // position 3
		"as_of":         cells[4].String(), // position 4
		"gmv":           cells[5].Float(),  // position 5 — see buildOrdiniKPISQL's gmv subquery
		"window":        window,
	}, nil
}

func init() {
	ordiniKpiCmd.Flags().StringVar(&ordiniKpiWindowFlag, "window", "24h", "Time window: 1h, 24h, 3d, 7d, 30d, etc.")
}

// windowToMySQLInterval parses "24h" / "3d" / "30m" / "2w" into MySQL
// INTERVAL expressions like "24 HOUR" / "3 DAY" / "30 MINUTE" / "2 WEEK".
// The result is spliced into a SQL template; the regex-based parser only
// accepts a digit prefix + single unit letter to keep injection surface zero.
var windowRegex = regexp.MustCompile(`^([0-9]+)([smhdwSMHDW])$`)

func windowToMySQLInterval(w string) (string, error) {
	m := windowRegex.FindStringSubmatch(w)
	if m == nil {
		return "", fmt.Errorf("invalid --window %q (expected e.g. 24h, 3d, 30m)", w)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return "", fmt.Errorf("invalid --window %q", w)
	}
	unit := ""
	switch m[2][0] {
	case 's', 'S':
		unit = "SECOND"
	case 'm', 'M':
		unit = "MINUTE"
	case 'h', 'H':
		unit = "HOUR"
	case 'd', 'D':
		unit = "DAY"
	case 'w', 'W':
		unit = "WEEK"
	}
	return fmt.Sprintf("%d %s", n, unit), nil
}

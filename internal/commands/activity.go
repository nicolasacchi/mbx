package commands

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nicolasacchi/mbx/internal/output"
)

const auditDB = 69 // Metabase Internal DB (Postgres) — verified 2026-05-08

var (
	activityLimitFlag int
	activitySinceFlag string
)

var activityCmd = &cobra.Command{
	Use:   "activity",
	Short: "Inspect Metabase query-execution history (via internal DB)",
}

var activityRecentCmd = &cobra.Command{
	Use:   "recent",
	Short: "Recent query executions (who/what/how-fast — last N)",
	Long: `Returns recent query-execution events from the Metabase Internal DB
(database_id=69, Postgres) — table query_execution. The /api/activity REST
endpoint was removed in newer Metabase versions; querying the internal DB
directly is more reliable and the same data.

Joined to core_user for executor name, report_card for card name, and
metabase_database for database name.

Useful for /daily-changelog: "what queries ran today, by whom".`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		where := []string{"qe.started_at IS NOT NULL"}
		if activitySinceFlag != "" {
			interval, err := sanitizeInterval(activitySinceFlag)
			if err != nil {
				return err
			}
			where = append(where, fmt.Sprintf("qe.started_at >= NOW() - INTERVAL '%s'", interval))
		}
		limit := activityLimitFlag
		if limit <= 0 {
			limit = 25
		}
		sql := fmt.Sprintf(`
			SELECT
			  qe.started_at,
			  qe.running_time,
			  qe.result_rows,
			  qe.context,
			  qe.error,
			  qe.cache_hit,
			  qe.card_id,
			  qe.dashboard_id,
			  qe.database_id,
			  COALESCE(c.name, '(ad-hoc)') AS card_name,
			  COALESCE(u.email, '(unknown)') AS executor_email,
			  COALESCE(d.name, '(unknown)') AS database_name
			FROM query_execution qe
			LEFT JOIN core_user u ON u.id = qe.executor_id
			LEFT JOIN report_card c ON c.id = qe.card_id
			LEFT JOIN metabase_database d ON d.id = qe.database_id
			WHERE %s
			ORDER BY qe.started_at DESC
			LIMIT %d`, strings.Join(where, " AND "), limit)

		body := map[string]any{
			"database": auditDB,
			"type":     "native",
			"native":   map[string]any{"query": sql},
		}
		resp, err := c.PostQuery(ctxBg(), "/api/dataset", body)
		if err != nil {
			return err
		}
		return output.PrintData("activity.recent", simplifyQueryResponse(resp, 0), jsonFlag, jqFlag)
	},
}

// intervalRe is the allowlist for a Postgres INTERVAL literal's body: digits,
// letters, and single spaces (e.g. "24 hours", "7 days") — matches every
// real --since value documented for this command and rejects everything
// else outright (W14).
var intervalRe = regexp.MustCompile(`^[0-9a-zA-Z ]+$`)

// sanitizeInterval validates a --since value against the INTERVAL-literal
// allowlist before it's spliced into the SQL, returning it unchanged on
// success or an error otherwise (W14 — allowlist regex, reject not strip).
// Replaces the pre-W14 version, which STRIPPED disallowed characters
// (quotes, semicolons, tabs, …) instead of rejecting the input — silently
// re-shaping "1; DROP" into "1 DROP" is a query that still runs, just not
// the one the caller typed; rejecting it surfaces the mistake instead.
func sanitizeInterval(s string) (string, error) {
	if !intervalRe.MatchString(s) {
		return "", fmt.Errorf("invalid --since %q: must match %s (digits, letters, spaces only)", s, intervalRe.String())
	}
	return s, nil
}

func init() {
	activityRecentCmd.Flags().IntVar(&activityLimitFlag, "limit", 25, "Cap result count")
	activityRecentCmd.Flags().StringVar(&activitySinceFlag, "since", "", "Postgres INTERVAL string (e.g. '24 hours', '7 days')")
	activityCmd.AddCommand(activityRecentCmd)
}

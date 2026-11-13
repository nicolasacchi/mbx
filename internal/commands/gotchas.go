package commands

// W9 — a curated, in-binary (db, table, column) -> hint table, shared by
// the SQL-lint stderr warning (lintSQLForGotchas, fired pre-flight on every
// `mbx query`), the A3 error-envelope hint (gotchaHint, fired post-flight on
// an "Unknown column" response), and `mbx schema --gotchas`. Before W9, the
// lint and the envelope hint were two independent copies of the same two
// ordini facts (stato/totale_ordine) — this is the one source of truth both
// read from now, so a wording fix or a newly-discovered trap updates every
// surface at once.
//
// Entries are curated, not generated: every one below was verified against a
// live schema/data query (`mbx schema <table> --database 2`,
// `mbx query --database 2 "SELECT COUNT(*) ..."`) on the date noted in its
// Hint. A table with no verified trap is deliberately left OUT of this list
// rather than padded with a speculative entry: curated means true.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// gotchaEntry is one curated (db, table, column) trap. Column may name a
// column that exists but is misleading (prodotti.nome exists but is ~87%
// NULL) or a plausible guess that doesn't exist at all (ordini has no
// totale_ordine column) — the mechanism doesn't care which; the Hint text
// carries the actual distinction.
type gotchaEntry struct {
	DB     int
	Table  string
	Column string
	Hint   string
}

// gotchaTable is the curated seed list. Order is preserved (not a map — Go
// map iteration is random) so `schema --gotchas` output and its golden test
// are deterministic.
var gotchaTable = []gotchaEntry{
	{DB: 2, Table: "ordini", Column: "stato",
		Hint: "the real status column is 'state' (varchar) — values include 'review', 'in_attesa_corriere', 'annullato', 'spedito', 'consegnato'"},
	{DB: 2, Table: "ordini", Column: "totale_ordine",
		Hint: "no revenue column on ordini — use 'mbx ordini-kpi' (gmv field) or SUM(op.prezzo * op.quantita) via ordine_prodotti joined on ordini.id, windowed on customer_confirm_at; the old google_analytics_measures JSON_EXTRACT path is dead (table dropped by PR #7148)"},
	{DB: 2, Table: "prodotti", Column: "nome",
		Hint: "'nome' exists but is ~87% NULL (verified 2026-07-20 live) — the actually-populated product-name column is 'name' (100% filled). Counter-intuitive for an Italian-schema table: the Italian-named column is the sparse one."},
	{DB: 2, Table: "prodotti", Column: "prezzo_min",
		Hint: "prezzo_min/prezzo_max/prezzo_web are ~97-99% NULL or zero on prodotti (verified 2026-07-20 live) — none is a reliable per-product current-price signal at this scale; cross-check against the live cart/checkout pricing flow before using any of them for revenue analysis."},
	{DB: 2, Table: "pharmacy_product_listings", Column: "price",
		Hint: "no 'price' (or 'quantity'/'stock') column on pharmacy_product_listings (verified 2026-07-20 live) — the table is a lean pharmacy x minsan fulfillment_type mapping only: id, pharmacy_code, minsan, fulfillment_type, created_at, updated_at. Price lives on prodotti — see that table's own gotcha here, its price columns are mostly unpopulated too."},
	{DB: 2, Table: "pharmacy_product_listings", Column: "quantity",
		Hint: "same as 'price' above — pharmacy_product_listings has no quantity/stock column, just id, pharmacy_code, minsan, fulfillment_type, created_at, updated_at (verified 2026-07-20 live)."},
	{DB: 2, Table: "coupons", Column: "status",
		Hint: "'status = 1' alone does NOT mean a coupon is live: 99.5% of status=1 rows have published=false (verified 2026-07-20 live) — published and visibility gate usability separately from status. Filter on all three, not status alone."},
}

// gotchasForTable returns every curated entry for (db, table), table-name
// matched case-insensitively. Used by `schema --gotchas <table>`.
func gotchasForTable(db int, table string) []gotchaEntry {
	var out []gotchaEntry
	for _, e := range gotchaTable {
		if e.DB == db && strings.EqualFold(e.Table, table) {
			out = append(out, e)
		}
	}
	return out
}

// lintSQLForGotchas prints a stderr warning for every curated gotcha whose
// table appears (case-insensitive substring) AND whose column appears as a
// whole word (case-insensitive \b<col>\b — NOT a substring) in sql, scoped
// to db. Generalizes the pre-W9 lintSQLForOrdiniGotchas, which only ever
// checked db==2, the literal substring "ordini", and two hardcoded columns —
// now driven entirely by gotchaTable, so a newly curated entry lints for
// free. Suppress with --no-lint or by querying a different database.
//
// Column matching is word-boundary, not substring: a plain strings.Contains
// check on "nome" fires on "cognome"/"denominazione", and the pre-existing
// "stato" check has the same latent bug on "acquistato"/"prestato" — a
// review caught this live with a genuine `u.cognome` join false-triggering
// the prodotti.nome warning. The table-name check stays substring-based
// (table names are long/specific enough in practice that this hasn't been a
// problem, and a table always appears as its own identifier — not embedded
// inside a longer one — in real SQL FROM/JOIN clauses).
func lintSQLForGotchas(db int, sql string) {
	lower := strings.ToLower(sql)
	for _, e := range gotchaTable {
		if e.DB != db {
			continue
		}
		if !strings.Contains(lower, strings.ToLower(e.Table)) {
			continue
		}
		if !columnWordRegex(e.Column).MatchString(sql) {
			continue
		}
		fmt.Fprintf(os.Stderr, "warning: %s.%s — %s\n", e.Table, e.Column, e.Hint)
	}
}

// columnWordRegex compiles a case-insensitive whole-word matcher for a
// column name. Compiled per call rather than cached — lintSQLForGotchas
// runs at most once per `mbx query` invocation (not a hot loop), and
// gotchaTable is a mutable package var in tests (TestOrdiniHintSharedSource
// swaps it wholesale), so precomputing regexes at package-init time would
// go stale the moment a test mutates the table.
func columnWordRegex(column string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(column) + `\b`)
}

// gotchaHint returns the curated hint for an exact (db, table, column)
// match, or "" if none is curated — the caller (the A3 unknown_column
// envelope) still has the column/table names in that case, which is enough
// to point an agent at `mbx schema <table>` or `mbx schema --gotchas`.
// Generalizes the pre-W9 ordiniHint, which only ever matched table=="ordini"
// regardless of db.
func gotchaHint(db int, table, column string) string {
	for _, e := range gotchaTable {
		if e.DB == db && strings.EqualFold(e.Table, table) && strings.EqualFold(e.Column, column) {
			return e.Hint
		}
	}
	return ""
}

package commands

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything written to it. Local to this file — config_test.go's
// captureConfigStdout captures stdout, not stderr, and other test files in
// this package redirect os.Stderr inline rather than via a shared helper.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestLintSQLForGotchas_FiresForEverySeededEntry walks the curated
// gotchaTable and asserts the lint fires a warning naming the table+column
// for every single seeded entry — a generalized version of the old
// ordini-only lint test, driven entirely by the shared table so a newly
// curated entry is covered automatically.
func TestLintSQLForGotchas_FiresForEverySeededEntry(t *testing.T) {
	for _, e := range gotchaTable {
		sql := "SELECT " + e.Column + " FROM " + e.Table
		out := captureStderr(t, func() {
			lintSQLForGotchas(e.DB, sql)
		})
		if !strings.Contains(out, e.Table) || !strings.Contains(out, e.Column) {
			t.Errorf("lintSQLForGotchas(%d, %q) did not warn about %s.%s — got: %q", e.DB, sql, e.Table, e.Column, out)
		}
	}
}

// TestLintSQLForGotchas_ScopedToDB pins that the lint is per-DB: a seeded
// (db=2, table, column) entry must NOT fire against a different db.
func TestLintSQLForGotchas_ScopedToDB(t *testing.T) {
	out := captureStderr(t, func() {
		lintSQLForGotchas(35, "SELECT stato FROM ordini")
	})
	if out != "" {
		t.Errorf("expected no warning for db=35 (entries are seeded for db=2), got: %q", out)
	}
}

// TestGotchaHint_MatchesLintedEntries pins that gotchaHint (the A3
// error-envelope hint) and lintSQLForGotchas (the pre-flight stderr warning)
// read from the SAME table: every entry lintSQLForGotchas can fire on also
// resolves via gotchaHint to the identical Hint text.
func TestGotchaHint_MatchesLintedEntries(t *testing.T) {
	for _, e := range gotchaTable {
		got := gotchaHint(e.DB, e.Table, e.Column)
		if got != e.Hint {
			t.Errorf("gotchaHint(%d, %q, %q) = %q, want %q", e.DB, e.Table, e.Column, got, e.Hint)
		}
	}
}

// TestGotchaHint_UnknownColumnReturnsEmpty pins the "no curated entry" case
// stays silent rather than fabricating a hint.
func TestGotchaHint_UnknownColumnReturnsEmpty(t *testing.T) {
	if got := gotchaHint(2, "ordini", "some_column_nobody_curated"); got != "" {
		t.Errorf("expected empty hint for an uncurated column, got %q", got)
	}
	if got := gotchaHint(2, "some_table_nobody_curated", "stato"); got != "" {
		t.Errorf("expected empty hint for an uncurated table, got %q", got)
	}
}

// TestOrdiniHintSharedSource is the carried-review "shared source" test:
// mutating gotchaTable changes what BOTH lintSQLForGotchas and gotchaHint
// report, proving there's exactly one copy of the data, not two that could
// drift the way the pre-W9 lint/ordiniHint split did.
func TestOrdiniHintSharedSource(t *testing.T) {
	orig := gotchaTable
	defer func() { gotchaTable = orig }()

	gotchaTable = []gotchaEntry{
		{DB: 9, Table: "widgets", Column: "gizmo", Hint: "test-only sentinel hint"},
	}

	if got := gotchaHint(9, "widgets", "gizmo"); got != "test-only sentinel hint" {
		t.Errorf("gotchaHint did not pick up the mutated table: got %q", got)
	}
	out := captureStderr(t, func() {
		lintSQLForGotchas(9, "SELECT gizmo FROM widgets")
	})
	if !strings.Contains(out, "test-only sentinel hint") {
		t.Errorf("lintSQLForGotchas did not pick up the mutated table: got %q", out)
	}

	// And the ordini entries the mutation clobbered must now be gone from
	// BOTH surfaces — proves neither one has a second, independent copy.
	if got := gotchaHint(2, "ordini", "stato"); got != "" {
		t.Errorf("expected the clobbered ordini entry to be gone from gotchaHint, got %q", got)
	}
	out2 := captureStderr(t, func() {
		lintSQLForGotchas(2, "SELECT stato FROM ordini")
	})
	if out2 != "" {
		t.Errorf("expected the clobbered ordini entry to be gone from the lint, got %q", out2)
	}
}

// TestLintSQLForGotchas_WordBoundaryNotSubstring is the review-caught
// regression test: a plain strings.Contains check on "nome" false-fires on
// any SQL containing "cognome" (proved live with a real `u.cognome` join) —
// same latent class as "stato" firing on "acquistato"/"prestato", which
// predates this wave but shares the fix. Column matching must be a
// case-insensitive whole word, not a substring.
func TestLintSQLForGotchas_WordBoundaryNotSubstring(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		wantColumn string // column whose warning must NOT fire
	}{
		{"cognome join does not fire prodotti.nome", "SELECT p.id, u.cognome FROM prodotti p JOIN users u ON u.id = p.user_id", "nome"},
		{"acquistato does not fire ordini.stato", "SELECT acquistato FROM ordini", "stato"},
		{"prestato does not fire ordini.stato", "SELECT prestato FROM ordini", "stato"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				lintSQLForGotchas(2, tc.sql)
			})
			if strings.Contains(out, "."+tc.wantColumn+" —") {
				t.Errorf("false positive: %q must NOT fire the %q warning, got: %q", tc.sql, tc.wantColumn, out)
			}
		})
	}
}

// TestLintSQLForGotchas_WholeWordStillFires pins the positive side: a bare
// whole-word reference to a curated column still fires, word-boundary
// matching didn't just suppress everything.
func TestLintSQLForGotchas_WholeWordStillFires(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		column string
		table  string
	}{
		{"bare nome fires prodotti.nome", "SELECT nome FROM prodotti", "nome", "prodotti"},
		{"qualified p.nome fires prodotti.nome", "SELECT p.nome FROM prodotti p", "nome", "prodotti"},
		{"bare stato fires ordini.stato", "SELECT stato FROM ordini", "stato", "ordini"},
		{"qualified o.stato fires ordini.stato", "SELECT o.stato FROM ordini o", "stato", "ordini"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				lintSQLForGotchas(2, tc.sql)
			})
			if !strings.Contains(out, tc.table+"."+tc.column) {
				t.Errorf("expected %q to fire the %s.%s warning, got: %q", tc.sql, tc.table, tc.column, out)
			}
		})
	}
}

// TestGotchasForTable pins the filter helper `schema --gotchas <table>` uses.
func TestGotchasForTable(t *testing.T) {
	entries := gotchasForTable(2, "ordini")
	if len(entries) != 2 {
		t.Fatalf("gotchasForTable(2, \"ordini\") = %d entries, want 2 (stato, totale_ordine)", len(entries))
	}
	for _, e := range entries {
		if e.Table != "ordini" {
			t.Errorf("gotchasForTable returned a non-ordini entry: %+v", e)
		}
	}
	// Case-insensitive table match.
	if got := gotchasForTable(2, "ORDINI"); len(got) != 2 {
		t.Errorf("gotchasForTable should match case-insensitively, got %d entries", len(got))
	}
	if got := gotchasForTable(2, "no_such_table"); len(got) != 0 {
		t.Errorf("gotchasForTable(2, \"no_such_table\") = %d entries, want 0", len(got))
	}
}

// TestSchemaGotchas_JSONGolden pins `mbx schema --gotchas`'s JSON output
// shape: an array of {db, table, column, hint} objects, unfiltered (all
// seeded entries) when no table arg is given, filtered to one table's
// entries when it is.
func TestSchemaGotchas_JSONGolden(t *testing.T) {
	origJSON := jsonFlag
	jsonFlag = true
	defer func() { jsonFlag = origJSON }()

	stdout := captureConfigStdout(t, func() {
		if err := runSchemaGotchas(schemaCmd, nil); err != nil {
			t.Fatalf("runSchemaGotchas: %v", err)
		}
	})
	for _, e := range gotchaTable {
		if !strings.Contains(stdout, e.Table) || !strings.Contains(stdout, e.Column) {
			t.Errorf("expected unfiltered --gotchas output to contain %s.%s, got: %s", e.Table, e.Column, stdout)
		}
	}

	filtered := captureConfigStdout(t, func() {
		if err := runSchemaGotchas(schemaCmd, []string{"coupons"}); err != nil {
			t.Fatalf("runSchemaGotchas: %v", err)
		}
	})
	if !strings.Contains(filtered, "coupons") {
		t.Errorf("expected filtered output to contain coupons, got: %s", filtered)
	}
	if strings.Contains(filtered, "\"table\":\"ordini\"") {
		t.Errorf("expected filtered output to NOT contain ordini entries, got: %s", filtered)
	}
}

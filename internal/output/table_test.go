package output

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it. Mirrors the pattern already used for --csv
// stdout capture in commands/query_test.go.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestPrintIndexTable_ActivityRecent is the suite's first table-rendering
// test (the chapter explicitly asked for one). It pins the {columns, rows}
// array-of-arrays shape simplifyQueryResponse produces for `mbx activity
// recent` — the exact shape that used to hard-fail ("no array at \"@this\"")
// on every TTY invocation before W5, because the old column map expected
// row objects from the long-removed /api/activity REST endpoint.
func TestPrintIndexTable_ActivityRecent(t *testing.T) {
	fixture := []byte(`{
		"columns": ["started_at","running_time","result_rows","context","error","cache_hit","card_id","dashboard_id","database_id","card_name","executor_email","database_name"],
		"rows": [
			["2026-07-20T09:00:00Z", 42, 1234, "question", null, true, 53, null, 2, "Orders by product", "analyst@example.com", "Example Replica"]
		],
		"row_count": 1
	}`)

	out := captureStdout(t, func() {
		if err := printTable("activity.recent", fixture); err != nil {
			t.Fatalf("printTable: %v", err)
		}
	})

	for _, want := range []string{"WHEN", "USER", "CARD", "analyst@example.com", "Orders by product"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered table missing %q:\n%s", want, out)
		}
	}
}

func TestPrintIndexTable_NoRows(t *testing.T) {
	fixture := []byte(`{"columns":["a"],"rows":[],"row_count":0}`)
	out := captureStdout(t, func() {
		if err := printTable("activity.recent", fixture); err != nil {
			t.Fatalf("printTable: %v", err)
		}
	})
	if !strings.Contains(out, "(no rows)") {
		t.Errorf("expected \"(no rows)\" for an empty row set, got: %q", out)
	}
}

// TestPrintIndexTable_MissingRowsErrors pins the negative case printTable
// must still surface as a non-errNoTable error (so PrintData knows to fall
// back to JSON rather than silently rendering an empty table).
func TestPrintIndexTable_MissingRowsErrors(t *testing.T) {
	fixture := []byte(`{"columns":["a"]}`) // no "rows" key at all
	if err := printTable("activity.recent", fixture); err == nil {
		t.Fatal("expected an error when \"rows\" is absent")
	}
}

// TestPrintData_TableRenderFailureFallsBackToJSON pins W5(b): any
// printTable error other than "no layout registered at all" degrades soft
// to JSON with a stderr note instead of failing the whole command — this is
// what de-risks every future column-map drift, not just activity.recent's.
func TestPrintData_TableRenderFailureFallsBackToJSON(t *testing.T) {
	// Neutralize the ambient CLAUDECODE=1 this test suite itself runs under
	// (compactRequested() would otherwise short-circuit useTable to false
	// and the test would pass without ever exercising the fallback path).
	t.Setenv("CLAUDECODE", "")
	t.Setenv("MBX_AGENT_MODE", "")
	origCompact := compactFlagInternal
	compactFlagInternal = false
	defer func() { compactFlagInternal = origCompact }()

	origTTY := isTTYFunc
	isTTYFunc = func() bool { return true }
	defer func() { isTTYFunc = origTTY }()

	// activity.recent is index-column-mapped and requires a "rows" array;
	// this fixture is missing it — the same failure class the pre-W5 code
	// hard-failed on for every real invocation.
	bogus := []byte(`{"columns":["a"]}`)

	origStderr := os.Stderr
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = wErr

	stdout := captureStdout(t, func() {
		if err := PrintData("activity.recent", bogus, false, ""); err != nil {
			t.Fatalf("PrintData must degrade soft, not error: %v", err)
		}
	})

	wErr.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(rErr)

	if !strings.Contains(stdout, `"columns"`) {
		t.Errorf("expected the raw JSON fallback on stdout, got: %q", stdout)
	}
	if !strings.Contains(string(stderrBytes), "falling back to JSON") {
		t.Errorf("expected a stderr note about the fallback, got: %q", stderrBytes)
	}
}

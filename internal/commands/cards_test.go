package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestCardsExecute_EchoesAuthoritativeDatabase pins W3's "cards execute
// echoes the card's authoritative database_id the same way" requirement:
// after executing, the response's own database_id (not any cached alias
// metadata) drives a "database: N (Name)" stderr echo — the visibility fix
// for "which DB did this actually run against" confusion.
func TestCardsExecute_EchoesAuthoritativeDatabase(t *testing.T) {
	resetDBListCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/card/53/query":
			_, _ = w.Write([]byte(`{"database_id":2,"row_count":1,"running_time":10,"data":{"cols":[{"name":"n"}],"rows":[[1]]}}`))
		case r.URL.Path == "/api/database":
			_, _ = w.Write([]byte(`{"data":[{"id":2,"name":"Example Replica","engine":"mysql"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	origStdout, origStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	runErr := cardsExecuteCmd.RunE(cardsExecuteCmd, []string{"53"})

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origStdout, origStderr
	_, _ = io.ReadAll(rOut)
	stderrBytes, _ := io.ReadAll(rErr)

	if runErr != nil {
		t.Fatalf("RunE: %v", runErr)
	}
	if !strings.Contains(string(stderrBytes), "database: 2 (Example Replica)") {
		t.Errorf("expected the resolved-DB echo on stderr, got: %q", stderrBytes)
	}
}

// TestCardsExecute_UnreachableDBHint pins W8's failure-path hint: when a
// card's query fails AND a follow-up probe of the card's own database also
// fails, the error must name the dead database so the failure reads as
// "the DB is down" rather than "the SQL/card is broken".
func TestCardsExecute_UnreachableDBHint(t *testing.T) {
	resetDBListCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/card/53/query":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"query failed"}`))
		case r.URL.Path == "/api/card/53":
			_, _ = w.Write([]byte(`{"id":53,"database_id":35,"name":"funnel"}`))
		case r.URL.Path == "/api/dataset":
			// The follow-up health probe against database_id=35 — also dead.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"database unreachable"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	runErr := cardsExecuteCmd.RunE(cardsExecuteCmd, []string{"53"})

	if runErr == nil {
		t.Fatal("expected an error when the card query fails")
	}
	if !strings.Contains(runErr.Error(), "card's database_id=35 unreachable") {
		t.Errorf("expected the unreachable-DB hint in the error, got: %v", runErr)
	}
	if !strings.Contains(runErr.Error(), "ordini-kpi") {
		t.Errorf("expected the hint to point at ordini-kpi, got: %v", runErr)
	}
}

// TestCardsExecute_NoHintWhenDBIsHealthy pins the negative case: a card
// query failure whose database probes healthy must NOT get the
// unreachable-DB hint appended — that would misdirect a genuine SQL/card
// bug as a database outage.
func TestCardsExecute_NoHintWhenDBIsHealthy(t *testing.T) {
	resetDBListCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/card/53/query":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Unknown column 'x'"}`))
		case r.URL.Path == "/api/card/53":
			_, _ = w.Write([]byte(`{"id":53,"database_id":2,"name":"orders-by-product"}`))
		case r.URL.Path == "/api/dataset":
			// The follow-up health probe succeeds — DB is fine, it's the SQL.
			_, _ = w.Write([]byte(`{"row_count":1,"running_time":1,"database_id":2,"data":{"cols":[{"name":"1"}],"rows":[[1]]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	runErr := cardsExecuteCmd.RunE(cardsExecuteCmd, []string{"53"})

	if runErr == nil {
		t.Fatal("expected an error when the card query fails")
	}
	if strings.Contains(runErr.Error(), "unreachable") {
		t.Errorf("must not append the unreachable-DB hint when the DB probes healthy, got: %v", runErr)
	}
}

// resetCardsExportFlags restores cardsExportFormat/cardsExportParams after a
// test mutates them via cardsExportCmd's flag vars directly.
func resetCardsExportFlags(t *testing.T) {
	t.Helper()
	origFormat, origParams := cardsExportFormat, cardsExportParams
	t.Cleanup(func() { cardsExportFormat, cardsExportParams = origFormat, origParams })
}

// TestCardsExport_CSV_200WithJSONErrorBodyRoutesThroughEnvelope pins the
// same real-world bug query --csv had (found via live smoke testing):
// Metabase's /api/card/:id/query/csv endpoint responds 200 OK with a JSON
// error body — not real CSV — when the underlying query fails. Before the
// shared streamOrEnvelope helper, cards export bare-io.Copy'd the response,
// so this JSON silently streamed to stdout as if it were CSV data, exit 0.
func TestCardsExport_CSV_200WithJSONErrorBodyRoutesThroughEnvelope(t *testing.T) {
	resetCardsExportFlags(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":"Unknown column 'bad_col' in 'field list'","error_type":"invalid-query"}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()
	cardsExportFormat = "csv"

	origStdout, origStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	runErr := cardsExportCmd.RunE(cardsExportCmd, []string{"53"})

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origStdout, origStderr
	stdoutBytes, _ := io.ReadAll(rOut)
	stderrBytes, _ := io.ReadAll(rErr)

	if runErr == nil {
		t.Fatal("expected an error for a 200-with-JSON-error-body export response")
	}
	if len(stdoutBytes) != 0 {
		t.Errorf("the JSON error body must NOT be streamed to stdout as if it were CSV, got: %q", stdoutBytes)
	}
	var envelopeLine string
	for _, line := range strings.Split(string(stderrBytes), "\n") {
		if strings.HasPrefix(line, `{"error"`) {
			envelopeLine = line
			break
		}
	}
	if envelopeLine == "" {
		t.Fatalf("expected an A3 envelope line on stderr, got: %q", stderrBytes)
	}
	if gjson.Get(envelopeLine, "error.kind").String() != "unknown_column" {
		t.Errorf("expected kind=unknown_column, got: %s", envelopeLine)
	}
}

// TestCardsExport_JSON_LegitCurlyBodyStreamsThroughUnchanged pins the
// negative case the review flagged: for --format json, a response body that
// starts with '{' can be LEGITIMATE export data, not an error. It must
// stream through byte-identical (no envelope, exit 0) as long as it has no
// "error" key — distinguishing this from the error case above must be based
// on the JSON shape, not just the leading byte.
func TestCardsExport_JSON_LegitCurlyBodyStreamsThroughUnchanged(t *testing.T) {
	resetCardsExportFlags(t)
	const legitBody = `{"count": 1, "columns": ["id"], "rows": [[1]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(legitBody))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()
	cardsExportFormat = "json"

	origStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	runErr := cardsExportCmd.RunE(cardsExportCmd, []string{"53"})

	wOut.Close()
	os.Stdout = origStdout
	stdoutBytes, _ := io.ReadAll(rOut)

	if runErr != nil {
		t.Fatalf("RunE: %v", runErr)
	}
	if string(stdoutBytes) != legitBody {
		t.Errorf("legit '{'-leading body must stream through byte-identical, got: %q, want: %q", stdoutBytes, legitBody)
	}
}

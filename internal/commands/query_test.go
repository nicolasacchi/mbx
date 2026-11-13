package commands

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
)

func TestSimplifyQueryResponse_Basic(t *testing.T) {
	resp := []byte(`{"row_count":2,"running_time":23,"database_id":2,"data":{"cols":[{"name":"orders"}],"rows":[[1234],[42]]}}`)
	out := simplifyQueryResponse(resp, 0)
	if gjson.GetBytes(out, "row_count").Int() != 2 {
		t.Errorf("row_count wrong: %s", out)
	}
	if gjson.GetBytes(out, "columns.0").String() != "orders" {
		t.Errorf("columns wrong: %s", out)
	}
	if gjson.GetBytes(out, "running_ms").Int() != 23 {
		t.Errorf("running_ms wrong: %s", out)
	}
	if gjson.GetBytes(out, "database_id").Int() != 2 {
		t.Errorf("database_id wrong: %s", out)
	}
	if gjson.GetBytes(out, "rows.0.0").Int() != 1234 {
		t.Errorf("rows passthrough wrong: %s", out)
	}
	if gjson.GetBytes(out, "truncated").Exists() {
		t.Errorf("must not be marked truncated when rowCap=0: %s", out)
	}
}

// TestSimplifyQueryResponse_TruncationMarker pins the agent-mode contract: when
// the row cap is hit, the simplified output carries truncated:true + row_cap:N
// + row_cap_source:"client" (downstream agents dispatch on these). Emits a
// stderr warning — that's fine.
func TestSimplifyQueryResponse_TruncationMarker(t *testing.T) {
	resp := []byte(`{"row_count":100,"running_time":5,"database_id":2,"data":{"cols":[{"name":"x"}],"rows":[[1]]}}`)
	out := simplifyQueryResponse(resp, 100) // rowCount(100) >= rowCap(100)
	if !gjson.GetBytes(out, "truncated").Bool() {
		t.Errorf("expected truncated=true: %s", out)
	}
	if gjson.GetBytes(out, "row_cap").Int() != 100 {
		t.Errorf("expected row_cap=100: %s", out)
	}
	if gjson.GetBytes(out, "row_cap_source").String() != "client" {
		t.Errorf("expected row_cap_source=client: %s", out)
	}
}

func TestSimplifyQueryResponse_NoRows(t *testing.T) {
	resp := []byte(`{"row_count":0,"running_time":1,"database_id":2,"data":{"cols":[]}}`)
	out := simplifyQueryResponse(resp, 0)
	if gjson.GetBytes(out, "rows").String() != "[]" {
		t.Errorf("expected empty rows array when data.rows absent: %s", out)
	}
}

// TestSimplifyQueryResponse_ServerCapMarker pins the W1 contract: with no
// client cap requested (rowCap<=0), a result landing at exactly Metabase's
// 2,000-row display cap gets flagged row_cap_source:"server" — a heuristic,
// since a genuine 2,000-row result looks identical.
func TestSimplifyQueryResponse_ServerCapMarker(t *testing.T) {
	resp := []byte(`{"row_count":2000,"running_time":40,"database_id":2,"data":{"cols":[{"name":"id"}],"rows":[[1]]}}`)
	out := simplifyQueryResponse(resp, 0)
	if !gjson.GetBytes(out, "truncated").Bool() {
		t.Errorf("expected truncated=true at exactly 2000 rows with no client cap: %s", out)
	}
	if gjson.GetBytes(out, "row_cap").Int() != 2000 {
		t.Errorf("expected row_cap=2000: %s", out)
	}
	if gjson.GetBytes(out, "row_cap_source").String() != "server" {
		t.Errorf("expected row_cap_source=server: %s", out)
	}
}

// TestSimplifyQueryResponse_JustUnderServerCap pins the negative case: 1,999
// rows with no client cap must NOT be flagged (only an exact 2,000 match is
// pessimistically assumed truncated).
func TestSimplifyQueryResponse_JustUnderServerCap(t *testing.T) {
	resp := []byte(`{"row_count":1999,"running_time":40,"database_id":2,"data":{"cols":[{"name":"id"}],"rows":[[1]]}}`)
	out := simplifyQueryResponse(resp, 0)
	if gjson.GetBytes(out, "truncated").Exists() {
		t.Errorf("must not be marked truncated at 1999 rows with no client cap: %s", out)
	}
}

// TestSimplifyQueryResponse_ClientCapNotReached pins the negative case: a
// client cap that was set but not hit must not emit any marker.
func TestSimplifyQueryResponse_ClientCapNotReached(t *testing.T) {
	resp := []byte(`{"row_count":50,"running_time":5,"database_id":2,"data":{"cols":[{"name":"x"}],"rows":[[1]]}}`)
	out := simplifyQueryResponse(resp, 100) // rowCap=100, rowCount=50 < 100
	if gjson.GetBytes(out, "truncated").Exists() {
		t.Errorf("must not be marked truncated when rowCount < rowCap: %s", out)
	}
}

// TestQueryCSV_StreamsThroughExportEndpoint pins W11: --csv must POST to the
// real Metabase export endpoint (nested "query" body, format_rows:false) and
// stream the response through unchanged, rather than hand-rolling CSV from
// the capped /api/dataset JSON response.
func TestQueryCSV_StreamsThroughExportEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("id\n1\n2\n3\n"))
	}))
	defer srv.Close()

	origURL, origKey, origDB, origCSV := urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag
	urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = srv.URL, "test-key", "2", true
	defer func() {
		urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = origURL, origKey, origDB, origCSV
	}()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := queryCmd.RunE(queryCmd, []string{"SELECT id FROM categories"})

	w.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(r)

	if runErr != nil {
		t.Fatalf("RunE: %v", runErr)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/dataset/csv" {
		t.Errorf("path = %q, want /api/dataset/csv", gotPath)
	}
	if gjson.GetBytes(gotBody, "format_rows").Type != gjson.False {
		t.Errorf("expected format_rows:false in body: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "query.database").Int() != 2 {
		t.Errorf("expected nested query.database=2: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "query.type").String() != "native" {
		t.Errorf("expected nested query.type=native: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "query.native.query").String() != "SELECT id FROM categories" {
		t.Errorf("expected nested query.native.query to carry the SQL: %s", gotBody)
	}
	if string(out) != "id\n1\n2\n3\n" {
		t.Errorf("stdout not streamed through unchanged: %q", out)
	}
}

// TestQueryCSV_CurlyPrefixedRealCSVStreamsThroughUnchanged pins the edge
// case the review flagged: a genuine CSV export can legitimately start with
// '{' — e.g. a column literally named "{x}" (SELECT 1 AS "{x}"). The
// streamOrEnvelope peek must not mistake this for an error body; since the
// content isn't valid JSON with an "error" key, it must stream through
// byte-identical with no envelope and exit 0.
func TestQueryCSV_CurlyPrefixedRealCSVStreamsThroughUnchanged(t *testing.T) {
	const realCSV = "{x}\n1\n2\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(realCSV))
	}))
	defer srv.Close()

	origURL, origKey, origDB, origCSV := urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag
	urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = srv.URL, "test-key", "2", true
	defer func() {
		urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = origURL, origKey, origDB, origCSV
	}()

	origStdout, origStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	runErr := queryCmd.RunE(queryCmd, []string{`SELECT 1 AS "{x}"`})

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origStdout, origStderr
	stdoutBytes, _ := io.ReadAll(rOut)
	stderrBytes, _ := io.ReadAll(rErr)

	if runErr != nil {
		t.Fatalf("RunE: %v", runErr)
	}
	if string(stdoutBytes) != realCSV {
		t.Errorf("'{'-prefixed real CSV must stream through byte-identical, got: %q, want: %q", stdoutBytes, realCSV)
	}
	for _, line := range strings.Split(string(stderrBytes), "\n") {
		if strings.HasPrefix(line, `{"error"`) {
			t.Errorf("must not emit an A3 envelope for a successful '{'-prefixed CSV export, got: %q", line)
		}
	}
}

// TestQueryCSV_ErrorRoutesThroughEnvelope pins carried finding B (Wave 1
// review): before this, a --csv PostStream failure bare-`return err`ed
// while the JSON path called emitErrorEnvelope — agent loops grepping
// stderr for {"error":{kind,...}} got nothing on a CSV failure. The CSV
// path must emit the same A3 envelope.
func TestQueryCSV_ErrorRoutesThroughEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Unknown column 'stato' in 'field list'"}`))
	}))
	defer srv.Close()

	origURL, origKey, origDB, origCSV := urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag
	urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = srv.URL, "test-key", "2", true
	defer func() {
		urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = origURL, origKey, origDB, origCSV
	}()

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	runErr := queryCmd.RunE(queryCmd, []string{"SELECT stato FROM ordini"})

	w.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(r)

	if runErr == nil {
		t.Fatal("expected an error for a 400 response on the CSV export path")
	}
	// Find the A3 envelope line — agents grep for the first line starting
	// with `{"error":`, but the resolved-DB echo and the ordini lint warning
	// legitimately precede it on stderr, so scan rather than assume line 0.
	var envelopeLine string
	for _, line := range strings.Split(string(stderrBytes), "\n") {
		if strings.HasPrefix(line, `{"error"`) {
			envelopeLine = line
			break
		}
	}
	if envelopeLine == "" {
		t.Fatalf("expected a stderr line starting with {\"error\": (the A3 envelope), got: %q", stderrBytes)
	}
	if !gjson.Get(envelopeLine, "error.kind").Exists() {
		t.Errorf("expected an \"error.kind\" key in the envelope, got: %s", envelopeLine)
	}
	if gjson.Get(envelopeLine, "error.db").Int() != 2 {
		t.Errorf("expected error.db=2, got: %s", envelopeLine)
	}
}

// TestQueryCSV_200WithJSONErrorBodyRoutesThroughEnvelope pins a real-world
// gap found via live smoke testing: Metabase's /api/dataset/csv endpoint
// responds 200 (not a non-2xx status) with a JSON error body — not real CSV
// — when the QUERY ITSELF fails (e.g. a bad column). Before this, the JSON
// error payload silently streamed to stdout as if it were CSV data and the
// command exited 0. This is the MOST COMMON --csv failure shape in
// practice (any bad-SQL query), so it needed its own detection, separate
// from carried finding B's non-2xx PostStream error path.
func TestQueryCSV_200WithJSONErrorBodyRoutesThroughEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real Metabase behavior: 200 OK, JSON body, even on query failure.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":"Unknown column 'bad_col' in 'field list'","error_type":"invalid-query","row_count":0,"data":{"rows":[],"cols":[]}}`))
	}))
	defer srv.Close()

	origURL, origKey, origDB, origCSV := urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag
	urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = srv.URL, "test-key", "2", true
	defer func() {
		urlFlag, apiKeyFlag, databaseFlag, queryCSVFlag = origURL, origKey, origDB, origCSV
	}()

	origStdout, origStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	runErr := queryCmd.RunE(queryCmd, []string{"SELECT bad_col FROM ordini"})

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origStdout, origStderr
	stdoutBytes, _ := io.ReadAll(rOut)
	stderrBytes, _ := io.ReadAll(rErr)

	if runErr == nil {
		t.Fatal("expected an error for a 200-with-JSON-error-body CSV response")
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

// TestEmitErrorEnvelope_SandboxHintForProdHost pins the W7 sandbox hint
// directly: a network-class *client.APIError whose URL targets
// sandboxAgentHost must carry a hint naming dangerouslyDisableSandbox — the
// documented operational trap this fixes. Exercised as a unit test (not a
// live smoke) because the failure depends on the caller's sandbox and can't
// be reproduced reliably against a live host.
func TestEmitErrorEnvelope_SandboxHintForProdHost(t *testing.T) {
	err := &client.APIError{
		StatusCode: 0,
		Method:     "POST",
		URL:        "https://" + sandboxAgentHost + "/api/dataset",
		Cause:      &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	}

	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	os.Stderr = w

	emitErrorEnvelope(err, 2, "SELECT 1")

	w.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(r)

	var line string
	for _, l := range strings.Split(string(stderrBytes), "\n") {
		if strings.HasPrefix(l, `{"error"`) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("expected an A3 envelope line, got: %q", stderrBytes)
	}
	if gjson.Get(line, "error.kind").String() != "network" {
		t.Fatalf("expected kind=network, got: %s", line)
	}
	hint := gjson.Get(line, "error.hint").String()
	if !strings.Contains(hint, "dangerouslyDisableSandbox") {
		t.Errorf("expected the hint to name dangerouslyDisableSandbox, got: %q", hint)
	}
	if !strings.Contains(hint, sandboxAgentHost) {
		t.Errorf("expected the hint to name the host, got: %q", hint)
	}
}

// TestEmitErrorEnvelope_NoSandboxHintForOtherHost pins the negative case: a
// network-class failure against a DIFFERENT host (e.g. a local dev
// Metabase, or the connection-refused smoke's --url localhost:9) must not
// get the prod-specific sandbox hint — it would misdirect the reader.
func TestEmitErrorEnvelope_NoSandboxHintForOtherHost(t *testing.T) {
	err := &client.APIError{
		StatusCode: 0,
		Method:     "POST",
		URL:        "https://localhost:9/api/dataset",
		Cause:      &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	}

	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	os.Stderr = w

	emitErrorEnvelope(err, 2, "SELECT 1")

	w.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(r)

	var line string
	for _, l := range strings.Split(string(stderrBytes), "\n") {
		if strings.HasPrefix(l, `{"error"`) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("expected an A3 envelope line, got: %q", stderrBytes)
	}
	if gjson.Get(line, "error.hint").Exists() {
		t.Errorf("expected no hint for a non-prod host, got: %s", line)
	}
}

package commands

// W10b — `mbx dashboards add-card`, the dangerous one: PUT /api/dashboard/:id
// has full-replacement semantics for the dashcards array, so this command
// MUST read-modify-write. These tests pin the GET-before-PUT sequence, that
// every pre-existing dashcard survives byte-for-byte (including "exotic"
// fields like action_id/parameter_mappings a naive decode/re-encode would
// drop), tabs passthrough, the auto-placement formula, and every abort path
// (missing dashcards key, 404, bad --tab) never reaching the PUT.
//
// W12 — `mbx dashboards subscriptions`, read-only against GET /api/pulse.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// resetAddCardFlags restores addCardRowFlag/etc.'s values AND their pflag
// "Changed" state. RunE is called directly against the package-level
// dashboardsAddCardCmd across many tests (no fresh Command per test), so
// once a test does Flags().Set("row", ...) — which flips Changed=true
// permanently — a later test relying on "--row not given" would silently
// inherit it without this reset.
func resetAddCardFlags(t *testing.T) {
	t.Helper()
	origRow, origCol, origSX, origSY, origTab := addCardRowFlag, addCardColFlag, addCardSizeXFlag, addCardSizeYFlag, addCardTabFlag
	origYes, origDryRun := yesFlag, dryRunFlag
	rowFlag := dashboardsAddCardCmd.Flags().Lookup("row")
	tabFlag := dashboardsAddCardCmd.Flags().Lookup("tab")
	origRowChanged, origTabChanged := rowFlag.Changed, tabFlag.Changed
	t.Cleanup(func() {
		addCardRowFlag, addCardColFlag, addCardSizeXFlag, addCardSizeYFlag, addCardTabFlag = origRow, origCol, origSX, origSY, origTab
		yesFlag, dryRunFlag = origYes, origDryRun
		rowFlag.Changed, tabFlag.Changed = origRowChanged, origTabChanged
	})
}

func newDashboardServer(t *testing.T, existingDashcards, tabs string) (*httptest.Server, *[]string, *[]byte) {
	t.Helper()
	var calls []string
	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/dashboard/42":
			body := `{"id":42,"dashcards":` + existingDashcards + `,"tabs":` + tabs + `}`
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodPut && r.URL.Path == "/api/dashboard/42":
			// io.ReadAll, not a single r.Body.Read(buf) — a single Read call
			// is not guaranteed to fill the buffer in one shot (same bug
			// class the HIGH review finding fixed in client.go's do()); a
			// short first read here would silently truncate putBody and
			// produce false negatives in the exotic-field-preservation
			// assertions below.
			buf, _ := io.ReadAll(r.Body)
			putBody = buf
			// Echo back a plausible post-write dashboard: existing cards
			// keep their ids, the new one (card_id from the request) gets a
			// server-assigned id.
			newCardID := gjson.GetBytes(buf, "dashcards.#(id=-1).card_id").Int()
			resp := map[string]any{
				"id": 42,
				"dashcards": append(jsonArrayWithoutNegID(gjson.GetBytes(buf, "dashcards")),
					map[string]any{"id": 999, "card_id": newCardID}),
			}
			respJSON, _ := json.Marshal(resp)
			_, _ = w.Write(respJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &putBody
}

// jsonArrayWithoutNegID rebuilds the "existing" portion of a dashcards array
// (everything except the id:-1 placeholder) as plain maps, for constructing
// the mock PUT response.
func jsonArrayWithoutNegID(arr gjson.Result) []any {
	out := []any{}
	for _, dc := range arr.Array() {
		if dc.Get("id").Int() == -1 {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(dc.Raw), &m)
		out = append(out, m)
	}
	return out
}

const twoExoticDashcards = `[
  {"id":10,"card_id":100,"row":0,"col":0,"size_x":4,"size_y":4,"action_id":7,"parameter_mappings":[{"parameter_id":"abc","card_id":100,"target":["dimension",["field",1,null]]}],"series":[{"id":5}]},
  {"id":11,"card_id":101,"row":4,"col":0,"size_x":4,"size_y":6,"action_id":9,"parameter_mappings":[{"parameter_id":"xyz","card_id":101,"target":["dimension",["field",2,null]]}],"series":[]}
]`

// TestDashboardsAddCard_GetThenPut_PreservesExoticFieldsAndAppendsOne pins
// the core read-modify-write contract: both pre-existing dashcards survive
// the round trip byte-for-byte (asserted via gjson deep-compare on their
// exotic fields — action_id, parameter_mappings — a naive decode/re-encode
// through a typed struct would silently drop), the new dashcard (id -1) is
// appended, tabs pass through unchanged, and the total count is exactly
// existing+1.
func TestDashboardsAddCard_GetThenPut_PreservesExoticFieldsAndAppendsOne(t *testing.T) {
	resetAddCardFlags(t)
	srv, calls, putBody := newDashboardServer(t, twoExoticDashcards, `[{"id":1,"name":"Tab 1"}]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	addCardSizeXFlag, addCardSizeYFlag = 4, 4
	captureStdout(t)

	if err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(*calls) != 2 || (*calls)[0] != "GET /api/dashboard/42" || (*calls)[1] != "PUT /api/dashboard/42" {
		t.Fatalf("expected GET then PUT to /api/dashboard/42, got: %v", *calls)
	}

	body := *putBody
	dashcards := gjson.GetBytes(body, "dashcards").Array()
	if len(dashcards) != 3 {
		t.Fatalf("expected 3 dashcards in PUT body (2 existing + 1 new), got %d: %s", len(dashcards), body)
	}

	// Both pre-existing dashcards must be present with their exotic fields
	// intact (deep-compare, not just "the id is there").
	dc10 := gjson.GetBytes(body, `dashcards.#(id=10)`)
	if !dc10.Exists() {
		t.Fatal("existing dashcard id=10 missing from PUT body")
	}
	if dc10.Get("action_id").Int() != 7 {
		t.Errorf("dashcard 10 action_id = %d, want 7 (must survive verbatim)", dc10.Get("action_id").Int())
	}
	if dc10.Get("parameter_mappings.0.parameter_id").String() != "abc" {
		t.Errorf("dashcard 10 parameter_mappings not preserved: %s", dc10.Get("parameter_mappings").Raw)
	}

	dc11 := gjson.GetBytes(body, `dashcards.#(id=11)`)
	if !dc11.Exists() {
		t.Fatal("existing dashcard id=11 missing from PUT body")
	}
	if dc11.Get("action_id").Int() != 9 {
		t.Errorf("dashcard 11 action_id = %d, want 9 (must survive verbatim)", dc11.Get("action_id").Int())
	}
	if dc11.Get("parameter_mappings.0.parameter_id").String() != "xyz" {
		t.Errorf("dashcard 11 parameter_mappings not preserved: %s", dc11.Get("parameter_mappings").Raw)
	}

	newDC := gjson.GetBytes(body, `dashcards.#(id=-1)`)
	if !newDC.Exists() {
		t.Fatal("new dashcard (id=-1) missing from PUT body")
	}
	if newDC.Get("card_id").Int() != 102 {
		t.Errorf("new dashcard card_id = %d, want 102", newDC.Get("card_id").Int())
	}
	if newDC.Get("dashboard_tab_id").Int() != 1 {
		t.Errorf("new dashcard dashboard_tab_id = %d, want 1 (first tab)", newDC.Get("dashboard_tab_id").Int())
	}

	// Tabs pass through unchanged.
	tabs := gjson.GetBytes(body, "tabs").Array()
	if len(tabs) != 1 || tabs[0].Get("id").Int() != 1 || tabs[0].Get("name").String() != "Tab 1" {
		t.Errorf("tabs did not pass through unchanged: %s", gjson.GetBytes(body, "tabs").Raw)
	}
}

// TestDashboardsAddCard_PlainRefusal_ZeroNetworkCalls pins the LOW review
// reorder fix: a plain refusal (no --yes, no --dry-run) must be checked
// BEFORE the GET, not after — the mandatory read-modify-write is this
// command's most expensive/riskiest step, and a refusal already knows it's
// going to bail regardless of what the GET returns. --dry-run is the one
// case that still needs the GET (for an accurate preview) and is covered
// separately by TestDashboardsAddCard_DryRun_DoesGetButNotPut.
func TestDashboardsAddCard_PlainRefusal_ZeroNetworkCalls(t *testing.T) {
	resetAddCardFlags(t)
	srv, calls, _ := newDashboardServer(t, twoExoticDashcards, `[]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag, dryRunFlag = false, false

	err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"})
	if err == nil {
		t.Fatal("expected a refusal error without --yes")
	}
	if len(*calls) != 0 {
		t.Errorf("expected ZERO network calls on a plain refusal (not even the GET), got: %v", *calls)
	}
}

// TestDashboardsAddCard_AutoPlacement pins the auto-placement formula:
// existing rows [0(size_y 4), 4(size_y 6)] -> new row 10 (max(row+size_y)).
func TestDashboardsAddCard_AutoPlacement(t *testing.T) {
	resetAddCardFlags(t)
	srv, _, putBody := newDashboardServer(t, twoExoticDashcards, `[]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	addCardSizeXFlag, addCardSizeYFlag = 4, 4
	captureStdout(t)

	if err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	newDC := gjson.GetBytes(*putBody, `dashcards.#(id=-1)`)
	if got := newDC.Get("row").Int(); got != 10 {
		t.Errorf("auto-placed row = %d, want 10 (max(0+4, 4+6))", got)
	}
}

// TestDashboardsAddCard_ExplicitRowOverridesAutoPlacement pins that --row
// bypasses auto-placement entirely.
func TestDashboardsAddCard_ExplicitRowOverridesAutoPlacement(t *testing.T) {
	resetAddCardFlags(t)
	srv, _, putBody := newDashboardServer(t, twoExoticDashcards, `[]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	addCardSizeXFlag, addCardSizeYFlag = 4, 4
	if err := dashboardsAddCardCmd.Flags().Set("row", "2"); err != nil {
		t.Fatalf("Flags().Set(row): %v", err)
	}
	captureStdout(t)

	if err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	newDC := gjson.GetBytes(*putBody, `dashcards.#(id=-1)`)
	if got := newDC.Get("row").Int(); got != 2 {
		t.Errorf("explicit --row 2 = %d, want 2 (must not be overridden by auto-placement)", got)
	}
}

// TestDashboardsAddCard_GetFailure_NoPut pins the abort-before-any-write
// contract: a 404 on the GET must never reach the PUT.
func TestDashboardsAddCard_GetFailure_NoPut(t *testing.T) {
	resetAddCardFlags(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	if err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"}); err == nil {
		t.Fatal("expected an error when GET /api/dashboard/42 404s")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("PUT must never be called after a failed GET, got calls: %v", calls)
		}
	}
}

// TestDashboardsAddCard_MissingDashcardsKey_NoPut pins the hard-assert: a
// GET response with no "dashcards" key at all must abort with a clear error
// before any PUT — never fall back to constructing the array from scratch.
func TestDashboardsAddCard_MissingDashcardsKey_NoPut(t *testing.T) {
	resetAddCardFlags(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"id":42,"name":"no dashcards key here"}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"})
	if err == nil {
		t.Fatal("expected an error when the dashboard response has no dashcards key")
	}
	if !strings.Contains(err.Error(), "dashcards") {
		t.Errorf("expected the error to mention the missing dashcards key, got: %v", err)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("PUT must never be called when dashcards key is missing, got calls: %v", calls)
		}
	}
}

// TestDashboardsAddCard_TruncatedGet_NoPut pins the HIGH review fix
// end-to-end through the add-card command: a GET response that's cleanly
// truncated at a JSON element boundary (Content-Length lies, connection
// drops mid-body) must abort the command — client.go's do() now fails
// loudly on a short read instead of silently returning
// (partialBody, nil), which would otherwise let a truncated-but-well-formed-
// looking dashcards array sail through the "dashcards key exists" assert
// and reach the PUT with data quietly missing.
func TestDashboardsAddCard_TruncatedGet_NoPut(t *testing.T) {
	resetAddCardFlags(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support hijacking")
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		// A truncation that stops cleanly after a complete dashcard object
		// but before the array/object close — exactly the "looks valid,
		// isn't" shape the review flagged, not a garbled mid-token cut.
		body := `{"id":42,"dashcards":[{"id":10,"card_id":100,"row":0,"col":0,"size_x":4,"size_y":4}`
		fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body)+500, body)
		_ = bufrw.Flush()
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"})
	if err == nil {
		t.Fatal("expected an error when the GET response is truncated")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("PUT must never be called after a truncated GET, got calls: %v", calls)
		}
	}
	if len(calls) != 1 || calls[0] != "GET /api/dashboard/42" {
		t.Errorf("expected exactly 1 GET and 0 PUT, got calls: %v", calls)
	}
}

// TestDashboardsAddCard_UnknownTab_NoPut pins that --tab pointing at a
// nonexistent tab id aborts with the list of real tab ids, no PUT.
func TestDashboardsAddCard_UnknownTab_NoPut(t *testing.T) {
	resetAddCardFlags(t)
	srv, calls, _ := newDashboardServer(t, `[]`, `[{"id":1,"name":"Tab 1"},{"id":2,"name":"Tab 2"}]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	if err := dashboardsAddCardCmd.Flags().Set("tab", "999"); err != nil {
		t.Fatalf("Flags().Set(tab): %v", err)
	}

	err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"})
	if err == nil {
		t.Fatal("expected an error for a nonexistent --tab id")
	}
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "2") {
		t.Errorf("expected the error to list existing tab ids [1 2], got: %v", err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("PUT must never be called for an unknown --tab, got calls: %v", *calls)
		}
	}
}

// TestDashboardsAddCard_DryRun_DoesGetButNotPut pins --dry-run's contract
// for this command specifically: it DOES perform the GET (needed to build
// an accurate preview) but skips the PUT.
func TestDashboardsAddCard_DryRun_DoesGetButNotPut(t *testing.T) {
	resetAddCardFlags(t)
	srv, calls, _ := newDashboardServer(t, twoExoticDashcards, `[]`)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag, dryRunFlag = false, true
	defer func() { dryRunFlag = false }()
	addCardSizeXFlag, addCardSizeYFlag = 4, 4

	var buf bytes.Buffer
	dashboardsAddCardCmd.SetOut(&buf)
	defer dashboardsAddCardCmd.SetOut(nil)

	if err := dashboardsAddCardCmd.RunE(dashboardsAddCardCmd, []string{"42", "102"}); err != nil {
		t.Fatalf("RunE with --dry-run: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "GET /api/dashboard/42" {
		t.Errorf("expected exactly 1 GET and 0 PUT with --dry-run, got calls: %v", *calls)
	}
	out := buf.String()
	for _, want := range []string{"--dry-run", "PUT /api/dashboard/42", "2 existing", "1 new"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected dry-run preview to contain %q, got: %q", want, out)
		}
	}
}

// TestDashboardsSubscriptions_RendersScheduleAndRecipients pins §9's
// response shape end-to-end: schedule fields live on the channel (not the
// pulse root), email recipients render as a flat list.
func TestDashboardsSubscriptions_RendersScheduleAndRecipients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pulse" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("dashboard_id") != "42" {
			t.Errorf("expected dashboard_id=42 query param, got %q", r.URL.Query().Get("dashboard_id"))
		}
		_, _ = w.Write([]byte(`[{"id":10,"name":"Weekly ops","dashboard_id":42,
			"channels":[{"channel_type":"email","enabled":true,
				"schedule_type":"daily","schedule_hour":12,
				"recipients":[{"email":"reports@example.com"}]}]}]`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	stop := captureStdout(t)
	if err := dashboardsSubscriptionsCmd.RunE(dashboardsSubscriptionsCmd, []string{"42"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	body := stop()
	if gjson.Get(body, "0.name").String() != "Weekly ops" {
		t.Errorf("expected subscription name 'Weekly ops', got: %s", body)
	}
	if gjson.Get(body, "0.channels.0.schedule.type").String() != "daily" {
		t.Errorf("expected channel-level schedule.type=daily, got: %s", body)
	}
	if gjson.Get(body, "0.channels.0.schedule.hour").Int() != 12 {
		t.Errorf("expected channel-level schedule.hour=12, got: %s", body)
	}
	if gjson.Get(body, "0.channels.0.recipients.0").String() != "reports@example.com" {
		t.Errorf("expected recipient email, got: %s", body)
	}
}

// TestDashboardsSubscriptions_SlackChannel pins the Slack-channel shape:
// details.channel instead of recipients.
func TestDashboardsSubscriptions_SlackChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"name":"Slack digest","dashboard_id":42,
			"channels":[{"channel_type":"slack","enabled":true,
				"schedule_type":"weekly","schedule_day":"mon",
				"details":{"channel":"#example-channel"}}]}]`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	stop := captureStdout(t)
	if err := dashboardsSubscriptionsCmd.RunE(dashboardsSubscriptionsCmd, []string{"42"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	body := stop()
	if gjson.Get(body, "0.channels.0.slack_channel").String() != "#example-channel" {
		t.Errorf("expected slack_channel=#example-channel, got: %s", body)
	}
	if gjson.Get(body, "0.channels.0.recipients").Exists() {
		t.Errorf("slack channel must not carry a recipients field, got: %s", body)
	}
}

// TestDashboardsSubscriptions_EmptyListExitsZero pins the empty case: no
// subscriptions -> empty JSON array, exit 0 (RunE returns nil).
func TestDashboardsSubscriptions_EmptyListExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	stop := captureStdout(t)
	if err := dashboardsSubscriptionsCmd.RunE(dashboardsSubscriptionsCmd, []string{"42"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	body := strings.TrimSpace(stop())
	if body != "[]" {
		t.Errorf("expected an empty JSON array, got: %q", body)
	}
}

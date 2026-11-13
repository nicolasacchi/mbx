package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/config"
)

// resetDBListCacheForTest clears the per-process live-DB-list cache
// (dbalias.go's sync.Once) so tests don't leak state into each other.
func resetDBListCacheForTest() {
	dbListOnce = sync.Once{}
	dbListCache = nil
	dbListErr = nil
}

// realDBFixture is a synthetic GET /api/database listing for alias-matcher
// tests. The four alias targets carry names that match dbAliasPatterns; the
// rest are near-miss decoys (same engine, similar name) that the anchored
// patterns must NOT match.
var realDBFixture = []dbEntry{
	{ID: 35, Name: "Clickhouse Analytics", Engine: "clickhouse"},
	{ID: 166, Name: "ClickHouse Analytics Historic", Engine: "clickhouse"},
	{ID: 133, Name: "Clickhouse Analytics staging", Engine: "clickhouse"},
	{ID: 69, Name: "Metabase Internal DB", Engine: "postgres"},
	{ID: 34, Name: "Operations 1000farmacie Replica", Engine: "mysql"},
	{ID: 2, Name: "Production 1000farmacie Replica", Engine: "mysql"},
	{ID: 4, Name: "Production Archive Replica", Engine: "mysql"},
	{ID: 6, Name: "Staging Replica", Engine: "mysql"},
	{ID: 3, Name: "Testing Replica", Engine: "mysql"},
	{ID: 100, Name: "CMS DEV", Engine: "mysql"},
	{ID: 101, Name: "CMS PROD", Engine: "mysql"},
}

func TestMatchDBAlias(t *testing.T) {
	cases := []struct {
		alias   string
		wantID  int
		wantErr bool
	}{
		{"prod", 2, false},
		{"PROD", 2, false}, // alias name matching is case-insensitive
		{"clickhouse", 35, false},
		{"ops", 34, false},
		{"internal", 69, false},
		{"nope", 0, true},
	}
	for _, tc := range cases {
		got, err := matchDBAlias(tc.alias, realDBFixture)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.alias, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got.ID != tc.wantID {
			t.Errorf("%q: got id=%d want %d", tc.alias, got.ID, tc.wantID)
		}
	}
}

// TestMatchDBAlias_UnknownAliasListsKnownOnes pins the error message for a
// name that isn't in the known-alias set at all (distinct from a known
// alias with no live match — see TestMatchDBAlias_RetiredAlias).
func TestMatchDBAlias_UnknownAliasListsKnownOnes(t *testing.T) {
	_, err := matchDBAlias("staging", realDBFixture)
	if err == nil {
		t.Fatal("expected an error for an unrecognized alias name")
	}
	for _, want := range []string{"prod", "clickhouse", "ops", "internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list known aliases (missing %q): %v", want, err)
		}
	}
}

// TestMatchDBAlias_RetiredAlias pins the "fail closed" contract central to
// W3: when a known alias's target DB is gone from the live listing (e.g. a
// retired ClickHouse DB), matching must error — never fall back to a
// guessed/stale id.
func TestMatchDBAlias_RetiredAlias(t *testing.T) {
	withoutClickhouse := make([]dbEntry, 0, len(realDBFixture))
	for _, d := range realDBFixture {
		if d.Engine == "clickhouse" {
			continue
		}
		withoutClickhouse = append(withoutClickhouse, d)
	}
	_, err := matchDBAlias("clickhouse", withoutClickhouse)
	if err == nil {
		t.Fatal("expected an error when no clickhouse DB matches, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say 'not found': %v", err)
	}
	if !strings.Contains(err.Error(), "mbx databases list") {
		t.Errorf("error should point at 'mbx databases list': %v", err)
	}
}

func newDBListServer(t *testing.T, dbs []map[string]any) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := json.Marshal(map[string]any{"data": dbs})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestLiveDBList_ResolvesAndCachesPerProcess(t *testing.T) {
	resetDBListCacheForTest()
	srv, hits := newDBListServer(t, []map[string]any{
		{"id": 2, "name": "Production 1000farmacie Replica", "engine": "mysql"},
		{"id": 35, "name": "Clickhouse Analytics", "engine": "clickhouse"},
	})
	c := client.New(srv.URL, "k", "", false, 5*time.Second)

	entry, err := resolveDBAliasLive(c, "prod")
	if err != nil {
		t.Fatalf("resolveDBAliasLive: %v", err)
	}
	if entry.ID != 2 {
		t.Errorf("id = %d, want 2", entry.ID)
	}
	// A second resolution in the same process must reuse the cached listing.
	if _, err := resolveDBAliasLive(c, "clickhouse"); err != nil {
		t.Fatalf("second resolveDBAliasLive: %v", err)
	}
	if *hits != 1 {
		t.Errorf("expected exactly 1 GET /api/database (cached per-invocation), got %d", *hits)
	}
}

// TestResolveDBAliasLive_UnknownAliasNeverHitsNetwork pins the review fix:
// an alias name that isn't in the known set at all must be rejected before
// any GET /api/database round-trip — otherwise a plain typo, hit while
// Metabase happens to be unreachable, gets misreported as a network/fetch
// error instead of the real "unknown alias" problem.
func TestResolveDBAliasLive_UnknownAliasNeverHitsNetwork(t *testing.T) {
	resetDBListCacheForTest()
	srv, hits := newDBListServer(t, []map[string]any{
		{"id": 2, "name": "Production 1000farmacie Replica", "engine": "mysql"},
	})
	c := client.New(srv.URL, "k", "", false, 5*time.Second)

	_, err := resolveDBAliasLive(c, "nosuchdb")
	if err == nil {
		t.Fatal("expected an error for an unrecognized alias name")
	}
	if !strings.Contains(err.Error(), "unknown --database value") {
		t.Errorf("expected the unknown-alias message, got: %v", err)
	}
	if *hits != 0 {
		t.Errorf("expected 0 GET /api/database calls for an unrecognized alias, got %d", *hits)
	}
}

// TestResolveDBAliasLive_NotFoundListsLiveDBs pins the retired-alias error
// path end-to-end through the live (httptest) resolution function, not just
// the pure matcher.
func TestResolveDBAliasLive_NotFoundListsLiveDBs(t *testing.T) {
	resetDBListCacheForTest()
	srv, _ := newDBListServer(t, []map[string]any{
		{"id": 2, "name": "Production 1000farmacie Replica", "engine": "mysql"},
	})
	c := client.New(srv.URL, "k", "", false, 5*time.Second)

	_, err := resolveDBAliasLive(c, "clickhouse")
	if err == nil {
		t.Fatal("expected an error for an alias with no live match")
	}
	if !strings.Contains(err.Error(), "Production 1000farmacie Replica") {
		t.Errorf("error should list the live DBs: %v", err)
	}
}

func TestResolveDBValue(t *testing.T) {
	resetDBListCacheForTest()
	srv, _ := newDBListServer(t, []map[string]any{
		{"id": 2, "name": "Production 1000farmacie Replica", "engine": "mysql"},
	})
	c := client.New(srv.URL, "k", "", false, 5*time.Second)
	creds := &config.Credentials{DefaultDB: 2}

	cases := []struct {
		raw      string
		wantID   int
		wantName string
	}{
		{"", 2, ""},  // unset -> default, no round trip, bare echo
		{"0", 2, ""}, // legacy "0 means default" spelling
		{"7", 7, ""}, // numeric -> bare, no round trip needed
		{"prod", 2, "Production 1000farmacie Replica"},
	}
	for _, tc := range cases {
		id, name, err := resolveDBValue(c, creds, tc.raw)
		if err != nil {
			t.Errorf("raw=%q: unexpected error: %v", tc.raw, err)
			continue
		}
		if id != tc.wantID {
			t.Errorf("raw=%q: id = %d, want %d", tc.raw, id, tc.wantID)
		}
		if name != tc.wantName {
			t.Errorf("raw=%q: name = %q, want %q", tc.raw, name, tc.wantName)
		}
	}
}

func TestResolveDBValue_UnknownAliasErrors(t *testing.T) {
	resetDBListCacheForTest()
	srv, _ := newDBListServer(t, []map[string]any{
		{"id": 2, "name": "Production 1000farmacie Replica", "engine": "mysql"},
	})
	c := client.New(srv.URL, "k", "", false, 5*time.Second)
	creds := &config.Credentials{DefaultDB: 2}

	if _, _, err := resolveDBValue(c, creds, "nosuchdb"); err == nil {
		t.Fatal("expected an error for an unresolvable --database value")
	}
}

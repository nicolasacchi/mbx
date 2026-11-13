package commands

// W10a — `mbx cards create`. Pins the exact POST /api/card body shape (all
// four required fields present, visualization_settings {} not null), id
// extraction from the response, and --alias registration.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/aliases"
)

// resetCardsCreateFlags restores cardsCreateCmd's flag vars (and the shared
// confirm-gate globals it depends on) after a test mutates them directly.
func resetCardsCreateFlags(t *testing.T) {
	t.Helper()
	origName, origSQL, origDisplay := cardsCreateName, cardsCreateSQL, cardsCreateDisplay
	origColl, origDesc, origAlias := cardsCreateCollection, cardsCreateDescription, cardsCreateAlias
	origYes, origDryRun, origDB := yesFlag, dryRunFlag, databaseFlag
	t.Cleanup(func() {
		cardsCreateName, cardsCreateSQL, cardsCreateDisplay = origName, origSQL, origDisplay
		cardsCreateCollection, cardsCreateDescription, cardsCreateAlias = origColl, origDesc, origAlias
		yesFlag, dryRunFlag, databaseFlag = origYes, origDryRun, origDB
	})
}

// TestCardsCreate_PostsExactBodyShapeAndExtractsID pins §9's minimal 4-field
// payload (name, display, visualization_settings, dataset_query) plus the
// optional collection_id/description fields, and that the command surfaces
// the created card's id/name/database_id.
func TestCardsCreate_PostsExactBodyShapeAndExtractsID(t *testing.T) {
	resetCardsCreateFlags(t)
	resetDBListCacheForTest()
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/card":
			gotMethod, gotPath = r.Method, r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"id":789,"name":"mbx-smoke-w4-card1","database_id":2}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	cardsCreateName = "mbx-smoke-w4-card1"
	cardsCreateSQL = "SELECT 1"
	databaseFlag = "2"

	captureStdout(t)

	if err := cardsCreateCmd.RunE(cardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/card" {
		t.Errorf("path = %q, want /api/card", gotPath)
	}
	for _, want := range []string{"name", "display", "visualization_settings", "dataset_query"} {
		if !gjson.GetBytes(gotBody, want).Exists() {
			t.Errorf("request body missing required field %q: %s", want, gotBody)
		}
	}
	if raw := gjson.GetBytes(gotBody, "visualization_settings").Raw; raw != "{}" {
		t.Errorf("visualization_settings = %q, want {} (not null/omitted)", raw)
	}
	if gjson.GetBytes(gotBody, "dataset_query.type").String() != "native" {
		t.Errorf("dataset_query.type = %q, want native", gjson.GetBytes(gotBody, "dataset_query.type").String())
	}
	if gjson.GetBytes(gotBody, "dataset_query.native.query").String() != "SELECT 1" {
		t.Errorf("dataset_query.native.query = %q, want %q", gjson.GetBytes(gotBody, "dataset_query.native.query").String(), "SELECT 1")
	}
	if gjson.GetBytes(gotBody, "dataset_query.database").Int() != 2 {
		t.Errorf("dataset_query.database = %d, want 2", gjson.GetBytes(gotBody, "dataset_query.database").Int())
	}
}

// TestCardsCreate_OptionalFieldsIncluded pins collection_id/description
// only appearing in the body when the corresponding flags are set.
func TestCardsCreate_OptionalFieldsIncluded(t *testing.T) {
	resetCardsCreateFlags(t)
	resetDBListCacheForTest()
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":790,"name":"x","database_id":2}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	cardsCreateName = "x"
	cardsCreateSQL = "SELECT 1"
	cardsCreateCollection = 42
	cardsCreateDescription = "a test card"
	databaseFlag = "2"

	captureStdout(t)

	if err := cardsCreateCmd.RunE(cardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if gjson.GetBytes(gotBody, "collection_id").Int() != 42 {
		t.Errorf("collection_id = %d, want 42", gjson.GetBytes(gotBody, "collection_id").Int())
	}
	if gjson.GetBytes(gotBody, "description").String() != "a test card" {
		t.Errorf("description = %q, want %q", gjson.GetBytes(gotBody, "description").String(), "a test card")
	}
}

// TestCardsCreate_AliasRegistration pins --alias registering the new card
// in the local aliases store (same Set/Save plumbing as `aliases sync`).
func TestCardsCreate_AliasRegistration(t *testing.T) {
	resetCardsCreateFlags(t)
	resetDBListCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":791,"name":"mbx-smoke-w4-card2","database_id":2}`))
	}))
	defer srv.Close()

	isolateConfigEnv(t)
	urlFlag, apiKeyFlag = srv.URL, "test-key"

	yesFlag = true
	cardsCreateName = "mbx-smoke-w4-card2"
	cardsCreateSQL = "SELECT 1"
	cardsCreateAlias = "mbx-smoke-w4-card2"
	databaseFlag = "2"

	captureStdout(t)

	if err := cardsCreateCmd.RunE(cardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	store, err := aliases.Load()
	if err != nil {
		t.Fatalf("aliases.Load: %v", err)
	}
	a, ok := store.Find("mbx-smoke-w4-card2")
	if !ok {
		t.Fatal("expected the new card to be registered under its alias")
	}
	if a.CardID != 791 {
		t.Errorf("CardID = %d, want 791", a.CardID)
	}
	if a.DatabaseID != 2 {
		t.Errorf("DatabaseID = %d, want 2", a.DatabaseID)
	}
}

// TestCardsCreate_AliasSaveFailurePrintsCardThenErrors pins the MEDIUM
// review fix: the card POST already succeeded by the time alias
// registration runs, so a failure there must NOT read as "nothing
// happened" — the created card's id/name/database_id must still reach
// stdout via the normal output path, and the returned error must
// explicitly say not to retry (retrying would create a SECOND card) and
// name the manual recovery command.
func TestCardsCreate_AliasSaveFailurePrintsCardThenErrors(t *testing.T) {
	resetCardsCreateFlags(t)
	resetDBListCacheForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":792,"name":"mbx-smoke-w4-card3","database_id":2}`))
	}))
	defer srv.Close()

	// Point XDG_CONFIG_HOME at a plain FILE, not a directory — aliases.Load
	// (called before Save) then fails deterministically: os.ReadFile on a
	// path through a non-directory component errors with something other
	// than os.ErrNotExist, so Load can't fall back to its "missing file ->
	// empty store" path. Whether it's Load or Save that fails is incidental
	// to this test; either way cards.go's alias-registration wrapper
	// returns a non-nil error, which is what's under test.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", notADir)

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag = true
	cardsCreateName = "mbx-smoke-w4-card3"
	cardsCreateSQL = "SELECT 1"
	cardsCreateAlias = "mbx-smoke-w4-card3"
	databaseFlag = "2"

	stop := captureStdout(t)
	runErr := cardsCreateCmd.RunE(cardsCreateCmd, nil)
	stdout := stop()

	if runErr == nil {
		t.Fatal("expected an error when alias registration fails")
	}
	for _, want := range []string{"card exists (id=792)", "do NOT retry create", "mbx aliases set", "--card-id 792"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("expected error to contain %q, got: %v", want, runErr)
		}
	}
	if gjson.Get(stdout, "id").Int() != 792 {
		t.Errorf("expected the created card's id on stdout despite the alias failure, got: %q", stdout)
	}
	if gjson.Get(stdout, "name").String() != "mbx-smoke-w4-card3" {
		t.Errorf("expected the created card's name on stdout, got: %q", stdout)
	}
}

// TestCardsCreate_DryRunSkipsNetwork pins --dry-run: no POST reaches the
// server, and the preview names the method/path/body.
func TestCardsCreate_DryRunSkipsNetwork(t *testing.T) {
	resetCardsCreateFlags(t)
	resetDBListCacheForTest()
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	yesFlag, dryRunFlag = false, true
	cardsCreateName = "mbx-smoke-w4-dryrun"
	cardsCreateSQL = "SELECT 1"
	databaseFlag = "2"
	defer func() { dryRunFlag = false }()

	var buf bytes.Buffer
	cardsCreateCmd.SetOut(&buf)
	defer cardsCreateCmd.SetOut(nil)

	if err := cardsCreateCmd.RunE(cardsCreateCmd, nil); err != nil {
		t.Fatalf("RunE with --dry-run: %v", err)
	}
	if writes != 0 {
		t.Errorf("expected 0 POSTs with --dry-run, got %d", writes)
	}
	out := buf.String()
	for _, want := range []string{"--dry-run", "POST /api/card", "mbx-smoke-w4-dryrun"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected dry-run preview to contain %q, got: %q", want, out)
		}
	}
}

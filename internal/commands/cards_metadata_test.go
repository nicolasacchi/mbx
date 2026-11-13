package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/aliases"
)

// TestCardsMetadata_PrimaryPath pins the happy path: GET
// /api/card/:id/query_metadata succeeds and its body is printed as-is.
func TestCardsMetadata_PrimaryPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":53,"database":{"id":2,"name":"prod"},"fields":[{"name":"id"}]}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	stdout := captureConfigStdout(t, func() {
		if err := cardsMetadataCmd.RunE(cardsMetadataCmd, []string{"53"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if gotPath != "/api/card/53/query_metadata" {
		t.Errorf("expected GET /api/card/53/query_metadata, got %q", gotPath)
	}
	if gjson.Get(stdout, "database.id").Int() != 2 {
		t.Errorf("expected the query_metadata body to be printed as-is, got: %s", stdout)
	}
}

// TestCardsMetadata_FallsBackToPlainCard pins the fallback: when
// query_metadata 404s (an older Metabase, or a misconfigured instance —
// query_metadata was added v0.51), the command falls back to plain
// GET /api/card/:id, same "richer endpoint, fall back" shape as
// tables.go:57-64.
func TestCardsMetadata_FallsBackToPlainCard(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/card/53/query_metadata":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case "/api/card/53":
			_, _ = w.Write([]byte(`{"id":53,"name":"orders-by-product","database_id":2}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	stdout := captureConfigStdout(t, func() {
		if err := cardsMetadataCmd.RunE(cardsMetadataCmd, []string{"53"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(paths) != 2 || paths[0] != "/api/card/53/query_metadata" || paths[1] != "/api/card/53" {
		t.Errorf("expected query_metadata then plain-card fallback, got paths: %v", paths)
	}
	if gjson.Get(stdout, "name").String() != "orders-by-product" {
		t.Errorf("expected the fallback plain-card body to be printed, got: %s", stdout)
	}
}

// TestCardsMetadata_BothFail pins that a failure on BOTH endpoints surfaces
// the fallback's error, not a nil/silent success.
func TestCardsMetadata_BothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	origStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut
	runErr := cardsMetadataCmd.RunE(cardsMetadataCmd, []string{"999"})
	wOut.Close()
	os.Stdout = origStdout
	_, _ = io.ReadAll(rOut)

	if runErr == nil {
		t.Fatal("expected an error when both query_metadata and plain-card fail")
	}
}

// TestCardsMetadata_ResolvesAlias pins that alias resolution (same plumbing
// as `cards get`/`cards execute`) works for the metadata command too.
func TestCardsMetadata_ResolvesAlias(t *testing.T) {
	isolateConfigEnv(t)
	store, err := aliases.Load()
	if err != nil {
		t.Fatalf("aliases.Load: %v", err)
	}
	store.Set(aliases.Alias{Alias: "funnel", CardID: 342, DatabaseID: 35, Name: "Website Funnel Trend", UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := aliases.Save(store); err != nil {
		t.Fatalf("aliases.Save: %v", err)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":342}`))
	}))
	defer srv.Close()

	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	_ = captureConfigStdout(t, func() {
		if err := cardsMetadataCmd.RunE(cardsMetadataCmd, []string{"funnel"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if gotPath != "/api/card/342/query_metadata" {
		t.Errorf("expected alias 'funnel' to resolve to card 342, got path %q", gotPath)
	}
}

package commands

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidateID_AcceptRejectMatrix is the W14 acceptance matrix: numeric
// ids pass through unchanged; anything path-traversal-shaped, non-numeric,
// negative, empty, or SQL-injection-shaped is rejected with a typed
// validation error (kind "validation", exit code 3).
func TestValidateID_AcceptRejectMatrix(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{"0", 0, false},
		{"12345", 12345, false},
		{"1/../2", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
		{"", 0, true},
		{"1 OR 1=1", 0, true},
		{"1;DROP TABLE x", 0, true},
		{"1.5", 0, true},
		{" 1", 0, true},
		{"1 ", 0, true},
	}
	for _, tc := range cases {
		got, err := validateID(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateID(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("validateID(%q) = %d, want %d", tc.in, got, tc.want)
		}
		if tc.wantErr && err != nil {
			typed, ok := err.(interface {
				Kind() string
				ExitCode() int
			})
			if !ok {
				t.Errorf("validateID(%q) error is not typed with Kind()/ExitCode(): %T", tc.in, err)
				continue
			}
			if typed.Kind() != "validation" {
				t.Errorf("validateID(%q) Kind() = %q, want validation", tc.in, typed.Kind())
			}
			if typed.ExitCode() != 3 {
				t.Errorf("validateID(%q) ExitCode() = %d, want 3", tc.in, typed.ExitCode())
			}
		}
	}
}

// --- spot tests: a representative sample of the guarded commands reject a
// non-numeric id BEFORE making any network call (an httptest server that
// would fail the test if it received a request proves this). ---

func failIfCalled(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call for an invalid id: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

func TestValidateID_DatabasesGetRejectsNonNumeric(t *testing.T) {
	srv := failIfCalled(t)
	defer srv.Close()
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	err := databasesGetCmd.RunE(databasesGetCmd, []string{"1 OR 1=1"})
	if err == nil {
		t.Fatal("expected a validation error for a non-numeric database id")
	}
	if kindOf(err, "") != "validation" {
		t.Errorf("expected kind=validation, got %v", err)
	}
}

func TestValidateID_TablesGetRejectsPathTraversalShapedID(t *testing.T) {
	srv := failIfCalled(t)
	defer srv.Close()
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	err := tablesGetCmd.RunE(tablesGetCmd, []string{"1/../2"})
	if err == nil {
		t.Fatal("expected a validation error for a path-traversal-shaped table id")
	}
}

func TestValidateID_CollectionsItemsRejectsEmpty(t *testing.T) {
	srv := failIfCalled(t)
	defer srv.Close()
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	err := collectionsItemsCmd.RunE(collectionsItemsCmd, []string{""})
	if err == nil {
		t.Fatal("expected a validation error for an empty collection id")
	}
}

func TestValidateID_DashboardsGetRejectsNonNumeric(t *testing.T) {
	srv := failIfCalled(t)
	defer srv.Close()
	origURL, origKey := urlFlag, apiKeyFlag
	urlFlag, apiKeyFlag = srv.URL, "test-key"
	defer func() { urlFlag, apiKeyFlag = origURL, origKey }()

	if err := dashboardsGetCmd.RunE(dashboardsGetCmd, []string{"abc"}); err == nil {
		t.Error("dashboards get: expected a validation error for a non-numeric id")
	}
	if err := dashboardsCardsCmd.RunE(dashboardsCardsCmd, []string{"abc"}); err == nil {
		t.Error("dashboards cards: expected a validation error for a non-numeric id")
	}
	if err := dashboardsRelatedCmd.RunE(dashboardsRelatedCmd, []string{"abc"}); err == nil {
		t.Error("dashboards related: expected a validation error for a non-numeric id")
	}
	if err := dashboardsRevisionsCmd.RunE(dashboardsRevisionsCmd, []string{"abc"}); err == nil {
		t.Error("dashboards revisions: expected a validation error for a non-numeric id")
	}
	if err := dashboardsSubscriptionsCmd.RunE(dashboardsSubscriptionsCmd, []string{"abc"}); err == nil {
		t.Error("dashboards subscriptions: expected a validation error for a non-numeric id")
	}
}

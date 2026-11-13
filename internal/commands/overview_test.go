package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
)

// TestOverview_PerDatabaseHealth pins W8: `overview` probes every database
// with a cheap SELECT 1 and reports OK/FAIL per database, distinguishing a
// genuinely dead database from "no data" (a database that stays down for
// days is otherwise indistinguishable from an empty result).
func TestOverview_PerDatabaseHealth(t *testing.T) {
	isolateConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/database":
			_, _ = w.Write([]byte(`{"data":[
				{"id":2,"name":"Example Replica","engine":"mysql"},
				{"id":35,"name":"Clickhouse Analytics","engine":"clickhouse"}
			]}`))
		case r.URL.Path == "/api/user/current":
			_, _ = w.Write([]byte(`{"id":1,"email":"x@example.com","common_name":"X","is_superuser":true}`))
		case r.URL.Path == "/api/dataset":
			var body struct {
				Database int `json:"database"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Database == 35 {
				// Fast, non-retryable failure — simulates the dead DB
				// without paying client.go's retry/backoff tax.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"database unreachable"}`))
				return
			}
			_, _ = w.Write([]byte(`{"row_count":1,"running_time":1,"database_id":2,"data":{"cols":[{"name":"1"}],"rows":[[1]]}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	origURL, origKey, origJSON := urlFlag, apiKeyFlag, jsonFlag
	urlFlag, apiKeyFlag, jsonFlag = srv.URL, "test-key", true
	defer func() { urlFlag, apiKeyFlag, jsonFlag = origURL, origKey, origJSON }()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := overviewCmd.RunE(overviewCmd, nil)

	w.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(r)

	if runErr != nil {
		t.Fatalf("RunE: %v", runErr)
	}

	health := gjson.GetBytes(out, "database_health")
	if !health.IsArray() {
		t.Fatalf("expected database_health array, got: %s", out)
	}
	byID := map[int64]gjson.Result{}
	health.ForEach(func(_, v gjson.Result) bool {
		byID[v.Get("id").Int()] = v
		return true
	})

	prod, ok := byID[2]
	if !ok {
		t.Fatalf("expected an entry for database 2, got: %s", out)
	}
	if !prod.Get("ok").Bool() {
		t.Errorf("expected database 2 to be healthy: %s", prod.Raw)
	}
	if prod.Get("kind").Exists() {
		t.Errorf("a healthy database must not carry a kind: %s", prod.Raw)
	}

	ch, ok := byID[35]
	if !ok {
		t.Fatalf("expected an entry for database 35, got: %s", out)
	}
	if ch.Get("ok").Bool() {
		t.Errorf("expected database 35 to be unhealthy: %s", ch.Raw)
	}
	if ch.Get("kind").String() != "database_unreachable" {
		t.Errorf("expected kind=database_unreachable, got: %s", ch.Raw)
	}
	if ch.Get("error").String() == "" {
		t.Errorf("expected a non-empty error message, got: %s", ch.Raw)
	}
}

// TestProbeDBHealth_InBodyFailure pins the false-OK fix: Metabase reports an
// unreachable database as HTTP 202 carrying status=failed / error, which the
// probe must treat as unhealthy.
func TestProbeDBHealth_InBodyFailure(t *testing.T) {
	isolateConfigEnv(t)
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"error field", `{"status":"failed","error":"Failed to create connection","data":{"rows":[],"cols":[]}}`, true},
		{"status only", `{"status":"failed","data":{"rows":[],"cols":[]}}`, true},
		{"healthy", `{"status":"completed","row_count":1,"data":{"cols":[{"name":"1"}],"rows":[[1]]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := client.New(srv.URL, "k", "", false, 5*time.Second)
			got := probeDBHealth(c, 166)
			if (got != nil) != tc.wantErr {
				t.Fatalf("probeDBHealth err=%v, wantErr=%v", got, tc.wantErr)
			}
		})
	}
}

package commands

// W8 — per-database liveness probing, shared by `overview` and
// `cards execute`'s failure-path hint. Before this, a dead database was
// indistinguishable from "no data" or "bad SQL" (a database could stay down
// for days and nothing in mbx's output could tell the two apart).

import (
	"context"
	"fmt"
	"time"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
)

// dbHealthProbeTimeout bounds each per-DB liveness probe — short enough
// that one dead database doesn't stall `overview` or a `cards execute`
// failure path waiting on the client's full retry/backoff cycle.
const dbHealthProbeTimeout = 3 * time.Second

// probeDBHealth runs a cheap SELECT 1 native query against a database to
// check it's actually reachable, independent of whatever the caller's real
// query was trying to do.
func probeDBHealth(c *client.Client, dbID int) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbHealthProbeTimeout)
	defer cancel()
	body := map[string]any{
		"database": dbID,
		"type":     "native",
		"native":   map[string]any{"query": "SELECT 1"},
	}
	resp, err := c.PostQuery(ctx, "/api/dataset", body)
	if err != nil {
		return err
	}
	return datasetBodyError(resp)
}

// datasetBodyError reports a failure carried inside an HTTP-2xx /api/dataset
// response. Metabase answers a query whose database connection cannot be
// opened with 202 and {"status":"failed","error":"Failed to create
// connection"...}; ignoring the body made dead databases look healthy.
func datasetBodyError(resp []byte) error {
	if msg := gjson.GetBytes(resp, "error").String(); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	if gjson.GetBytes(resp, "status").String() == "failed" {
		return fmt.Errorf("query status failed")
	}
	return nil
}

// dbHealthResult is one line of `overview`'s per-database health section.
type dbHealthResult struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	OK     bool   `json:"ok"`
	Kind   string `json:"kind,omitempty"`
	Error  string `json:"error,omitempty"`
}

// probeOneDB runs probeDBHealth for one database and shapes the result for
// `overview`'s JSON output. "database_unreachable" is a domain-specific
// liveness classification, deliberately not the underlying APIError.Kind()
// (network/timeout/server_error/…) — callers care whether the DB answers a
// query at all, not which transport reason it failed for.
func probeOneDB(c *client.Client, d dbEntry) dbHealthResult {
	res := dbHealthResult{ID: d.ID, Name: d.Name, Engine: d.Engine}
	if err := probeDBHealth(c, d.ID); err != nil {
		res.Kind = "database_unreachable"
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// unreachableDBHintForCard is `cards execute`'s failure-only follow-up: when
// a card's query fails, fetch its AUTHORITATIVE database_id (fresh — the id
// resolveCardID returns from the alias cache can be 0 or stale) and probe
// that database. If the database itself is unreachable, return a hint
// distinguishing "the SQL/card is broken" from "the whole database is
// down". Best-effort: any failure to even
// determine the hint returns "", never a second error.
func unreachableDBHintForCard(c *client.Client, cardID int) string {
	cardResp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", cardID), nil)
	if err != nil {
		return ""
	}
	dbID := int(gjson.GetBytes(cardResp, "database_id").Int())
	if dbID <= 0 {
		return ""
	}
	if probeErr := probeDBHealth(c, dbID); probeErr != nil {
		return fmt.Sprintf("card's database_id=%d unreachable — MySQL KPIs still available via ordini-kpi", dbID)
	}
	return ""
}

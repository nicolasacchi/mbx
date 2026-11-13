package commands

import (
	"strings"
	"testing"
)

// TestBuildOrdiniKPISQL_WindowSemantics pins W4's central correctness fix:
// the gmv subquery must window on customer_confirm_at (placed orders) while
// the outer aggregate (order_count/unique_buyers/cancelled) stays on
// created_at — a shared created_at window over-counts GMV by including
// unconfirmed carts that were never placed.
func TestBuildOrdiniKPISQL_WindowSemantics(t *testing.T) {
	sql := buildOrdiniKPISQL("24 HOUR")

	// The gmv subquery is embedded mid-SELECT-list, and the outer WHERE
	// (created_at) trails it at the very end of the query — so isolate the
	// subquery text and diff it out, rather than assuming a linear split.
	// The column alignment in buildOrdiniKPISQL pads arbitrary whitespace
	// before "AS gmv", so search for the keyword rather than ") AS gmv".
	start := strings.Index(sql, "(SELECT COALESCE")
	asGMV := strings.Index(sql, "AS gmv")
	if start < 0 || asGMV < 0 || asGMV < start {
		t.Fatalf("expected a `(SELECT COALESCE ...) AS gmv` subquery in the SQL: %s", sql)
	}
	end := asGMV + len("AS gmv")
	gmvSubquery := sql[start:end]
	mainQuery := sql[:start] + sql[end:]

	if !strings.Contains(mainQuery, "created_at >= NOW() - INTERVAL 24 HOUR") {
		t.Errorf("main KPI aggregate must window on created_at: %s", mainQuery)
	}
	if strings.Contains(mainQuery, "customer_confirm_at") {
		t.Errorf("main KPI aggregate must NOT reference customer_confirm_at: %s", mainQuery)
	}
	if !strings.Contains(gmvSubquery, "customer_confirm_at >= NOW() - INTERVAL 24 HOUR") {
		t.Errorf("gmv subquery must window on customer_confirm_at: %s", gmvSubquery)
	}
	if !strings.Contains(gmvSubquery, "op.prezzo * op.quantita") {
		t.Errorf("gmv subquery must sum prezzo*quantita: %s", gmvSubquery)
	}
	if !strings.Contains(gmvSubquery, "ordine_prodotti") {
		t.Errorf("gmv subquery must join ordine_prodotti: %s", gmvSubquery)
	}
	// Cancelled orders stay in GMV (placement-time semantics, matches the
	// old GMV card) — the subquery must not filter on `state`.
	if strings.Contains(gmvSubquery, "state") {
		t.Errorf("gmv subquery must not exclude cancelled orders: %s", gmvSubquery)
	}
}

// TestDecodeOrdiniKPIRow pins the 6-cell decode (order_count, unique_buyers,
// cancelled, latest, as_of, gmv) — the arity bump from the pre-W4 5-cell shape.
func TestDecodeOrdiniKPIRow(t *testing.T) {
	resp := []byte(`{"data":{"rows":[[100,80,5,"2026-07-20T10:00:00Z","2026-07-20T10:05:00Z",1234.56]]}}`)
	out, err := decodeOrdiniKPIRow(resp, "1d")
	if err != nil {
		t.Fatalf("decodeOrdiniKPIRow: %v", err)
	}
	if out["order_count"] != int64(100) {
		t.Errorf("order_count = %v, want 100", out["order_count"])
	}
	if out["unique_buyers"] != int64(80) {
		t.Errorf("unique_buyers = %v, want 80", out["unique_buyers"])
	}
	if out["cancelled"] != int64(5) {
		t.Errorf("cancelled = %v, want 5", out["cancelled"])
	}
	if out["gmv"] != 1234.56 {
		t.Errorf("gmv = %v, want 1234.56", out["gmv"])
	}
	if out["window"] != "1d" {
		t.Errorf("window = %v, want 1d", out["window"])
	}
}

// TestDecodeOrdiniKPIRow_MissingGMVErrors pins the arity guard: a pre-W4
// 5-cell row (no gmv) must error rather than silently omit the field.
func TestDecodeOrdiniKPIRow_MissingGMVErrors(t *testing.T) {
	resp := []byte(`{"data":{"rows":[[1,2,3,"x","y"]]}}`) // only 5 cells
	if _, err := decodeOrdiniKPIRow(resp, "1d"); err == nil {
		t.Fatal("expected an error for a 5-cell row (missing gmv)")
	}
}

// TestDecodeOrdiniKPIRow_ZeroGMV pins the "empty window -> 0, not null"
// acceptance criterion from the wave plan.
func TestDecodeOrdiniKPIRow_ZeroGMV(t *testing.T) {
	resp := []byte(`{"data":{"rows":[[0,0,0,null,"2026-07-20T10:05:00Z",0]]}}`)
	out, err := decodeOrdiniKPIRow(resp, "1s")
	if err != nil {
		t.Fatalf("decodeOrdiniKPIRow: %v", err)
	}
	if out["gmv"] != float64(0) {
		t.Errorf("gmv = %v, want 0", out["gmv"])
	}
}

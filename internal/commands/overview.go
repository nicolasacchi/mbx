package commands

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/nicolasacchi/mbx/internal/aliases"
	"github.com/nicolasacchi/mbx/internal/output"
)

var overviewDeepFlag bool

var overviewCmd = &cobra.Command{
	Use:   "overview",
	Short: "Parallel health snapshot — DBs, named cards, recent activity",
	Long: `Fetches in parallel:
  - List of databases
  - Metadata for each named alias (cheap — uses /api/card/:id, not execute)
  - Current Metabase user (auth check)

With --deep, also round-trips each named card via /api/card/:id/query — this
takes 5-30s when funnel/CR cards are involved.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, creds, err := getClient()
		if err != nil {
			return err
		}

		start := time.Now()
		// Bound the goroutine pool — if aliases.toml ever holds 100 entries,
		// fanning out 100 concurrent HTTPS calls would trigger Metabase rate
		// limits. errgroup.SetLimit(8) is enough to saturate HTTP/2
		// multiplexing on a single TLS connection.
		g := new(errgroup.Group)
		g.SetLimit(8)
		var mu sync.Mutex
		out := map[string]any{
			"url":        creds.URL,
			"default_db": creds.DefaultDB,
		}
		errs := []string{}
		recordErr := func(label string, err error) {
			mu.Lock()
			defer mu.Unlock()
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}

		// 1. Databases + per-DB health probes (W8). Probing needs the id
		// list first, so this stays one dependent chain inside a single
		// g.Go — the auth check and named-card metadata below still run
		// fully in parallel with it via the outer errgroup.
		g.Go(func() error {
			resp, err := c.Get(ctxBg(), "/api/database", nil)
			if err != nil {
				recordErr("databases", err)
				return nil
			}
			data := resp
			if d := gjson.GetBytes(resp, "data"); d.IsArray() {
				data = []byte(d.Raw)
			}
			var entries []dbEntry
			summary := []map[string]any{}
			gjson.ParseBytes(data).ForEach(func(_, v gjson.Result) bool {
				d := dbEntry{
					ID:     int(v.Get("id").Int()),
					Name:   v.Get("name").String(),
					Engine: v.Get("engine").String(),
				}
				entries = append(entries, d)
				summary = append(summary, map[string]any{
					"id":     d.ID,
					"name":   d.Name,
					"engine": d.Engine,
				})
				return true
			})
			mu.Lock()
			out["databases"] = summary
			mu.Unlock()

			// W8: bounded-parallel SELECT 1 liveness probe per database —
			// without it, "this database is down" and "no data" look the
			// same, possibly for days.
			health := make([]dbHealthResult, len(entries))
			hg := new(errgroup.Group)
			hg.SetLimit(4)
			for i, d := range entries {
				i, d := i, d
				hg.Go(func() error {
					health[i] = probeOneDB(c, d)
					return nil
				})
			}
			_ = hg.Wait()
			mu.Lock()
			out["database_health"] = health
			mu.Unlock()
			return nil
		})

		// 2. Auth check
		g.Go(func() error {
			resp, err := c.Get(ctxBg(), "/api/user/current", nil)
			if err != nil {
				recordErr("auth", err)
				return nil
			}
			user := map[string]any{
				"id":           gjson.GetBytes(resp, "id").Int(),
				"email":        gjson.GetBytes(resp, "email").String(),
				"common_name":  gjson.GetBytes(resp, "common_name").String(),
				"is_superuser": gjson.GetBytes(resp, "is_superuser").Bool(),
			}
			mu.Lock()
			out["user"] = user
			mu.Unlock()
			return nil
		})

		// 3. Named-card metadata
		store, _ := aliases.Load()
		var cardSummaries []map[string]any
		if store != nil && len(store.Aliases) > 0 {
			cardSummaries = make([]map[string]any, 0, len(store.Aliases))
			var cardsMu sync.Mutex
			for _, a := range store.All() {
				a := a
				g.Go(func() error {
					resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", a.CardID), nil)
					if err != nil {
						recordErr(fmt.Sprintf("card %d (%s)", a.CardID, a.Alias), err)
						return nil
					}
					summary := map[string]any{
						"alias":            a.Alias,
						"card_id":          a.CardID,
						"name":             gjson.GetBytes(resp, "name").String(),
						"database_id":      gjson.GetBytes(resp, "database_id").Int(),
						"archived":         gjson.GetBytes(resp, "archived").Bool(),
						"last_query_start": gjson.GetBytes(resp, "last_query_start").String(),
						"average_query_ms": gjson.GetBytes(resp, "average_query_time").Float(),
					}
					if overviewDeepFlag {
						qResp, qErr := c.Post(ctxBg(), fmt.Sprintf("/api/card/%d/query", a.CardID), map[string]any{})
						if qErr != nil {
							summary["execute_error"] = qErr.Error()
						} else {
							summary["execute_row_count"] = gjson.GetBytes(qResp, "row_count").Int()
							summary["execute_running_ms"] = gjson.GetBytes(qResp, "running_time").Int()
						}
					}
					cardsMu.Lock()
					cardSummaries = append(cardSummaries, summary)
					cardsMu.Unlock()
					return nil
				})
			}
		}

		_ = g.Wait()
		if cardSummaries != nil {
			out["named_cards"] = cardSummaries
		}
		out["elapsed_ms"] = time.Since(start).Milliseconds()
		if len(errs) > 0 {
			out["errors"] = errs
		}

		marshaled, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return output.PrintData("overview", marshaled, jsonFlag, jqFlag)
	},
}

func init() {
	overviewCmd.Flags().BoolVar(&overviewDeepFlag, "deep", false, "Also execute each named card (5-30s for funnel/CR cards)")
}

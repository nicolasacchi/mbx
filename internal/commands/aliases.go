package commands

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/nicolasacchi/mbx/internal/aliases"
	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/output"
)

// Seed names — saved questions the original deployment referenced most
// often. `mbx aliases sync` resolves them against the live API by name and
// records ID + database_id.
// Seeds are NOT card IDs — they're search queries used during sync. The
// resulting aliases.toml is user-editable.
//
// "gmv-by-product" was removed from the seed list (W4): the card is broken
// in Metabase — it queries the now-dropped google_analytics_measures table
// and hard-fails live. Use `mbx ordini-kpi`'s gmv field
// instead. A pre-existing local aliases.toml may still carry a stale
// gmv-by-product entry from an earlier sync; re-running `mbx aliases sync`
// no longer re-seeds it, but doesn't remove it either — hand-edit
// aliases.toml to drop it.
var aliasSeeds = []aliasSeed{
	{Alias: "orders-by-product", Search: "Order Distributions per ProductType"},
	{Alias: "minsan-by-product", Search: "Minsan Distributions per ProductType"},
	{Alias: "funnel", Search: "Website Funnel Trend"},
	{Alias: "cr-desktop", Search: "CR Desktop"},
}

type aliasSeed struct {
	Alias  string
	Search string
}

var aliasesCmd = &cobra.Command{
	Use:   "aliases",
	Short: "Manage card-name → ID aliases (~/.config/mbx/aliases.toml)",
}

var aliasesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured aliases",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := aliases.Load()
		if err != nil {
			return err
		}
		// Convert to JSON for output.PrintData
		all := store.All()
		raw, err := json.Marshal(all)
		if err != nil {
			return err
		}
		return output.PrintData("aliases.list", raw, jsonFlag, jqFlag)
	},
}

var (
	aliasesSetCardID     int
	aliasesSetDatabaseID int
	aliasesSetName       string
)

// aliasesSetCmd registers alias -> card_id directly, without going through
// aliases sync's search-based resolution — no network call, local file
// write only. Exists mainly for the `cards create` recovery path (v0.4):
// when the card POST succeeds but the --alias registration step fails, the
// card already exists server-side, so `cards create` can't be safely
// retried (that would create a SECOND card); this command lets the operator
// register the alias for the already-created card by hand.
var aliasesSetCmd = &cobra.Command{
	Use:   "set <alias> --card-id <id>",
	Short: "Manually register a local alias for an existing card id",
	Long: `Writes alias -> card_id (+ optional --database-id/--name) directly
to ~/.config/mbx/aliases.toml, bypassing 'aliases sync's search-based
resolution. Useful when you already know the card id — most commonly to
recover from 'mbx cards create --alias' succeeding at the card POST but
failing to save the alias: the card already exists in Metabase at that
point, so re-running 'cards create' would create a duplicate; this command
registers the alias for the card that's already there.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if aliasesSetCardID <= 0 {
			return fmt.Errorf("--card-id is required and must be a positive integer")
		}
		store, err := aliases.Load()
		if err != nil {
			return err
		}
		store.Set(aliases.Alias{
			Alias:      args[0],
			CardID:     aliasesSetCardID,
			DatabaseID: aliasesSetDatabaseID,
			Name:       aliasesSetName,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		})
		if err := aliases.Save(store); err != nil {
			return err
		}
		path, _ := aliases.Path()
		fmt.Fprintf(os.Stderr, "wrote %s — alias %q -> card %d\n", path, args[0], aliasesSetCardID)
		return nil
	},
}

var aliasesSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Refresh aliases from live Metabase /api/search",
	Long: `Walks the seed list (orders-by-product, minsan-by-product, funnel,
cr-desktop) and resolves each to the current best-match card via the
Metabase /api/search?models=card endpoint. Writes the result to
~/.config/mbx/aliases.toml.

Aliases not found in search are skipped with a stderr warning. Existing aliases
not in the seed list are preserved (you can hand-edit aliases.toml to add more).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		store, err := aliases.Load()
		if err != nil {
			return err
		}

		now := time.Now().UTC().Format(time.RFC3339)

		// Parallelise the per-seed search + /api/card/:id lookup. Each seed
		// makes 2 sequential HTTP calls; with errgroup.SetLimit(5) all five
		// seeds resolve in ~2 RTT instead of ~10 RTT (B2 — saves 750 ms-4 s).
		g := new(errgroup.Group)
		g.SetLimit(5)
		var mu sync.Mutex
		var updated, skipped int
		for _, seed := range aliasSeeds {
			seed := seed
			g.Go(func() error {
				a, ok := resolveSeed(c, seed, now)
				mu.Lock()
				defer mu.Unlock()
				if !ok {
					skipped++
					return nil
				}
				store.Set(a)
				updated++
				fmt.Fprintf(os.Stderr, "aliases sync: %-22s → card %d (DB %d) %q\n",
					a.Alias, a.CardID, a.DatabaseID, a.Name)
				return nil
			})
		}
		_ = g.Wait()

		store.UpdatedAt = now
		if err := aliases.Save(store); err != nil {
			return err
		}
		path, _ := aliases.Path()
		fmt.Fprintf(os.Stderr, "wrote %s — %d updated, %d skipped\n", path, updated, skipped)
		return nil
	},
}

// resolveSeed handles one alias seed: search Metabase for the card, then GET
// /api/card/:id for authoritative database_id/name. Returns (Alias, false) on
// any failure with a stderr warning. Safe to call concurrently.
func resolveSeed(c *client.Client, seed aliasSeed, now string) (aliases.Alias, bool) {
	q := url.Values{}
	q.Set("q", seed.Search)
	q.Set("models", "card")
	resp, err := c.Get(ctxBg(), "/api/search", q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aliases sync: %s — search failed: %v\n", seed.Alias, err)
		return aliases.Alias{}, false
	}
	items := gjson.GetBytes(resp, "data")
	if !items.IsArray() || len(items.Array()) == 0 {
		fmt.Fprintf(os.Stderr, "aliases sync: %s — no match for %q\n", seed.Alias, seed.Search)
		return aliases.Alias{}, false
	}
	best := items.Array()[0]
	cardID := int(best.Get("id").Int())
	name := best.Get("name").String()
	dbID := int(best.Get("database_id").Int())
	// Search sometimes returns a stale database_id; /api/card/:id is authoritative.
	if cardResp, cerr := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", cardID), nil); cerr == nil {
		if v := gjson.GetBytes(cardResp, "database_id").Int(); v > 0 {
			dbID = int(v)
		}
		if n := gjson.GetBytes(cardResp, "name").String(); n != "" {
			name = n
		}
	}
	return aliases.Alias{
		Alias:      seed.Alias,
		CardID:     cardID,
		DatabaseID: dbID,
		Name:       name,
		UpdatedAt:  now,
	}, true
}

func init() {
	aliasesSetCmd.Flags().IntVar(&aliasesSetCardID, "card-id", 0, "Card id (required)")
	aliasesSetCmd.Flags().IntVar(&aliasesSetDatabaseID, "database-id", 0, "Optional database id")
	aliasesSetCmd.Flags().StringVar(&aliasesSetName, "name", "", "Optional display name")

	aliasesCmd.AddCommand(aliasesListCmd, aliasesSyncCmd, aliasesSetCmd)
}

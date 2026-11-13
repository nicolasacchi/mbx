package commands

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/aliases"
	"github.com/nicolasacchi/mbx/internal/client"
	"github.com/nicolasacchi/mbx/internal/output"
)

var (
	cardsExportFormat  string
	cardsExecuteParams []string
	cardsExportParams  []string

	cardsCreateName        string
	cardsCreateSQL         string
	cardsCreateDisplay     string
	cardsCreateCollection  int
	cardsCreateDescription string
	cardsCreateAlias       string
)

var cardsCmd = &cobra.Command{
	Use:   "cards",
	Short: "List, inspect, and execute saved questions (cards)",
}

var cardsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all saved questions",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		// /api/card returns a bare array
		resp, err := c.Get(ctxBg(), "/api/card", nil)
		if err != nil {
			return err
		}
		return output.PrintData("cards.list", resp, jsonFlag, jqFlag)
	},
}

var cardsGetCmd = &cobra.Command{
	Use:   "get <id|alias>",
	Short: "Get card metadata by ID (or alias from `mbx aliases list`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("cards.get", resp, jsonFlag, jqFlag)
	},
}

// cardsMetadataCmd (W13) fetches a card's query_metadata — field, database,
// and table metadata in one round trip, useful for "explain this card"
// flows that would otherwise need up to three separate calls. Falls back to
// plain GET /api/card/:id on failure, same shape as tables.go:57-64's
// query_metadata-then-plain fallback (query_metadata was added in Metabase
// v0.51 — the fallback keeps this command working against an older or
// misconfigured instance too).
var cardsMetadataCmd = &cobra.Command{
	Use:   "metadata <id|alias>",
	Short: "Get a card's query metadata (fields/database/table in one call, with plain-card fallback)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d/query_metadata", id), nil)
		if err != nil {
			// Fall back to plain /api/card/:id.
			resp, err = c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", id), nil)
			if err != nil {
				return err
			}
		}
		return output.PrintData("cards.metadata", resp, jsonFlag, jqFlag)
	},
}

var cardsExecuteCmd = &cobra.Command{
	Use:   "execute <id|alias>",
	Short: "Run a saved question and return results",
	Long: `Execute a saved question. Without --param flags, uses the card's default
parameter values (matches the MCP execute_card behaviour).

To override template-tag values:

  mbx cards execute funnel --param from_date=2026-04-01 --param to_date=2026-05-01

Parameter slugs are resolved against the card's metadata. Use 'mbx cards get <id>'
to inspect available parameters (under .parameters[]).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		body, err := buildCardQueryBody(c, id, cardsExecuteParams)
		if err != nil {
			return err
		}
		resp, err := c.Post(ctxBg(), fmt.Sprintf("/api/card/%d/query", id), body)
		if err != nil {
			// W8: on failure, probe the card's own database — a dead
			// database and a broken card/SQL are otherwise indistinguishable.
			// Only runs on the failure path — no extra round-trip
			// when the query succeeds.
			if hint := unreachableDBHintForCard(c, id); hint != "" {
				return fmt.Errorf("%w (%s)", err, hint)
			}
			return err
		}
		if errStr := gjson.GetBytes(resp, "error").String(); errStr != "" {
			return fmt.Errorf("query error: %s", errStr)
		}
		// Echo the card's AUTHORITATIVE database_id (from the executed
		// response, not any cached alias metadata) the same way resolveDB
		// does for query/schema/tables/playground/ordini-kpi (W3) — a card
		// silently pointed at the wrong DB is otherwise indistinguishable
		// from "no data".
		if dbID := gjson.GetBytes(resp, "database_id").Int(); dbID > 0 {
			echoResolvedDB(int(dbID), lookupDBName(c, int(dbID)))
		}
		return output.PrintData("cards.execute", simplifyQueryResponse(resp, 0), jsonFlag, jqFlag)
	},
}

var cardsExportCmd = &cobra.Command{
	Use:   "export <id|alias>",
	Short: "Export card results in a chosen format (csv, json, xlsx)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		format := cardsExportFormat
		if format == "" {
			format = "csv"
		}
		switch format {
		case "csv", "json", "xlsx":
		default:
			return fmt.Errorf("invalid --format %q (must be csv, json, or xlsx)", format)
		}
		body, err := buildCardQueryBody(c, id, cardsExportParams)
		if err != nil {
			return err
		}
		// Stream the export — large CSV/XLSX exports must not be buffered
		// in memory (was the v0.2 bug). streamOrEnvelope pipes Metabase's
		// response body directly to stdout, EXCEPT when the query itself
		// failed: Metabase's export endpoint responds 200 OK with a JSON
		// error body (not real export data) in that case — the exact bug
		// query --csv had before it was fixed to route through the A3
		// envelope instead of silently streaming the JSON as if it were
		// export data (exit 0). db/sql are unknown here (no resolveDB
		// round trip for a saved-question export) — the envelope's db/table
		// fields are best-effort and simply stay empty.
		rc, err := c.PostStream(ctxBg(), fmt.Sprintf("/api/card/%d/query/%s", id, format), body)
		if err != nil {
			emitErrorEnvelope(err, 0, "")
			return err
		}
		return streamOrEnvelope(rc, 0, "")
	},
}

var cardsRevisionsCmd = &cobra.Command{
	Use:   "revisions <id|alias>",
	Short: "Get revision history of a card",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		q := url.Values{}
		q.Set("entity", "card")
		q.Set("id", fmt.Sprintf("%d", id))
		resp, err := c.Get(ctxBg(), "/api/revision", q)
		if err != nil {
			return err
		}
		return output.PrintData("cards.revisions", resp, jsonFlag, jqFlag)
	},
}

var cardsDashboardsCmd = &cobra.Command{
	Use:   "dashboards <id|alias>",
	Short: "List dashboards containing this card",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		id, _, err := resolveCardID(args[0])
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d/dashboards", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("cards.dashboards", resp, jsonFlag, jqFlag)
	},
}

// cardsCreateCmd creates a new saved question (card) from SQL (W10a).
// --database resolves through the same resolveDB plumbing as query/schema
// (aliases work, the resolved DB echoes to stderr) since --database is
// already a persistent root flag. --dry-run prints the would-be POST body
// without sending it.
var cardsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new saved question (card) from SQL (requires --yes)",
	Long: `Creates a new saved question from native SQL.

Requires --yes (or --confirm / MBX_CONFIRM=1). Uses MBX_WRITE_API_KEY if set,
otherwise falls back to METABASE_API_KEY. With --alias, registers the new
card in the local aliases store (~/.config/mbx/aliases.toml) the same way
'mbx aliases sync' does.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(cardsCreateName) == "" {
			return fmt.Errorf("--name is required")
		}
		if strings.TrimSpace(cardsCreateSQL) == "" {
			return fmt.Errorf("--sql is required")
		}
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		dbID, err := resolveDB(c, creds)
		if err != nil {
			return err
		}
		display := cardsCreateDisplay
		if display == "" {
			display = "table"
		}
		body := map[string]any{
			"name":                   cardsCreateName,
			"display":                display,
			"visualization_settings": map[string]any{},
			"dataset_query": map[string]any{
				"database": dbID,
				"type":     "native",
				"native": map[string]any{
					"query": cardsCreateSQL,
				},
			},
		}
		if cardsCreateCollection > 0 {
			body["collection_id"] = cardsCreateCollection
		}
		if cardsCreateDescription != "" {
			body["description"] = cardsCreateDescription
		}
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal cards create body: %w", err)
		}
		action := fmt.Sprintf("POST /api/card with body %s", bodyJSON)
		if handled, err := gate(cmd, action); handled {
			return err
		}
		resp, err := c.PostWrite(ctxBg(), "/api/card", body)
		if err != nil {
			return err
		}
		id := gjson.GetBytes(resp, "id").Int()
		if id == 0 {
			return fmt.Errorf("card create: response missing id: %s", resp)
		}
		name := gjson.GetBytes(resp, "name").String()
		if name == "" {
			name = cardsCreateName
		}
		out := map[string]any{
			"id":          id,
			"name":        name,
			"database_id": dbID,
		}
		outJSON, err := json.Marshal(out)
		if err != nil {
			return err
		}

		if cardsCreateAlias != "" {
			aliasErr := func() error {
				store, err := aliases.Load()
				if err != nil {
					return err
				}
				store.Set(aliases.Alias{
					Alias:      cardsCreateAlias,
					CardID:     int(id),
					DatabaseID: dbID,
					Name:       name,
					UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
				})
				return aliases.Save(store)
			}()
			if aliasErr != nil {
				// The card already exists server-side by this point — print
				// its id/name via the normal output path FIRST (an agent
				// loop reading only the error would otherwise have no way
				// to learn the id it just paid a write for), then report
				// the alias failure with an explicit no-retry instruction:
				// re-running `cards create` here would create a SECOND
				// card, not fix the alias.
				_ = output.PrintData("cards.create", outJSON, jsonFlag, jqFlag)
				return fmt.Errorf("card exists (id=%d) — do NOT retry create; run 'mbx aliases set %s --card-id %d --database-id %d' manually: %w",
					id, cardsCreateAlias, id, dbID, aliasErr)
			}
		}
		return output.PrintData("cards.create", outJSON, jsonFlag, jqFlag)
	},
}

func init() {
	cardsExportCmd.Flags().StringVar(&cardsExportFormat, "format", "csv", "Export format: csv, json, xlsx")
	cardsExecuteCmd.Flags().StringArrayVar(&cardsExecuteParams, "param", nil, "Override a card parameter (repeatable): --param slug=value")
	cardsExportCmd.Flags().StringArrayVar(&cardsExportParams, "param", nil, "Override a card parameter (repeatable): --param slug=value")

	cardsCreateCmd.Flags().StringVar(&cardsCreateName, "name", "", "Card name (required)")
	cardsCreateCmd.Flags().StringVar(&cardsCreateSQL, "sql", "", "Native SQL query (required)")
	cardsCreateCmd.Flags().StringVar(&cardsCreateDisplay, "display", "table", "Visualization display type")
	cardsCreateCmd.Flags().IntVar(&cardsCreateCollection, "collection", 0, "Optional parent collection ID")
	cardsCreateCmd.Flags().StringVar(&cardsCreateDescription, "description", "", "Optional description")
	cardsCreateCmd.Flags().StringVar(&cardsCreateAlias, "alias", "", "Optional local alias to register for the new card")

	cardsCmd.AddCommand(cardsListCmd, cardsGetCmd, cardsExecuteCmd, cardsExportCmd, cardsDashboardsCmd, cardsRevisionsCmd, cardsCreateCmd, cardsMetadataCmd)
}

// resolveCardID accepts a numeric ID or an alias name and returns (id, dbID).
// dbID is 0 if unknown.
func resolveCardID(arg string) (int, int, error) {
	if id, ok := aliases.ParseInt(arg); ok {
		return id, 0, nil
	}
	store, err := aliases.Load()
	if err != nil {
		return 0, 0, fmt.Errorf("could not resolve alias %q: %w", arg, err)
	}
	a, ok := store.Find(arg)
	if !ok {
		return 0, 0, fmt.Errorf("alias %q not found — run 'mbx aliases sync' or 'mbx aliases list'", arg)
	}
	return a.CardID, a.DatabaseID, nil
}

type cardParam struct {
	ID     string
	Slug   string
	Name   string
	Type   string
	Target json.RawMessage
}

// buildCardQueryBody returns the JSON body for /api/card/:id/query[/format].
// Without --param flags it returns {} (matches MCP behaviour — server uses defaults).
// With --param key=value, it fetches the card's parameters[] and resolves each key
// against `slug` then `name`.
func buildCardQueryBody(c *client.Client, cardID int, paramFlags []string) (map[string]any, error) {
	if len(paramFlags) == 0 {
		return map[string]any{}, nil
	}

	cardResp, err := c.Get(ctxBg(), fmt.Sprintf("/api/card/%d", cardID), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch card metadata: %w", err)
	}

	available := make([]cardParam, 0)
	for _, p := range gjson.GetBytes(cardResp, "parameters").Array() {
		available = append(available, cardParam{
			ID:     p.Get("id").String(),
			Slug:   p.Get("slug").String(),
			Name:   p.Get("name").String(),
			Type:   p.Get("type").String(),
			Target: json.RawMessage(p.Get("target").Raw),
		})
	}

	out := make([]map[string]any, 0, len(paramFlags))
	for _, kv := range paramFlags {
		key, value, ok := splitKV(kv)
		if !ok {
			return nil, fmt.Errorf("invalid --param %q (expected slug=value)", kv)
		}
		match, ok := findParam(available, key)
		if !ok {
			slugs := make([]string, 0, len(available))
			for _, p := range available {
				if p.Slug != "" {
					slugs = append(slugs, p.Slug)
				}
			}
			return nil, fmt.Errorf("unknown parameter %q on card %d (available: %v)", key, cardID, slugs)
		}
		entry := map[string]any{
			"id":    match.ID,
			"type":  match.Type,
			"value": value,
		}
		if len(match.Target) > 0 && string(match.Target) != "null" {
			var t any
			if err := json.Unmarshal(match.Target, &t); err == nil {
				entry["target"] = t
			}
		}
		out = append(out, entry)
	}
	return map[string]any{"parameters": out}, nil
}

func splitKV(s string) (key, value string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func findParam(params []cardParam, key string) (cardParam, bool) {
	for _, p := range params {
		if p.Slug == key {
			return p, true
		}
	}
	for _, p := range params {
		if p.Name == key {
			return p, true
		}
	}
	return cardParam{}, false
}

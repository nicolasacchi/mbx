package commands

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var (
	dashboardCreateName        string
	dashboardCreateDescription string
	dashboardCreateCollection  int
)

var dashboardsCmd = &cobra.Command{
	Use:   "dashboards",
	Short: "List, inspect, and create dashboards",
}

var dashboardsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all dashboards",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), "/api/dashboard", nil)
		if err != nil {
			return err
		}
		// Some Metabase versions wrap in {"data":[...]} — unwrap if so.
		if data := gjson.GetBytes(resp, "data"); data.IsArray() {
			return output.PrintData("dashboards.list", []byte(data.Raw), jsonFlag, jqFlag)
		}
		return output.PrintData("dashboards.list", resp, jsonFlag, jqFlag)
	},
}

var dashboardsGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get dashboard details by ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := validateID(args[0])
		if err != nil {
			return err
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/dashboard/%d", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.get", resp, jsonFlag, jqFlag)
	},
}

var dashboardsCardsCmd = &cobra.Command{
	Use:   "cards <id>",
	Short: "List cards on a dashboard",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := validateID(args[0])
		if err != nil {
			return err
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		// Dashboard cards live on /api/dashboard/:id under .dashcards (or .ordered_cards on older).
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/dashboard/%d", id), nil)
		if err != nil {
			return err
		}
		for _, key := range []string{"dashcards", "ordered_cards", "tabs"} {
			if v := gjson.GetBytes(resp, key); v.IsArray() {
				return output.PrintData("dashboards.cards", []byte(v.Raw), jsonFlag, jqFlag)
			}
		}
		return output.PrintData("dashboards.cards", resp, jsonFlag, jqFlag)
	},
}

var dashboardsRelatedCmd = &cobra.Command{
	Use:   "related <id>",
	Short: "Get related dashboards for a dashboard",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := validateID(args[0])
		if err != nil {
			return err
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/dashboard/%d/related", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.related", resp, jsonFlag, jqFlag)
	},
}

var dashboardsRevisionsCmd = &cobra.Command{
	Use:   "revisions <id>",
	Short: "Get revision history of a dashboard",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := validateID(args[0])
		if err != nil {
			return err
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/dashboard/%d/revisions", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.revisions", resp, jsonFlag, jqFlag)
	},
}

var dashboardsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new dashboard (requires --yes)",
	Long: `Creates an empty dashboard with the given name. Requires --yes
(or --confirm / MBX_CONFIRM=1) to guard against accidental writes from
agentic loops. --dry-run prints the would-be POST body without sending it.

Uses MBX_WRITE_API_KEY if set, otherwise falls back to METABASE_API_KEY.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(dashboardCreateName) == "" {
			return fmt.Errorf("--name is required")
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		body := map[string]any{
			"name":        dashboardCreateName,
			"description": dashboardCreateDescription,
		}
		if dashboardCreateCollection > 0 {
			body["collection_id"] = dashboardCreateCollection
		}
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal dashboards create body: %w", err)
		}
		action := fmt.Sprintf("POST /api/dashboard with body %s", bodyJSON)
		if handled, err := gate(cmd, action); handled {
			return err
		}
		resp, err := c.PostWrite(ctxBg(), "/api/dashboard", body)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.create", resp, jsonFlag, jqFlag)
	},
}

var (
	addCardRowFlag   int
	addCardColFlag   int
	addCardSizeXFlag int
	addCardSizeYFlag int
	addCardTabFlag   int
)

// dashboardsAddCardCmd attaches an existing card to a dashboard. This is the
// dangerous one (W10b): PUT /api/dashboard/:id has FULL-REPLACEMENT
// semantics for the dashcards array — any dashcard the PUT body omits is
// DELETED. So this command is a mandatory read-modify-write: GET the
// dashboard first, hard-assert the response actually carries a dashcards
// key, pass every existing dashcard through VERBATIM (raw JSON round-trip —
// never decode/re-encode them, which would silently drop fields Metabase
// sent like parameter_mappings/series/action_id), append exactly one new
// dashcard, then PUT the whole thing back.
var dashboardsAddCardCmd = &cobra.Command{
	Use:   "add-card <dashboard> <card>",
	Short: "Attach a card to a dashboard (requires --yes)",
	Long: `Attaches an existing card to a dashboard.

Metabase's PUT /api/dashboard/:id REPLACES the entire dashcard set — a PUT
body that only contains the new card would DELETE every existing card on the
dashboard. This command never constructs the dashcards array from scratch:
it GETs the dashboard first, hard-asserts the response has a "dashcards" key,
and passes every existing dashcard through byte-for-byte, appending only the
one new entry.

<dashboard> is a numeric dashboard id. <card> accepts a numeric card id or a
local alias (see 'mbx aliases list').

Without --row, the new card is placed below every existing card on its tab
(row = max(existing row + size_y), 0 for an empty dashboard). --dry-run
performs the GET (needed to compute the accurate preview) but skips the PUT.

The read-modify-write is NOT atomic: a concurrent add-card run, or a human
editing the dashboard in the Metabase UI, between this command's GET and PUT
gets silently overwritten by this command's PUT (last write wins, per
Metabase's normal PUT semantics). Don't run concurrent add-cards against the
same dashboard.

Requires --yes (or --confirm / MBX_CONFIRM=1). Uses MBX_WRITE_API_KEY if
set, otherwise falls back to METABASE_API_KEY.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		dashID, err := validateID(args[0])
		if err != nil {
			return err
		}
		cardID, _, err := resolveCardID(args[1])
		if err != nil {
			return err
		}

		c, _, err := getClient()
		if err != nil {
			return err
		}

		// Refuse before touching the network at all when this is a plain
		// refusal (no --yes/--confirm/MBX_CONFIRM, no --dry-run): the
		// mandatory GET below is this command's most expensive/riskiest
		// step, no reason to run it just to throw the result away.
		// --dry-run still needs the GET for an accurate preview (existing
		// dashcard count, the new dashcard's JSON), so it skips this early
		// check and falls through to the gate() call below the PUT body is
		// built — dryRunFlag being set there prints the detailed preview.
		if !dryRunFlag {
			if handled, err := gate(cmd, fmt.Sprintf("attach card %d to dashboard %d", cardID, dashID)); handled {
				return err
			}
		}

		// Mandatory read-first. Abort before any write if the GET fails or
		// the response doesn't carry a dashcards array at all — never fall
		// back to constructing one from scratch.
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/dashboard/%d", dashID), nil)
		if err != nil {
			return fmt.Errorf("GET dashboard %d (aborting before any write): %w", dashID, err)
		}
		dashcardsResult := gjson.GetBytes(resp, "dashcards")
		if !dashcardsResult.Exists() || !dashcardsResult.IsArray() {
			return fmt.Errorf("dashboard %d response has no \"dashcards\" array — aborting to avoid a destructive PUT (Metabase's PUT /api/dashboard/:id replaces the entire dashcard set)", dashID)
		}
		existing := dashcardsResult.Array()

		// Raw JSON round-trip — every existing dashcard is carried through
		// byte-for-byte (parameter_mappings, series, visualization_settings,
		// action_id, whatever Metabase sent), never decoded into a typed
		// struct that could silently drop a field.
		existingRaw := make([]json.RawMessage, len(existing))
		existingIDs := make(map[int64]bool, len(existing))
		for i, dc := range existing {
			existingRaw[i] = json.RawMessage(dc.Raw)
			existingIDs[dc.Get("id").Int()] = true
		}

		tabsResult := gjson.GetBytes(resp, "tabs")
		var tabsRaw []json.RawMessage
		var tabIDs []int64
		if tabsResult.IsArray() {
			tabArr := tabsResult.Array()
			tabsRaw = make([]json.RawMessage, len(tabArr))
			for i, t := range tabArr {
				tabsRaw[i] = json.RawMessage(t.Raw)
				tabIDs = append(tabIDs, t.Get("id").Int())
			}
		}

		row := addCardRowFlag
		if !cmd.Flags().Changed("row") {
			row = autoPlacementRow(existing)
		}

		newDashcard := map[string]any{
			"id":                 -1,
			"card_id":            cardID,
			"row":                row,
			"col":                addCardColFlag,
			"size_x":             addCardSizeXFlag,
			"size_y":             addCardSizeYFlag,
			"parameter_mappings": []any{},
			"series":             []any{},
		}
		switch {
		case len(tabIDs) > 0 && cmd.Flags().Changed("tab"):
			found := false
			for _, id := range tabIDs {
				if id == int64(addCardTabFlag) {
					newDashcard["dashboard_tab_id"] = id
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("--tab %d not found on dashboard %d (existing tab ids: %v)", addCardTabFlag, dashID, tabIDs)
			}
		case len(tabIDs) > 0:
			newDashcard["dashboard_tab_id"] = tabIDs[0]
		case cmd.Flags().Changed("tab"):
			return fmt.Errorf("--tab %d given but dashboard %d has no tabs", addCardTabFlag, dashID)
		}

		newDashcardJSON, err := json.Marshal(newDashcard)
		if err != nil {
			return fmt.Errorf("marshal new dashcard: %w", err)
		}

		allDashcards := make([]json.RawMessage, 0, len(existingRaw)+1)
		allDashcards = append(allDashcards, existingRaw...)
		allDashcards = append(allDashcards, newDashcardJSON)
		// Safety belt: the PUT body's dashcard count must be exactly
		// "everything GET returned, plus the one new card" — never less
		// (which would delete cards) and never more.
		if len(allDashcards) != len(existingRaw)+1 {
			return fmt.Errorf("internal error: built %d dashcards, expected %d — aborting before PUT", len(allDashcards), len(existingRaw)+1)
		}

		putBody := map[string]any{"dashcards": allDashcards}
		if tabsRaw != nil {
			putBody["tabs"] = tabsRaw
		}

		action := fmt.Sprintf("PUT /api/dashboard/%d — dashcards: %d existing + 1 new (total %d), tabs: %d passthrough — new dashcard: %s",
			dashID, len(existingRaw), len(allDashcards), len(tabsRaw), newDashcardJSON)
		if handled, err := gate(cmd, action); handled {
			return err
		}

		putResp, err := c.PutWrite(ctxBg(), fmt.Sprintf("/api/dashboard/%d", dashID), putBody)
		if err != nil {
			return err
		}

		newDashcardID := findNewDashcardID(putResp, cardID, existingIDs)
		if newDashcardID == 0 {
			// The PUT already succeeded by this point — this is a
			// reporting gap, not a failure, but it shouldn't pass silently:
			// the caller has no dashcard_id to reference in a follow-up.
			fmt.Fprintln(os.Stderr, "note: write succeeded; could not identify new dashcard id in response")
		}
		out := map[string]any{
			"dashboard_id": dashID,
			"card_id":      cardID,
			"row":          row,
			"col":          addCardColFlag,
			"size_x":       addCardSizeXFlag,
			"size_y":       addCardSizeYFlag,
			"dashcard_id":  newDashcardID,
		}
		outJSON, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.add_card", outJSON, jsonFlag, jqFlag)
	},
}

// autoPlacementRow computes the default row for a new dashcard when --row
// isn't given: the first free row below every existing dashcard on the
// (implicitly first) tab, i.e. max(row + size_y) over existing dashcards.
// 0 for an empty dashboard.
func autoPlacementRow(existing []gjson.Result) int {
	max := 0
	for _, dc := range existing {
		if bottom := int(dc.Get("row").Int()) + int(dc.Get("size_y").Int()); bottom > max {
			max = bottom
		}
	}
	return max
}

// findNewDashcardID locates the server-assigned id of the just-attached
// dashcard in a PUT response: the entry matching cardID whose id wasn't
// already present before the write. Returns 0 if it can't be found
// (best-effort — the write itself already succeeded by this point).
func findNewDashcardID(putResp []byte, cardID int, existingIDs map[int64]bool) int64 {
	for _, dc := range gjson.GetBytes(putResp, "dashcards").Array() {
		if int(dc.Get("card_id").Int()) != cardID {
			continue
		}
		if id := dc.Get("id").Int(); !existingIDs[id] {
			return id
		}
	}
	return 0
}

// dashboardsSubscriptionsCmd lists Metabase "pulses" (subscriptions) for a
// dashboard. GET /api/dashboard/:id/subscriptions does not exist — a prior
// research pass fabricated it; the real, if upstream-deprecated (Metabase
// v0.54 moved alerts to /api/notification; still served as of v0.58 —
// revisit at upgrade time), endpoint is the pulse namespace filtered by
// dashboard_id. Read-only, no confirm gate.
var dashboardsSubscriptionsCmd = &cobra.Command{
	Use:   "subscriptions <id>",
	Short: "List email/Slack subscriptions (pulses) for a dashboard",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := validateID(args[0])
		if err != nil {
			return err
		}
		c, _, err := getClient()
		if err != nil {
			return err
		}
		q := url.Values{}
		q.Set("dashboard_id", fmt.Sprintf("%d", id))
		resp, err := c.Get(ctxBg(), "/api/pulse", q)
		if err != nil {
			return err
		}
		return output.PrintData("dashboards.subscriptions", renderSubscriptions(resp), jsonFlag, jqFlag)
	},
}

type subscriptionOut struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	DashboardID int64        `json:"dashboard_id"`
	Channels    []channelOut `json:"channels"`
}

type channelOut struct {
	ChannelType  string      `json:"channel_type"`
	Enabled      bool        `json:"enabled"`
	Schedule     scheduleOut `json:"schedule"`
	Recipients   []string    `json:"recipients,omitempty"`
	SlackChannel string      `json:"slack_channel,omitempty"`
}

type scheduleOut struct {
	Type  string `json:"type,omitempty"`
	Hour  *int64 `json:"hour,omitempty"`
	Day   string `json:"day,omitempty"`
	Frame string `json:"frame,omitempty"`
}

// renderSubscriptions reshapes GET /api/pulse's response into the
// subscriptionOut shape: schedule fields live on the channel (not the pulse
// root), and recipients vs. Slack channel are mutually exclusive per
// channel_type. An empty/absent pulse list renders as [], not null.
func renderSubscriptions(resp []byte) []byte {
	root := gjson.ParseBytes(resp)
	pulses := root.Array()
	if !root.IsArray() {
		if d := gjson.GetBytes(resp, "data"); d.IsArray() {
			pulses = d.Array()
		}
	}
	subs := make([]subscriptionOut, 0, len(pulses))
	for _, p := range pulses {
		sub := subscriptionOut{
			ID:          p.Get("id").Int(),
			Name:        p.Get("name").String(),
			DashboardID: p.Get("dashboard_id").Int(),
		}
		for _, ch := range p.Get("channels").Array() {
			chOut := channelOut{
				ChannelType: ch.Get("channel_type").String(),
				Enabled:     ch.Get("enabled").Bool(),
				Schedule: scheduleOut{
					Type:  ch.Get("schedule_type").String(),
					Day:   ch.Get("schedule_day").String(),
					Frame: ch.Get("schedule_frame").String(),
				},
			}
			if h := ch.Get("schedule_hour"); h.Exists() {
				hv := h.Int()
				chOut.Schedule.Hour = &hv
			}
			if chOut.ChannelType == "slack" {
				chOut.SlackChannel = ch.Get("details.channel").String()
			} else {
				for _, r := range ch.Get("recipients").Array() {
					if email := r.Get("email").String(); email != "" {
						chOut.Recipients = append(chOut.Recipients, email)
					}
				}
			}
			sub.Channels = append(sub.Channels, chOut)
		}
		subs = append(subs, sub)
	}
	b, err := json.Marshal(subs)
	if err != nil {
		return resp
	}
	return b
}

func init() {
	dashboardsCreateCmd.Flags().StringVar(&dashboardCreateName, "name", "", "Dashboard name (required)")
	dashboardsCreateCmd.Flags().StringVar(&dashboardCreateDescription, "description", "", "Optional description")
	dashboardsCreateCmd.Flags().IntVar(&dashboardCreateCollection, "collection-id", 0, "Optional parent collection ID")

	dashboardsAddCardCmd.Flags().IntVar(&addCardRowFlag, "row", 0, "Row position (default: auto-placed below existing cards)")
	dashboardsAddCardCmd.Flags().IntVar(&addCardColFlag, "col", 0, "Column position")
	dashboardsAddCardCmd.Flags().IntVar(&addCardSizeXFlag, "size-x", 4, "Card width in grid units")
	dashboardsAddCardCmd.Flags().IntVar(&addCardSizeYFlag, "size-y", 4, "Card height in grid units")
	dashboardsAddCardCmd.Flags().IntVar(&addCardTabFlag, "tab", 0, "Target tab id (default: first tab, if the dashboard has tabs)")

	dashboardsCmd.AddCommand(
		dashboardsListCmd, dashboardsGetCmd, dashboardsCardsCmd,
		dashboardsRelatedCmd, dashboardsRevisionsCmd, dashboardsCreateCmd,
		dashboardsAddCardCmd, dashboardsSubscriptionsCmd,
	)
}

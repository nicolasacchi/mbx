package commands

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var tablesCmd = &cobra.Command{
	Use:   "tables",
	Short: "List and inspect tables",
}

var tablesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tables (filter by --database)",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), "/api/table", nil)
		if err != nil {
			return err
		}

		// --database 0 (explicit) shows tables across every DB — no filter,
		// no echo (there's nothing specific to name). Unset or a resolved
		// value (numeric id or named alias) filters to that DB and echoes it,
		// same contract as query/schema/playground/ordini-kpi (W3).
		if databaseFlag == "0" {
			return output.PrintData("tables.list", resp, jsonFlag, jqFlag)
		}
		dbFilter, name, err := resolveDBValue(c, creds, databaseFlag)
		if err != nil {
			return err
		}
		echoResolvedDB(dbFilter, name)
		filtered := filterTablesByDB(resp, dbFilter)
		return output.PrintData("tables.list", filtered, jsonFlag, jqFlag)
	},
}

var tablesGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get table schema (columns, types) by table ID",
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
		// Add ?include_editable_data_model=true to surface field metadata
		q := url.Values{}
		q.Set("include_editable_data_model", "true")
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/table/%d/query_metadata", id), q)
		if err != nil {
			// Fall back to plain /api/table/:id
			resp, err = c.Get(ctxBg(), fmt.Sprintf("/api/table/%d", id), nil)
			if err != nil {
				return err
			}
		}
		return output.PrintData("tables.get", resp, jsonFlag, jqFlag)
	},
}

func init() {
	tablesCmd.AddCommand(tablesListCmd, tablesGetCmd)
}

// filterTablesByDB filters /api/table responses to a given db_id.
// /api/table returns a bare array of tables.
func filterTablesByDB(data []byte, db int) []byte {
	target := strconv.Itoa(db)
	res := gjson.GetBytes(data, `#(db_id=="`+target+`")#`)
	// fall back to integer match
	if !res.Exists() || !res.IsArray() {
		res = gjson.GetBytes(data, `#(db_id==`+target+`)#`)
	}
	if res.Exists() {
		return []byte(res.Raw)
	}
	return data
}

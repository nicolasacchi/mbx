package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var databasesCmd = &cobra.Command{
	Use:     "databases",
	Aliases: []string{"db"},
	Short:   "List and inspect Metabase databases",
}

var databasesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all connected databases",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		// The /api/database endpoint returns an envelope: {"data": [...], "total": N}.
		// We unwrap to a bare array for table rendering.
		resp, err := c.Get(ctxBg(), "/api/database", nil)
		if err != nil {
			return err
		}
		if data := gjson.GetBytes(resp, "data"); data.IsArray() {
			return output.PrintData("databases.list", []byte(data.Raw), jsonFlag, jqFlag)
		}
		return output.PrintData("databases.list", resp, jsonFlag, jqFlag)
	},
}

var databasesGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get database details by ID",
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
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/database/%d", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("databases.get", resp, jsonFlag, jqFlag)
	},
}

func init() {
	databasesCmd.AddCommand(databasesListCmd, databasesGetCmd)
}

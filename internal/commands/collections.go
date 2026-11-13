package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var collectionsCmd = &cobra.Command{
	Use:     "collections",
	Aliases: []string{"col"},
	Short:   "List collections (folders) and their items",
}

var collectionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all collections",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), "/api/collection", nil)
		if err != nil {
			return err
		}
		if data := gjson.GetBytes(resp, "data"); data.IsArray() {
			return output.PrintData("collections.list", []byte(data.Raw), jsonFlag, jqFlag)
		}
		return output.PrintData("collections.list", resp, jsonFlag, jqFlag)
	},
}

var collectionsItemsCmd = &cobra.Command{
	Use:   "items <id>",
	Short: "List items in a collection",
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
		resp, err := c.Get(ctxBg(), fmt.Sprintf("/api/collection/%d/items", id), nil)
		if err != nil {
			return err
		}
		return output.PrintData("collections.items", resp, jsonFlag, jqFlag)
	},
}

func init() {
	collectionsCmd.AddCommand(collectionsListCmd, collectionsItemsCmd)
}

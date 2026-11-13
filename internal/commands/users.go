package commands

import (
	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/output"
)

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "List Metabase users",
}

var usersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List users",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		resp, err := c.Get(ctxBg(), "/api/user", nil)
		if err != nil {
			return err
		}
		if data := gjson.GetBytes(resp, "data"); data.IsArray() {
			return output.PrintData("users.list", []byte(data.Raw), jsonFlag, jqFlag)
		}
		return output.PrintData("users.list", resp, jsonFlag, jqFlag)
	},
}

func init() {
	usersCmd.AddCommand(usersListCmd)
}

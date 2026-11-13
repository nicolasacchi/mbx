package commands

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

var playgroundCmd = &cobra.Command{
	Use:   "playground <SQL>",
	Short: "Print a Metabase native playground URL with the SQL pre-filled",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, creds, err := getClient()
		if err != nil {
			return err
		}
		db, err := resolveDB(c, creds)
		if err != nil {
			return err
		}
		// Metabase URL hash format:
		//   /question#{"dataset_query":{"type":"native","native":{"query":"..."},"database":N}}
		payload := fmt.Sprintf(
			`{"dataset_query":{"type":"native","native":{"query":%q},"database":%d}}`,
			args[0], db,
		)
		// Hash fragments shouldn't be URL-encoded for Metabase, but spaces/quotes need it.
		encoded := url.QueryEscape(payload)
		fmt.Printf("%s/question#%s\n", creds.URL, encoded)
		return nil
	},
}

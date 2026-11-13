package commands

import (
	"net/url"

	"github.com/spf13/cobra"

	"github.com/nicolasacchi/mbx/internal/output"
)

var (
	searchModelsFlag string
	searchLimitFlag  int
)

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search across cards, dashboards, collections, models",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := getClient()
		if err != nil {
			return err
		}
		q := url.Values{}
		q.Set("q", args[0])
		if searchModelsFlag != "" {
			q.Set("models", searchModelsFlag)
		}
		if searchLimitFlag > 0 {
			q.Set("limit", itoa(searchLimitFlag))
		}
		resp, err := c.Get(ctxBg(), "/api/search", q)
		if err != nil {
			return err
		}
		return output.PrintData("search.results", resp, jsonFlag, jqFlag)
	},
}

func init() {
	searchCmd.Flags().StringVar(&searchModelsFlag, "models", "", "Comma-separated model filter: card,dashboard,collection,table,database")
	searchCmd.Flags().IntVar(&searchLimitFlag, "limit", 0, "Cap result count")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

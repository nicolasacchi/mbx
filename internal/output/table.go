package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/tidwall/gjson"
)

// ColumnDef defines one table column for one command.
type ColumnDef struct {
	Header string
	Key    string // gjson path *within* one row
	Format FormatFunc
}

// commandColumns maps "<group>.<leaf>" command keys to column layouts.
// Commands without an entry fall through to the JSON output path.
var commandColumns = map[string][]ColumnDef{
	"databases.list": {
		{Header: "ID", Key: "id"},
		{Header: "NAME", Key: "name"},
		{Header: "ENGINE", Key: "engine"},
		{Header: "TIMEZONE", Key: "timezone"},
	},
	"tables.list": {
		{Header: "ID", Key: "id"},
		{Header: "SCHEMA", Key: "schema"},
		{Header: "NAME", Key: "name"},
		{Header: "DB_ID", Key: "db_id"},
	},
	"cards.list": {
		{Header: "ID", Key: "id"},
		{Header: "NAME", Key: "name", Format: Truncate(60)},
		{Header: "DB", Key: "database_id"},
		{Header: "LAST_QUERY", Key: "last_query_start", Format: FormatTimestamp},
		{Header: "ARCHIVED", Key: "archived", Format: FormatBool},
	},
	"dashboards.list": {
		{Header: "ID", Key: "id"},
		{Header: "NAME", Key: "name", Format: Truncate(60)},
		{Header: "ARCHIVED", Key: "archived", Format: FormatBool},
		{Header: "UPDATED", Key: "updated_at", Format: FormatTimestamp},
	},
	"collections.list": {
		{Header: "ID", Key: "id"},
		{Header: "NAME", Key: "name"},
		{Header: "LOCATION", Key: "location"},
		{Header: "PERSONAL", Key: "is_personal", Format: FormatBool},
	},
	"users.list": {
		{Header: "ID", Key: "id"},
		{Header: "EMAIL", Key: "email"},
		{Header: "NAME", Key: "common_name"},
		{Header: "SUPERUSER", Key: "is_superuser", Format: FormatBool},
		{Header: "LAST_LOGIN", Key: "last_login", Format: FormatTimestamp},
	},
	"search.results": {
		{Header: "MODEL", Key: "model"},
		{Header: "ID", Key: "id"},
		{Header: "NAME", Key: "name", Format: Truncate(60)},
		{Header: "COLLECTION", Key: "collection.name"},
	},
	"aliases.list": {
		{Header: "ALIAS", Key: "alias"},
		{Header: "CARD_ID", Key: "card_id"},
		{Header: "DB", Key: "database_id"},
		{Header: "NAME", Key: "name", Format: Truncate(60)},
	},
	"schema.gotchas": {
		{Header: "DB", Key: "db"},
		{Header: "TABLE", Key: "table"},
		{Header: "COLUMN", Key: "column"},
		{Header: "HINT", Key: "hint", Format: Truncate(100)},
	},
}

// IndexColumnDef defines one table column for commands whose row shape is
// the {columns:[...], rows:[[...],...]} object form (array-of-arrays) —
// i.e. simplifyQueryResponse's shape, straight from a native SQL query —
// rather than an array of row objects. Idx selects the cell position within
// each row array; it must match the SQL column order, NOT any JSON key.
type IndexColumnDef struct {
	Header string
	Idx    int
	Format FormatFunc
}

// commandIndexColumns maps "<group>.<leaf>" keys whose row shape is the
// {columns, rows} object form. Checked before commandColumns in printTable.
//
// activity.recent used to be keyed here as an object-array command (the old
// removed /api/activity REST shape); it now comes from a native SQL query
// against the Metabase Internal DB via simplifyQueryResponse, which emits
// {columns, rows} — an object, not an array — so the old object-keyed map
// made printTable hard-fail ("no array at \"@this\"") on every TTY
// invocation instead of rendering (W5). Column order here must match the
// SELECT list in activityRecentCmd's SQL (commands/activity.go).
var commandIndexColumns = map[string][]IndexColumnDef{
	"activity.recent": {
		{Header: "WHEN", Idx: 0, Format: FormatTimestamp},
		{Header: "MS", Idx: 1},
		{Header: "ROWS", Idx: 2},
		{Header: "CONTEXT", Idx: 3},
		{Header: "ERROR", Idx: 4, Format: Truncate(40)},
		{Header: "CARD", Idx: 9, Format: Truncate(40)},
		{Header: "USER", Idx: 10},
		{Header: "DB", Idx: 11},
	},
}

// commandRowsPath overrides the gjson path used to extract the rows array.
// Default is "@this" (root array). Object responses with `data.foo` arrays
// override here.
var commandRowsPath = map[string]string{
	"search.results": "data",
}

var errNoTable = errors.New("no table layout")

// printTable dispatches on the row shape a command's output uses: the
// {columns, rows} array-of-arrays form (commandIndexColumns) checked first,
// then the legacy array-of-row-objects form (commandColumns). Neither
// registered → errNoTable (PrintData's cue to go straight to JSON).
func printTable(command string, data []byte) error {
	if cols, ok := commandIndexColumns[command]; ok {
		return printIndexTable(cols, data)
	}

	cols, ok := commandColumns[command]
	if !ok {
		return errNoTable
	}

	rowsPath := commandRowsPath[command]
	if rowsPath == "" {
		rowsPath = "@this"
	}

	rows := gjson.GetBytes(data, rowsPath)
	if !rows.Exists() || !rows.IsArray() {
		return fmt.Errorf("no array at %q", rowsPath)
	}

	tw := newTableWriter(headersFor(cols, func(c ColumnDef) string { return c.Header }))

	rendered := 0
	rows.ForEach(func(_, row gjson.Result) bool {
		out := make(table.Row, len(cols))
		for i, c := range cols {
			cell := row.Get(c.Key)
			var v any
			if !cell.Exists() {
				v = ""
			} else {
				v = unwrap(cell)
			}
			if c.Format != nil {
				out[i] = c.Format(v)
			} else {
				out[i] = v
			}
		}
		tw.AppendRow(out)
		rendered++
		return true
	})

	return renderOrEmpty(tw, rendered)
}

// printIndexTable renders the {columns, rows} array-of-arrays shape
// (simplifyQueryResponse's output) — cells are addressed by position within
// each row array, not by a JSON key, since that shape carries no per-row
// field names.
func printIndexTable(cols []IndexColumnDef, data []byte) error {
	rows := gjson.GetBytes(data, "rows")
	if !rows.Exists() || !rows.IsArray() {
		return fmt.Errorf("no \"rows\" array in data")
	}

	tw := newTableWriter(headersFor(cols, func(c IndexColumnDef) string { return c.Header }))

	rendered := 0
	rows.ForEach(func(_, row gjson.Result) bool {
		if !row.IsArray() {
			return true // skip a malformed row rather than panic on index
		}
		cells := row.Array()
		out := make(table.Row, len(cols))
		for i, c := range cols {
			var v any
			if c.Idx < len(cells) {
				v = unwrap(cells[c.Idx])
			}
			if c.Format != nil {
				out[i] = c.Format(v)
			} else {
				out[i] = v
			}
		}
		tw.AppendRow(out)
		rendered++
		return true
	})

	return renderOrEmpty(tw, rendered)
}

func newTableWriter(headers table.Row) table.Writer {
	tw := table.NewWriter()
	tw.SetOutputMirror(os.Stdout)
	tw.SetStyle(table.StyleLight)
	tw.Style().Format.Header = text.FormatUpper
	tw.AppendHeader(headers)
	return tw
}

func headersFor[T any](cols []T, header func(T) string) table.Row {
	headers := make(table.Row, len(cols))
	for i, c := range cols {
		headers[i] = header(c)
	}
	return headers
}

func renderOrEmpty(tw table.Writer, rendered int) error {
	if rendered == 0 {
		fmt.Println("(no rows)")
		return nil
	}
	tw.Render()
	return nil
}

func unwrap(r gjson.Result) any {
	switch r.Type {
	case gjson.Number:
		return r.Num
	case gjson.String:
		return r.Str
	case gjson.True:
		return true
	case gjson.False:
		return false
	case gjson.Null:
		return nil
	default:
		var v any
		_ = json.Unmarshal([]byte(r.Raw), &v)
		return v
	}
}

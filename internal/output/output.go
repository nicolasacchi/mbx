// Package output handles JSON / table rendering and gjson filtering.
//
// Convention (matches ddx/jx/gx/stx):
//   - TTY default: table (when registered for the command)
//   - Pipe default: JSON
//   - --json: forces JSON on TTY
//   - --jq <gjson>: filters; applied only on the JSON path
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tidwall/gjson"
)

// PrintData writes data to stdout. Behaviour:
//   - TTY + command has a column map + !jsonMode + no jqFilter → table render
//   - otherwise → JSON (with optional gjson --jq filter)
//
// Compact mode (CLAUDECODE=1 or --compact) bypasses the indent path and
// writes raw bytes directly. Saves ~18-25% output bytes for agent invocations.
//
// The command key is "<group>.<leaf>" (e.g. "databases.list").
//
// A registered-but-mismatched column map (any printTable error other than
// "no layout at all") degrades soft to JSON with a one-line stderr note
// instead of failing the command (W5) — this is what let
// `mbx activity recent` hard-fail on every TTY invocation when its column
// map drifted out of sync with the real response shape; the fallback
// de-risks every future drift of the same kind, not just that one command.
func PrintData(command string, data []byte, jsonMode bool, jqFilter string) error {
	useTable := !jsonMode && jqFilter == "" && IsTTY() && !compactRequested()
	if useTable {
		err := printTable(command, data)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errNoTable) {
			fmt.Fprintf(os.Stderr, "note: table render failed (%v); falling back to JSON\n", err)
		}
	}

	if jqFilter != "" {
		filtered, err := ApplyFilter(data, jqFilter)
		if err != nil {
			return err
		}
		data = filtered
	}
	return printJSON(os.Stdout, data)
}

// compactRequested reports whether the caller wants compact JSON output.
// Set by the commands layer via SetCompact (when --compact or CLAUDECODE=1).
var compactFlagInternal bool

// SetCompact toggles compact JSON output. Called from commands/root.go.
func SetCompact(on bool) { compactFlagInternal = on }

func compactRequested() bool {
	if compactFlagInternal {
		return true
	}
	return os.Getenv("CLAUDECODE") == "1" || os.Getenv("MBX_AGENT_MODE") == "1"
}

// ApplyFilter runs a gjson expression against data.
// gjson syntax — NOT real jq. Examples:
//
//	"0.name"                       first element's name
//	"#.name"                       all names (array)
//	"#.{id:id,name:name}"          project per element
//	"data.rows.0"                  first row of a query result
func ApplyFilter(data []byte, expr string) ([]byte, error) {
	res := gjson.GetBytes(data, expr)
	if !res.Exists() {
		return []byte("null"), nil
	}
	return []byte(res.Raw), nil
}

func printJSON(w io.Writer, data []byte) error {
	// Compact mode (CLAUDECODE=1 or --compact) skips the pretty-print path
	// even on TTY — saves the json.Unmarshal+Encode double-pass.
	if isTTY(w) && !compactRequested() {
		var pretty bytesBuf
		if err := jsonIndent(&pretty, data); err == nil {
			_, err := fmt.Fprintln(w, pretty.String())
			return err
		}
	}
	_, err := w.Write(append(data, '\n'))
	return err
}

// isTTYFunc backs IsTTY — a package-level var (rather than calling isTTY
// directly) so tests can force the TTY-only table-render path without a
// real terminal attached to the test binary's stdout.
var isTTYFunc = func() bool { return isTTY(os.Stdout) }

// IsTTY returns true when stdout is a terminal.
func IsTTY() bool { return isTTYFunc() }

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

type bytesBuf struct{ buf []byte }

func (b *bytesBuf) Write(p []byte) (int, error) { b.buf = append(b.buf, p...); return len(p), nil }
func (b *bytesBuf) String() string              { return string(b.buf) }

func jsonIndent(dst *bytesBuf, src []byte) error {
	var v any
	if err := json.Unmarshal(src, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(dst)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

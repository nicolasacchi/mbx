package commands

// W3 — named database aliases, resolved live against GET /api/database
// rather than a hardcoded id table. Operators otherwise memorize numeric ids,
// and a wrong --database value is indistinguishable from "no data". Database
// ids churn (a DB gets retired or re-added), so hardcoding alias→id would
// silently break —
// matching on engine+name instead means a retired/renamed DB fails closed
// with "alias not found" and the live DB list, never a guessed id.

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/nicolasacchi/mbx/internal/client"
)

// dbEntry is the subset of GET /api/database fields the alias matcher needs.
type dbEntry struct {
	ID     int
	Name   string
	Engine string
}

// dbAliasPattern matches a named --database alias by engine + name pattern.
type dbAliasPattern struct {
	engine string
	name   *regexp.Regexp
}

// dbAliasPatterns is deliberately NOT an alias→id map — see the package
// doc comment above. Patterns verified live 2026-07-20 against the real
// GET /api/database names.
var dbAliasPatterns = map[string]dbAliasPattern{
	"prod":       {engine: "mysql", name: regexp.MustCompile(`(?i)^production\s+1000farmacie\s+replica$`)},
	"clickhouse": {engine: "clickhouse", name: regexp.MustCompile(`(?i)^clickhouse\s+analytics$`)},
	"ops":        {engine: "mysql", name: regexp.MustCompile(`(?i)^operations\s+1000farmacie\s+replica$`)},
	"internal":   {engine: "postgres", name: regexp.MustCompile(`(?i)^metabase\s+internal\s+db$`)},
}

// knownDBAliases lists recognized alias names in a stable order for error messages.
var knownDBAliases = []string{"prod", "clickhouse", "ops", "internal"}

// unknownDBAliasErr reports a --database value that isn't a recognized
// alias name at all — distinct from a recognized alias with no live match
// (matchDBAlias's other error). Shared so the message is identical whether
// the check runs before (resolveDBAliasLive, no network needed) or during
// (matchDBAlias, given an already-fetched listing) live resolution.
func unknownDBAliasErr(alias string) error {
	return fmt.Errorf("unknown --database value %q (known aliases: %s; or a numeric id)", alias, strings.Join(knownDBAliases, ", "))
}

// matchDBAlias finds the dbEntry a named alias resolves to within a live
// GET /api/database listing. Pure (no network) — directly table-testable.
func matchDBAlias(alias string, dbs []dbEntry) (dbEntry, error) {
	pat, ok := dbAliasPatterns[strings.ToLower(alias)]
	if !ok {
		return dbEntry{}, unknownDBAliasErr(alias)
	}
	for _, d := range dbs {
		if d.Engine == pat.engine && pat.name.MatchString(d.Name) {
			return d, nil
		}
	}
	return dbEntry{}, fmt.Errorf("alias %q not found; run 'mbx databases list' — live databases: %s", alias, formatDBList(dbs))
}

func formatDBList(dbs []dbEntry) string {
	parts := make([]string, 0, len(dbs))
	for _, d := range dbs {
		parts = append(parts, fmt.Sprintf("%d=%s(%s)", d.ID, d.Name, d.Engine))
	}
	return strings.Join(parts, ", ")
}

// --- live listing, cached per-process: one CLI invocation is one process,
// so a sync.Once is enough to satisfy "cache the listing per-invocation"
// without adding a TTL/expiry concept that a one-shot CLI never needs. ---

var (
	dbListOnce  sync.Once
	dbListCache []dbEntry
	dbListErr   error
)

// liveDBList fetches GET /api/database once per process and reuses the
// result for every alias resolution / echo-name lookup in the same
// invocation.
func liveDBList(c *client.Client) ([]dbEntry, error) {
	dbListOnce.Do(func() {
		dbListCache, dbListErr = fetchDBList(c)
	})
	return dbListCache, dbListErr
}

func fetchDBList(c *client.Client) ([]dbEntry, error) {
	resp, err := c.Get(ctxBg(), "/api/database", nil)
	if err != nil {
		return nil, err
	}
	data := resp
	if d := gjson.GetBytes(resp, "data"); d.IsArray() {
		data = []byte(d.Raw)
	}
	var entries []dbEntry
	gjson.ParseBytes(data).ForEach(func(_, v gjson.Result) bool {
		entries = append(entries, dbEntry{
			ID:     int(v.Get("id").Int()),
			Name:   v.Get("name").String(),
			Engine: v.Get("engine").String(),
		})
		return true
	})
	return entries, nil
}

// resolveDBAliasLive resolves a named alias against the live listing.
// Rejects an alias name that isn't recognized at all BEFORE fetching —
// otherwise a plain typo while Metabase happens to be unreachable gets
// misreported as a network/fetch error instead of the real "unknown alias".
func resolveDBAliasLive(c *client.Client, alias string) (dbEntry, error) {
	if _, ok := dbAliasPatterns[strings.ToLower(alias)]; !ok {
		return dbEntry{}, unknownDBAliasErr(alias)
	}
	dbs, err := liveDBList(c)
	if err != nil {
		return dbEntry{}, fmt.Errorf("fetch live database list for alias %q: %w", alias, err)
	}
	return matchDBAlias(alias, dbs)
}

// lookupDBName resolves a numeric database id to its live display name —
// best-effort, empty string on any failure (a cosmetic echo must never fail
// the caller's command).
func lookupDBName(c *client.Client, id int) string {
	dbs, err := liveDBList(c)
	if err != nil {
		return ""
	}
	for _, d := range dbs {
		if d.ID == id {
			return d.Name
		}
	}
	return ""
}

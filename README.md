# mbx

Read-and-write CLI for the [Metabase API](https://www.metabase.com/docs/latest/api-documentation). Go, single binary, JSON output, gjson `--jq` filters, opinionated `--database 2` default (override per project with `default_db` or via `MBX_DEFAULT_DB`), dynamic card aliases.

**Status (v0.4)**: 37 leaf commands across 15 top-level groups (`mbx --help` is the source of truth for this count — it drifted from earlier "32 commands / 12 groups" wording as Waves 2-5 added subcommands without the banner catching up each time). v0.4 (Wave 5 — hygiene & hardening, closing the v0.4 roadmap) generalizes the schema-gotcha lint from a hardcoded ordini-only check into a curated `(db, table, column) -> hint` table shared by three surfaces — the SQL-lint stderr warning, the `unknown_column` error-envelope hint, and the new `mbx schema --gotchas [table]` command — with entries now covering `prodotti` and `coupons` (`pharmacy_product_listings` too) alongside the original `ordini` pair, each verified live on 2026-07-20. `mbx cards metadata <id|alias>` adds a one-call "explain this card" shortcut (`GET /api/card/:id/query_metadata`, falling back to a plain card GET). Wave 5 also closes out the May SEC-001/2/3 security backlog: every positional/query-parameter id across the read surface (`databases get`, `tables get`, `collections items`, `dashboards get/cards/related/revisions/add-card/subscriptions`) is now validated as a bare non-negative integer before it's spliced into a request (`kind: "validation"`, exit 3, on rejection) instead of being trusted as-is; the `https://`-or-`localhost` URL-scheme guard (`config add`, Wave 2) now also covers `LoadCredentials` itself, so a hand-edited config file or a stray `METABASE_URL=http://...` fails fast instead of sailing through; and the two remaining denylist-style sanitizers (`schema`'s table-name splice, `activity`'s `--since` interval splice) were converted from character-stripping to reject-not-strip allowlist regexes. v0.4 (Wave 4) added the first real write surface beyond an empty dashboard: `cards create` (POST a native-SQL saved question) and `dashboards add-card` (attach a card to a dashboard via a mandatory read-modify-write — `PUT /api/dashboard/:id` REPLACES the entire dashcard set, so this never constructs the array from scratch), plus read-only `dashboards subscriptions`. The confirm gate itself was rebuilt on the fleet-shared `clicore/confirm` package: refusing a write now exits **6** (`write_locked`) instead of the old bare exit 1, `--yes` is the primary confirmation flag (`--confirm` is now an alias, `MBX_CONFIRM=1` still works), and every write command supports `--dry-run` to preview the exact request without sending it. v0.4 (Wave 3) adds `kind: "network"`/`"timeout"` to the structured error envelope for transport-layer failures — dial errors, DNS misses, TLS handshake failures, client timeouts — which used to fall through as unclassified `client_error`; a `network`-kind failure against the built-in sandbox host (`sandboxAgentHost` in `internal/commands/network_hint.go`) carries a hint naming `dangerouslyDisableSandbox` as the likely cause. `mbx overview` now reports per-database health (`SELECT 1` liveness probe), and `mbx cards execute` appends an unreachable-DB hint on a failure whose database also probes dead. `mbx config doctor` triages connectivity vs. auth by probing `/api/health` before the auth check. v0.3 baseline: HTTP/2 multiplexing, `mbx ordini-kpi` (canonical KPIs without schema-gotcha SQL), structured JSON error envelope (`{kind, message, table, column, hint, db}`), agent-mode row cap (100 rows when `CLAUDECODE=1`), compact JSON output, `--timing` flag, parallel `aliases sync` (5×), bounded `overview` goroutine pool, and streaming `cards export` (no more buffering 100 MB CSVs in RAM). Plus the v0.2 baseline: full MCP-parity read surface + parameterized cards + activity log + card revisions + `dashboards create` (gated).

## Why mbx vs the metabase MCP

When deciding whether to migrate from the `@cognitionai/metabase-mcp-server` MCP to this CLI, three honest things to know:

### 1. Per-HTTP-call wire time is roughly tied

Both go to the same Metabase instance. The dominant cost is the HTTPS round-trip (~100–500 ms depending on query). The MCP reuses a persistent Node.js process so it skips Go's cold-start (~30–50 ms); `mbx` skips the JSON-RPC double-encoding and Node.js wrapper logic. Net: **within ±100 ms on a single call**. If anything, MCP can be marginally faster per call once already running.

So if you're measuring "how long until the bytes return", they're a wash.

### 2. Session-init: mbx wins by 1–3 s, paid every session restart

```
.mcp.json → Claude Code spawns: npx -y @cognitionai/metabase-mcp-server
            ↓
            npx: resolve package → spawn node → load module → MCP handshake
            ↓
            ~1–3 s, paid ONCE per session
```

`mbx` is a single Go binary invoked on demand. Zero session-init cost. Whenever Claude Code restarts (session limits, `/clear`, IDE reconnect), the MCP pays the spawn cost again; `mbx` doesn't.

### 3. Token / context cost: mbx wins by ~50–100×

This is the real win for agentic use. Same query — `SELECT COUNT(*) FROM ordini WHERE created_at >= NOW() - INTERVAL 24 HOUR`:

| | MCP response | mbx response |
|---|---|---|
| Bytes | ~3 KB | ~150 B (raw) / ~10 B (`--jq 'rows.0.0'`) |
| Lines | ~90 | 1 |
| Approx tokens | ~750–1000 | ~10–40 |

The MCP returns the integer `1234` wrapped in `data.cols[].fingerprint.global.{distinct-count,nil%}.type.{type/Number}.{min,q1,q3,max,sd,avg}` plus `native_form`, `json_query`, `results_metadata`, `cached`, `started_at`, `database_id`, `average_execution_time`, `status`, `context`, `row_count`, `running_time`. `mbx` returns the integer.

The agent has to *read* every byte the tool returned. Across an agent workflow with 8 Metabase calls, that's ~6–8 K input tokens of noise per run vs ~80–300 with `mbx`. And Claude has to think about every one of those bytes — input-processing time scales with context size, so this shows up as agent latency too.

### 4. Error path: mbx is dramatically cleaner on bad SQL

When SQL references a column that doesn't exist (e.g. `WHERE stato = …` on `ordini` — see `mbx schema ordini`):

| | MCP | mbx |
|---|---|---|
| Lines | ~700 | 3 |
| Bytes | ~30 KB | ~200 B |
| Stack traces | 3 (MariaDB → JDBC → Clojure ExceptionInfo) | 0 |
| Pre-emptive warning | none | yes — `warning: ordini.stato — the real status column is 'state' (varchar) — values include 'review', 'in_attesa_corriere', 'annullato', 'spedito', 'consegnato'` |

This matters disproportionately for agent loops that retry-on-error: with the MCP, an iteration eats ~7,000 tokens of stack trace before retrying; with `mbx`, ~30. The lint warning often prevents the wrong query from running at all.

### 5. Capabilities mbx has, MCP doesn't

These features simply don't exist via the MCP path:

- **Parameterized cards**: `mbx cards execute funnel --param from_date=2026-05-06 --param to_date=2026-05-08`. Any card whose native SQL declares template-tags (here `from_date`/`to_date`) can be run with overrides; `execute_card` always uses defaults.
- **Activity log**: `mbx activity recent --since "1 hour"`. The REST `/api/activity` endpoint was removed in newer Metabase versions; `mbx` instead reads query-execution history from Metabase's own application database (Postgres), if your instance exposes it as a data source. The database id is currently a compile-time constant (`auditDB` in `internal/commands/activity.go`) — adjust it for your instance.
- **Card revisions**: `mbx cards revisions <id|alias>`. The MCP exposes only dashboard revisions, not card revisions.

### 6. Default `--database 2` + `mbx schema ordini` gotcha note

Encoded institutional knowledge — DB 2 is the default for `query`/`schema`/`tables list`/`playground`; `mbx schema ordini` flags that `stato` and `totale_ordine` don't exist (the real status column is `state`). The MCP requires `database_id` on every call and surfaces no schema gotchas.

### Bottom line

The headline "is mbx faster" depends on what you measure:

- **Raw HTTP latency**: ≈ tied.
- **Session-init**: mbx wins by 1–3 s every restart.
- **Agent-perceived latency** (context to read): mbx wins by ~50–100× per call.
- **Error iteration cost**: mbx wins large on every malformed-SQL retry.
- **Capability**: three mbx-only commands the MCP cannot deliver.

For human use the difference is subtle. For AI-agent use (which is the design target — see `CLAUDE.md`), it compounds quickly across a multi-call slash command.

## Install

```bash
go install github.com/nicolasacchi/mbx/cmd/mbx@latest
```

Or build from source:

```bash
git clone https://github.com/nicolasacchi/mbx.git
cd mbx
make install   # installs bin/mbx into $GOBIN
```

## Quick Start

```bash
# 1. Get a Metabase API key:
#    https://metabase.example.com/admin/settings/authentication/api-keys
# 2. Export env vars (also picked up by direnv)
export METABASE_URL='https://metabase.example.com'
export METABASE_API_KEY='mb_...'

# 3. Verify auth + permissions
mbx config doctor
# or, to write those exports into a named project instead:
mbx config add production --url "$METABASE_URL" --api-key "$METABASE_API_KEY"

# 4. Use it
mbx databases list
mbx query "SELECT COUNT(*) FROM ordini WHERE created_at >= NOW() - INTERVAL 24 HOUR"
mbx schema ordini                          # INFORMATION_SCHEMA + ordini gotcha note
mbx aliases sync                            # populate ~/.config/mbx/aliases.toml
mbx cards execute orders-by-product         # alias-aware
mbx overview                                # parallel: DB list + named cards + auth
```

## Quick recipes

```bash
# 24-hour order volume, exit 1 on query error (good for health checks)
mbx query "SELECT COUNT(*) AS orders FROM ordini WHERE created_at >= NOW() - INTERVAL 24 HOUR" --jq 'rows.0.0'

# Discover columns before writing SQL — flags ordini.stato/totale_ordine traps
mbx schema ordini

# Dashboard list as projection
mbx dashboards list --jq '#.{id:id,name:name,archived:archived}'

# Search across cards, dashboards, models
mbx search "ordini" --models card --jq 'data.#.{model:model,id:id,name:name}'

# Round-trip-execute every named card to confirm liveness (5-30s)
mbx overview --deep

# Open a query in the Metabase native playground in your browser
xdg-open "$(mbx playground 'SELECT COUNT(*) FROM ordini')"

# Export a saved question as CSV
mbx cards export 53 --format csv > orders_by_product.csv

# v0.2: parameterized card execution (override from_date / to_date template-tags)
mbx cards execute funnel --param from_date=2026-05-01 --param to_date=2026-05-08 --jq 'row_count'

# v0.2: query history — what ran, by whom, in the last hour
mbx activity recent --since "1 hour" --limit 25 --jq '#.{started:0,ms:1,rows:2,by:10,card:9}'

# v0.2: card revisions
mbx cards revisions orders-by-product --jq '#.{when:timestamp,by:user.common_name,what:description}'

# v0.3: canonical ordini KPIs (replaces hand-coded SQL with the schema gotcha)
mbx ordini-kpi --window 24h
mbx ordini-kpi --window 3d --jq '{order_count:order_count,cancelled:cancelled}'

# v0.3: agent-mode row cap (CLAUDECODE=1 → default 100 rows + truncated marker in JSON)
CLAUDECODE=1 mbx query "SELECT id FROM ordini" --jq '{count:row_count,cap:row_cap,truncated:truncated}'

# v0.4: server-side 2,000-row display cap, detected heuristically (row_cap_source: "server")
mbx query "SELECT id FROM categories" --rows 0 --jq '{count:row_count,src:row_cap_source}'

# v0.4: --csv routes through the real Metabase export endpoint — bypasses BOTH
# mbx's --rows and Metabase's 2,000-row display cap (no truncated marker possible)
mbx query "SELECT id FROM categories" --csv | wc -l

# v0.4: --csv failures now go through the A3 envelope too — including the
# common case where Metabase's export endpoint returns 200 OK with a JSON
# error body instead of real CSV on bad SQL (used to silently pipe the JSON
# to stdout as if it were CSV and exit 0)
mbx query "SELECT bad_col FROM ordini" --csv 2>&1 >/dev/null | head -1
# {"error":{"kind":"unknown_column","table":"ordini","column":"bad_col","db":2}}

# v0.4: named --database aliases, resolved live against GET /api/database —
# every resolved invocation echoes "database: <id> (<name>)" to stderr
mbx query "SELECT 1" --database prod         # resolved by engine + name, not a fixed id
mbx query "SELECT 1" --database ops          # keeps working if the database's id changes

# v0.4: canonical GMV — replaces the dead JSON_EXTRACT(google_analytics_measures, ...)
# recipe (that table has since been dropped); windowed on customer_confirm_at
mbx ordini-kpi --window 24h --jq '{orders:order_count,gmv:gmv}'

# v0.4: machine-readable auth doctor for scripts/health checks
mbx config doctor --json --require; echo "exit=$?"   # exit 2 on any auth problem

# v0.4: provision a named project non-interactively (writes ~/.config/mbx/config.toml)
mbx config add production --url https://metabase.example.com --api-key mb_...

# v0.3: structured error envelope (stderr) — for AI agent self-correction loops
mbx query "SELECT * FROM ordini WHERE stato='x'" 2>&1 >/dev/null | head -1
# {"error":{"kind":"unknown_column","table":"ordini","column":"stato","hint":"the real status column is 'state'…","db":2}}

# v0.4: network/timeout failures are now classified too (used to be an
# unclassified "client_error") — e.g. a forgotten dangerouslyDisableSandbox:
mbx query "SELECT 1" 2>&1 >/dev/null | head -1
# {"error":{"kind":"network","message":"…dial tcp: connect: connection refused","hint":"network failure reaching <sandbox host> — … needs dangerouslyDisableSandbox: true …","db":2}}

# v0.4: per-database health as part of overview
mbx overview --jq 'database_health'
# [{"id":2,"name":"Example Replica","engine":"mysql","ok":true}, ...]

# v0.3: timing
mbx --timing query "SELECT 1"   # stderr: mbx: POST /api/dataset — server=3ms wall=607ms

# v0.4: write refusal without --yes exits 6 (write_locked) — was exit 1 before
mbx dashboards create --name "x"; echo "exit=$?"   # exit=6, no request sent

# v0.4: --dry-run previews the exact request without sending it
mbx cards create --name "orders" --sql "SELECT 1" --database prod --dry-run
# --dry-run: would POST /api/card with body {...}, no changes made

# v0.4: create a saved question from SQL, register a local alias for it
mbx cards create --name "mbx-smoke" --sql "SELECT 1" --database prod --alias mbx-smoke --yes
# {"id":789,"name":"mbx-smoke","database_id":2}

# v0.4: attach a card to a dashboard — mandatory read-modify-write under the
# hood (PUT /api/dashboard/:id replaces the WHOLE dashcard set; mbx GETs
# first and passes every existing card through byte-for-byte). NOT atomic —
# a concurrent add-card or a human editing the dashboard in the UI between
# the GET and the PUT gets silently overwritten; don't run two at once
# against the same dashboard.
mbx dashboards add-card 42 mbx-smoke --yes
mbx dashboards cards 42   # existing cards are still there — no clobber

# v0.4: list a dashboard's email/Slack subscriptions (GET /api/pulse, the
# real endpoint — /api/dashboard/:id/subscriptions does not exist)
mbx dashboards subscriptions 42

# v0.4 Wave 5: curated (db, table, column) gotcha table — no network call
mbx schema --gotchas               # every curated entry
mbx schema --gotchas coupons       # filtered to one table

# v0.4 Wave 5: one-call card metadata (fields + database + table), with a
# plain-card fallback if query_metadata 404s
mbx cards metadata orders-by-product

# v0.4 Wave 5: a non-numeric positional id is rejected client-side, exit 3 —
# no request is sent (SEC-002)
mbx databases get "1;DROP"; echo "exit=$?"   # exit=3, no GET issued

# v0.4 Wave 5: config add / LoadCredentials both enforce the https-or-local
# scheme guard now (SEC-001) — this used to only fire on `config add`
mbx config add x --url ftp://metabase.example --api-key mb_...; echo "exit=$?"   # exit=3
```

## Top-level command groups

| Group | Purpose |
|---|---|
| `config` | `doctor` (`--json`, `--require`), `show`, `path`, `add` (v0.4) — verify auth, report key permissions, provision a named project |
| `query` | execute native SQL against any database (default DB 2); `--csv` streams the real export endpoint (uncapped) |
| `schema` | INFORMATION_SCHEMA shortcut + ordini gotcha note (DB 2 only); **`--gotchas [table]`** (v0.4 Wave 5) prints the curated `(db, table, column) -> hint` table instead — no network call |
| `databases` | list / get (id validated as a bare non-negative integer, v0.4 Wave 5) |
| `tables` | list (filtered by `--database`) / get (returns full field metadata; id validated, v0.4 Wave 5) |
| `cards` | list / get / execute (`--param`) / export (`--param`) / dashboards / revisions / **create** (v0.4, gated) / **metadata** (v0.4 Wave 5 — query_metadata with plain-card fallback) (alias-aware); `execute` failures get an unreachable-DB hint when the card's own database also probes dead (v0.4) |
| `dashboards` | list / get / cards / related / revisions / **create** (gated) / **add-card** (v0.4, gated — mandatory read-modify-write) / **subscriptions** (v0.4, read-only); `get`/`cards`/`related`/`revisions`/`add-card`/`subscriptions` all validate their id args (v0.4 Wave 5) |
| `activity` | recent — query-execution history via Metabase's application database (when exposed as a data source); `--since` validated against an allowlist regex (v0.4 Wave 5, was denylist character-stripping) |
| `collections` | list / items (id validated, v0.4 Wave 5) |
| `search` | content search across cards / dashboards / collections / models |
| `users` | list |
| `aliases` | list / sync / **set** (v0.4, manual local register, no network) — user-controlled card-name → ID mappings (v0.3: parallel sync — 5× faster) |
| `playground` | build a native-query playground URL with the SQL pre-filled |
| `overview` | parallel snapshot — DBs + per-DB health (v0.4) + named cards + auth (<2 s); v0.3: HTTP/2 multiplexed |
| `ordini-kpi` | canonical ordini KPIs (v0.3) — order_count, unique_buyers, cancelled, latest, as_of, `gmv` (v0.4), window |

## Global flags

| Flag | Default | Notes |
|---|---|---|
| `--url` | env / config | overrides `METABASE_URL` |
| `--api-key` | env / config | overrides `METABASE_API_KEY` |
| `--project` | `default_project` | named project from `~/.config/mbx/config.toml` |
| `--database` | `2` (or `MBX_DEFAULT_DB`) | `prod`\|`clickhouse`\|`ops`\|`internal`\|`<numeric id>` for `query`/`schema`/`tables list`/`playground`/`ordini-kpi` (v0.4). Named aliases resolve live against `GET /api/database` — see [Database aliases](#database-aliases) |
| `--json` | (auto) | force JSON output (default on pipe) |
| `--jq <expr>` | none | gjson filter (NOT real jq — see below) |
| `--yes` | false | required for write commands (`dashboards create`/`add-card`, `cards create`); `--confirm` is an alias (same var), `MBX_CONFIRM=1` env also works |
| `--dry-run` | false | preview a write command's request (method/path/body) without sending it — exits 0, no `--yes` needed |
| `--timeout` | `60s` | HTTP timeout (Metabase queries can be slow) |
| `--verbose` | false | log HTTP requests to stderr |
| `--compact` | false | force compact JSON output (auto when `CLAUDECODE=1`) |
| `--timing` | false | log per-call wall + server `running_time` to stderr |
| `--rows` | 0 | agent/client row cap on `mbx query` (mbx-side; auto-100 when `CLAUDECODE=1`); `--rows 0` disables. Independent of Metabase's own 2,000-row server display cap — see [Row caps](#row-caps) |

## Row caps

`mbx query` can hit two independent, unrelated caps — the simplified JSON output distinguishes them via `row_cap_source`:

| Source | What it is | Trigger | Escape hatch |
|---|---|---|---|
| `client` | mbx's own `--rows` flag (or the agent-mode default of 100) | `--rows N` was set (or `CLAUDECODE=1`) and the result hit `N` | `--rows 0` |
| `server` | Metabase's own display-time truncation on unconstrained `POST /api/dataset` queries | no client cap applied (`--rows 0` / unset outside agent mode) and the result came back at exactly 2,000 rows | `--csv` (real export, uncapped), `LIMIT`/`OFFSET` pagination, or aggregation |

The server-side marker is **heuristic and intentionally pessimistic**: a genuine 2,000-row result is indistinguishable from a truncated one, so mbx flags both and the stderr warning says "may be server-truncated" rather than asserting it. `--csv` bypasses the server cap entirely by routing through Metabase's `/api/dataset/csv` export endpoint (ceiling ~1,048,575 rows) instead of the display-constrained query path — see the v0.4 recipes above.

## Authentication

Resolution order, **per field independently** (first non-empty wins):

1. `--url` / `--api-key` flags
2. `METABASE_URL` / `METABASE_API_KEY` env vars
3. Project block in `~/.config/mbx/config.toml` (named via `--project` or `default_project`)

For mutating commands (`dashboards create`/`add-card`, `cards create`), `mbx` will prefer the `MBX_WRITE_API_KEY` env var if set, falling back to the read key. This lets you provision a separate higher-privilege key for writes. A refused write (no `--yes`/`--confirm`/`MBX_CONFIRM=1`) exits **6** (`write_locked`) — see [Exit codes](#exit-codes) below.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | API or runtime error (network, timeout, 5xx, decode failure) |
| 2 | auth error (401/403, or missing credentials) |
| 3 | validation — a Metabase 400, OR (v0.4 Wave 5) a client-side rejection before any request is sent: a non-numeric id (`kind: "validation"`) or a URL scheme guard failure (`kind: "validation"`, `*config.InvalidURLSchemeError`) |
| 4 | not found (404) |
| 5 | rate limited (429) |
| 6 | write refused — `--yes`/`--confirm`/`MBX_CONFIRM=1` not given (v0.4; was exit 1 before the confirm-gate rebuild on `clicore/confirm`) |

Fleet-canonical table (`clicore/cierrors.ExitCodeFor`), same scheme as `kv`/`stx`/`otx`. **v0.4 Wave 5** added two purely client-side sources of exit 3 that never touch the network: `mbx databases get "1;DROP"` (or any non-numeric positional id on a validated command) and `mbx config add x --url ftp://evil` (or any credential-resolution path — flag/env/config — hitting a non-`https://`/non-local URL).

### Multi-project config

```toml
default_project = "production"

[projects.production]
url = "https://metabase.example.com"
api_key = "mb_..."
write_api_key = "mb_..."   # optional
default_db = 2

[projects.staging]
url = "https://metabase-staging.example.com"
api_key = "mb_..."
default_db = 2
```

## gjson `--jq` filters

Not real jq. Examples:

| Expression | Meaning |
|---|---|
| `0.id` | first element's `id` |
| `#.name` | array of every element's `name` |
| `#.{id:id,name:name}` | project per element |
| `#(archived==false)#` | filter array elements |
| `data.rows.0.0` | first row, first column of a query result |

## Database aliases

`--database` takes a numeric Metabase database id or one of four named aliases: `prod`, `ops`, `clickhouse`, `internal`. Run `mbx databases list` to see the ids and names on your instance.

**Named aliases are resolved LIVE against `GET /api/database`** (matched on engine + an anchored name pattern), never hardcoded to an id — so when a database is retired or re-created under a new id, the alias keeps working (or fails closed with "alias not found; run 'mbx databases list'" plus the live list, rather than silently resolving to a stale id). Every resolved `query`/`schema`/`tables list`/`playground`/`ordini-kpi` invocation, and `cards execute`, echoes `database: <id> (<name>)` to stderr — numeric ids echo bare (no extra round-trip).

The name patterns live in `dbAliasPatterns` (`internal/commands/dbalias.go`) and match the naming of the deployment mbx was built for; adjust them to your own database names. For example, given this listing:

| ID | Name | Engine |
|---|---|---|
| 2 | Example Replica | MySQL 8 |
| 7 | Example Analytics | ClickHouse |

an alias whose pattern is `{engine: "mysql", name: ^example\s+replica$}` resolves to `2`, and one with `{engine: "clickhouse", name: ^example\s+analytics$}` resolves to `7`. If database 7 is later dropped, that alias errors out instead of guessing.

## Schema gotchas (DB 2)

`mbx query`/`mbx schema` warn on a curated `(db, table, column) -> hint` table (v0.4 Wave 5 — `internal/commands/gotchas.go`, one source of truth shared by the SQL-lint stderr warning, the `unknown_column` error-envelope hint, and `mbx schema --gotchas`). Run `mbx schema --gotchas` for the full list with citations, or `mbx schema --gotchas <table>` to filter. Every entry was verified live (schema inspection or a live `COUNT(*)`) on the date noted in its hint text — curated means true, not generated. Current entries:

- **`ordini.stato`** — does NOT exist; the actual column is `state` (varchar, English values). Verified 2026-05-08.
- **`ordini.totale_ordine`** — does NOT exist. Revenue isn't directly stored on `ordini`; use `mbx ordini-kpi`'s `gmv` field, or `SUM(op.prezzo * op.quantita)` via `ordine_prodotti` joined on `ordini.id`, windowed on `customer_confirm_at` (placed orders — a `created_at` window over-counts ~2x). The old `JSON_EXTRACT` on `google_analytics_measures` recipe is **dead** (v0.4) — that table has since been dropped.
- **`prodotti.nome`** — exists, but is ~87% NULL. The actually-populated product-name column is `name` (100% filled) — the Italian-named column is the sparse one. Verified 2026-07-20.
- **`prodotti.prezzo_min`** (and `prezzo_max`/`prezzo_web`) — ~97-99% NULL or zero. Not a reliable current-price signal. Verified 2026-07-20.
- **`pharmacy_product_listings.price`** (and `quantity`) — do NOT exist. The table is a lean pharmacy × minsan `fulfillment_type` mapping only (`id`, `pharmacy_code`, `minsan`, `fulfillment_type`, `created_at`, `updated_at`). Verified 2026-07-20.
- **`coupons.status`** — exists, but `status = 1` alone does NOT mean a coupon is live: 99.5% of `status=1` rows have `published=false`. `published` and `visibility` gate usability separately. Verified 2026-07-20.

## Sandbox

When mbx runs under an agent sandbox (e.g. Claude Code's Bash tool), your Metabase host is probably outside the sandbox's default network allowlist, so the Bash invocation needs `dangerouslyDisableSandbox: true` (or the host added to the allowlist).

**v0.4:** forgetting this shows up as a raw connection failure — `kind: "network"` in the error envelope. When the target host is the one hardcoded in `sandboxAgentHost` (`internal/commands/network_hint.go` — set it to your own host), the envelope's `hint` names `dangerouslyDisableSandbox` explicitly. `mbx config doctor` gives the same diagnosis up front (it probes `/api/health` before checking auth), so it is also the fastest way to confirm whether a failure is "forgot the sandbox flag" vs. a real credentials problem.

## License

MIT — see [LICENSE](LICENSE).

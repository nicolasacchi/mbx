# CLAUDE.md — mbx

Go CLI for Metabase. Single binary, JSON output, X-API-Key auth. **37 leaf commands across 15 top-level groups** (`mbx --help` is the source of truth for this count; v0.4 complete — Waves 1-5). Opinionated `--database 2` default for the production replica, dynamic card aliases, HTTP/2 multiplexed transport, structured JSON error envelope (now classifying network/timeout failures too), agent-mode row cap, a write surface (`cards create`, `dashboards create`/`add-card`) gated by `--yes`/`--dry-run` (fleet-shared `clicore/confirm`, refusal exits 6), a curated schema-gotcha table shared by the SQL lint and the error envelope (`schema --gotchas`), and hardened input handling (every positional id validated numeric-only before it's spliced into a request; the https-or-localhost URL-scheme guard covers every credential-resolution path, not just `config add`). Designed for AI agent integration.

**Dead databases**: since v0.5.0 `mbx overview` reports a database whose `SELECT 1` probe fails as `ok:false` / `database_unreachable` (the probe inspects the in-body failure of Metabase's HTTP-202 response, not just the status code). The `funnel` and `cr-desktop` seed aliases and the `clickhouse` DB alias target ClickHouse databases; if yours are gone, prefer `--database prod`.

**API**: Metabase REST API. Base URL from config (e.g. `https://metabase.example.com`).

## v0.4 highlights for agents

| Feature | Behavior |
|---|---|
| `CLAUDECODE=1` env detection | Auto-enables compact JSON + default `--rows 100` on `mbx query`. Set by Claude Code. |
| Structured error envelope | First line of stderr on errors is JSON: `{"error":{"kind":"unknown_column","table":"ordini","column":"stato","hint":"…","db":2}}`. Dispatch on `kind` for retry strategy. `kind` now includes `timeout` and `network` (v0.4) — transport-layer failures (dial errors, DNS misses, TLS handshake failures, client timeouts) used to fall through to `client_error` with no signal at all. `--csv` failures go through the same envelope now too (v0.4), including the common case where Metabase's export endpoint responds `200 OK` with a JSON error body instead of real CSV on a bad query — this used to silently stream the JSON to stdout as if it were CSV data and exit `0`. |
| Sandbox hint (v0.4) | A `network`-kind envelope against the built-in sandbox host (`sandboxAgentHost` in `internal/commands/network_hint.go`) carries a `hint` naming `dangerouslyDisableSandbox` — a forgotten sandbox flag is the most common cause of a raw connection failure against a host outside the agent sandbox's allowlist. `mbx config doctor` gives the same triage: it probes the unauthenticated `/api/health` endpoint first, so a pure connectivity failure reports `kind: "network"`/exit 1 instead of being misdiagnosed as `auth_failed`. |
| Per-database health (v0.4) | `mbx overview`'s `database_health` array runs a bounded-parallel `SELECT 1` against every listed database and reports `ok`/`kind: "database_unreachable"` per DB — distinguishes "this DB is down" from "no data". `cards execute` does the same probe on failure only: if the card's own database is also unreachable, the error gains a `"card's database_id=N unreachable — MySQL KPIs still available via ordini-kpi"` hint. |
| `mbx ordini-kpi --window 24h` | Canonical KPIs without hand-coding SQL. Output: `{order_count, unique_buyers, cancelled, latest, as_of, gmv, window}`. `gmv` (v0.4) is `SUM(op.prezzo * op.quantita)` over PLACED orders (windowed on `customer_confirm_at`, not `created_at` — see [Schema Reality](#schema-reality-verified-2026-05-08)). Replaces the dead `google_analytics_measures` JSON_EXTRACT recipe (that table has since been dropped). |
| `--database prod\|clickhouse\|ops\|internal` (v0.4) | Named DB aliases, resolved live against `GET /api/database` (never hardcoded ids — a retired/renamed DB fails closed with "alias not found", not a stale guess). Every resolved query/schema/tables/playground/ordini-kpi/cards-execute invocation echoes `database: <id> (<name>)` to stderr. |
| `truncated: true` JSON marker | When `--rows N` cap was hit, the simplified output gets `truncated: true` + `row_cap: N` (survives `\| jq`). |
| HTTP/2 + connection prewarm | `overview` and other multi-call commands use one TLS connection (multiplexing); a single GET `/api/health` is fired on startup as a best-effort prewarm. |
| `--timing` | Stderr lines like `mbx: POST /api/dataset — server=3ms wall=607ms`. |
| `--compact` | Force compact JSON output even on TTY (auto when `CLAUDECODE=1`). |
| Write surface (v0.4) | `mbx cards create` (POST a native-SQL saved question) and `mbx dashboards add-card` (attach a card to a dashboard) join `dashboards create` as gated writes. `add-card` is a mandatory read-modify-write: `PUT /api/dashboard/:id` REPLACES the entire `dashcards` array, so mbx GETs the dashboard first, hard-asserts a `dashcards` key exists, and passes every existing dashcard through byte-for-byte (raw JSON, never decoded) before appending exactly one new entry — never constructs the array from scratch. |
| Confirm gate → exit 6 (v0.4) | The gate now delegates to the fleet-shared `clicore/confirm` package. `--yes` is the primary confirm flag (`--confirm` is now an alias — same underlying var — and `MBX_CONFIRM=1` still works); a refusal is `kind: "write_locked"`, exit **6** (was exit 1). `--dry-run` previews the exact request (method/path/body) without sending it — for `dashboards add-card` this still performs the GET (needed for an accurate preview) but skips the PUT. |
| `mbx dashboards subscriptions <id>` (v0.4) | Read-only: lists a dashboard's email/Slack subscriptions via `GET /api/pulse?dashboard_id=N` — `GET /api/dashboard/:id/subscriptions` does not exist (a prior research pass fabricated it). |
| `mbx schema --gotchas [table]` (v0.4 Wave 5) | Prints the curated `(db, table, column) -> hint` table (`internal/commands/gotchas.go`) — no network call. Same source of truth the `mbx query`/`mbx schema` SQL-lint stderr warning and the `unknown_column` error-envelope hint read from; a fix to a hint's wording, or a newly curated trap, updates all three surfaces at once. Entries today cover `ordini` (`stato`/`totale_ordine`), `prodotti` (`nome`/`prezzo_min`), `pharmacy_product_listings` (`price`/`quantity`), and `coupons` (`status`) — every one verified live on 2026-07-20 (or 2026-05-08 for the original `ordini` pair). |
| `mbx cards metadata <id\|alias>` (v0.4 Wave 5) | One-call "explain this card": `GET /api/card/:id/query_metadata` (fields + database + table metadata together), falling back to a plain `GET /api/card/:id` if that 404s (same shape as `tables get`'s `query_metadata`-then-plain fallback). Alias-aware via the same `resolveCardID` plumbing as `cards get`/`execute`. No confirm gate (read-only). |
| Input hardening (v0.4 Wave 5, SEC-001/2/3) | Every positional id on a read command that splices it into a URL path or query param (`databases get`, `tables get`, `collections items`, `dashboards get`/`cards`/`related`/`revisions`/`add-card`/`subscriptions`) is validated as a bare non-negative integer BEFORE any request is sent — reject, not sanitize; a path-traversal- or SQL-shaped id (`"1/../2"`, `"1 OR 1=1"`) fails with `kind: "validation"`, exit 3, zero network calls. The `https://`-or-`localhost` URL-scheme guard (`config add`, Wave 2) now also runs inside `LoadCredentials` itself, so a hand-edited `config.toml` or a stray `METABASE_URL=http://...` env var fails the same way instead of sailing through unchecked. `schema`'s table-name splice and `activity`'s `--since` interval splice were converted from denylist character-stripping to reject-not-strip allowlist regexes — stripping can silently change a query's meaning; rejecting surfaces the mistake. |

## Authentication

Resolution order (first non-empty wins, per field):

1. `--url` / `--api-key` flags
2. `METABASE_URL` / `METABASE_API_KEY` env vars
3. `~/.config/mbx/config.toml` — project from `--project` flag, then `default_project`

For write commands (`dashboards create`/`add-card`, `cards create`), `MBX_WRITE_API_KEY` is checked first. Falls back to `METABASE_API_KEY`.

### Multi-project config

```toml
default_project = "production"

[projects.production]
url = "https://metabase.example.com"
api_key = "mb_..."
write_api_key = "mb_..."   # optional, separate key for writes
default_db = 2
```

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--url` | — | METABASE_URL override |
| `--api-key` | — | METABASE_API_KEY override |
| `--project` | — | Named project from config |
| `--database` | project default (2) | `prod`\|`clickhouse`\|`ops`\|`internal`\|`<numeric id>` for `query`/`schema`/`tables list`/`playground`/`ordini-kpi`. Named aliases resolve live against `GET /api/database` (v0.4). |
| `--json` | false | Force JSON output even on TTY |
| `--jq` | — | gjson path filter (NOT real jq) |
| `--yes` | false | Required for write commands (v0.4: primary flag; `--confirm` is an alias — same var — `MBX_CONFIRM=1` also works) |
| `--dry-run` | false | Preview a write command's request (method/path/body) without sending it (v0.4) |
| `--timeout` | 60s | HTTP timeout |
| `--verbose` | false | Log HTTP requests to stderr |

## Commands

### config

```bash
mbx config doctor              # verify auth, report key permissions group
mbx config doctor --json       # one JSON object on stdout, no stderr preamble (v0.4)
mbx config doctor --require    # non-OK result -> typed error, non-zero exit (auth issues exit 2) (v0.4)
mbx config show                # print resolved creds (key redacted)
mbx config path                # print resolved config file path
mbx config add <name> --url <url> --api-key <key> [--write-api-key <key>] [--default-db <id>]  # (v0.4)
```

`config doctor` always prints one JSON object to stdout summarizing the result (`ok`, and on failure `kind`/`message`/`remediation` — e.g. `kind: "auth_missing"` for missing credentials). `--json` additionally suppresses the human-readable stderr preamble. `--require` promotes the permission-group probe (normally advisory) to a hard failure too, so scripts can gate purely on exit code.

`config doctor` probes the **unauthenticated** `/api/health` endpoint *before* the auth check (v0.4) — a connectivity failure (sandbox block, DNS miss, timeout) there reports `kind: "network"`/`"timeout"` (exit 1) instead of falling through to the `/api/user/current` check and being misdiagnosed as `auth_failed`; the credentials were never the problem. The same triage applies to the `--require`-gated permission-group probe: a network-class failure there now reports its real `kind`/exit 1, not a fabricated `not_authorized`/exit 2.

`config add` writes an atomic `[projects.<name>]` block; the first project added becomes `default_project`. `--url` must be `https://`, or `http://` for `localhost`/`127.0.0.1` only (SEC-001, landed early — Wave 2). **v0.4 Wave 5** moved the canonical scheme-check implementation into `internal/config` (`ValidateURLScheme`, `config add`'s check is now a thin wrapper around it) and wired it into `LoadCredentials` itself — so the guard now covers every credential-resolution path (`--url` flag, `METABASE_URL` env, a hand-edited `config.toml` project block), not just the one command that writes a new entry. A violation there returns a typed `*config.InvalidURLSchemeError` (`kind: "validation"`, exit 3).

**API**: `GET /api/health` (unauthenticated connectivity probe, v0.4), `GET /api/user/current` (auth check), `GET /api/permissions/group` (probe write capability).

### query

```bash
mbx query "<SQL>"                                   # default --database 2
mbx query "<SQL>" --database prod                   # live-resolved alias (v0.4); prefer it, ids churn
mbx query "<SQL>" --database 34                     # by numeric id
mbx query "<SQL>" --csv > out.csv                   # CSV output (RFC 4180)
mbx query "<SQL>" --raw                             # raw Metabase response (with .data.cols)
mbx query "<SQL>" --no-lint                         # suppress schema-gotcha warnings (v0.4 Wave 5: table-driven, not ordini-only — see 'mbx schema --gotchas')
```

Default response shape (simplified):

```json
{
  "columns": ["orders"],
  "rows": [[1234]],
  "row_count": 1,
  "running_ms": 23,
  "database_id": 2
}
```

**API**: `POST /api/dataset` with `{"database": <id>, "type": "native", "native": {"query": "..."}}`.

**Lint**: fires stderr warnings from the curated gotcha table (`internal/commands/gotchas.go`, v0.4 Wave 5 — see [`mbx schema --gotchas`](#schema)) whenever a query's `--database` and SQL match a curated `(db, table)` pair and reference the flagged column as a whole word (word-boundary matched, not substring — `u.cognome` does NOT fire the `prodotti.nome` warning). Originally `ordini`-only (`stato`/`totale_ordine`); now also covers `prodotti`, `pharmacy_product_listings`, `coupons`. Suppress with `--no-lint`.

### schema

```bash
mbx schema ordini              # INFORMATION_SCHEMA + ordini gotcha note (DB 2)
mbx schema users --database 4  # against another MySQL database
mbx schema --gotchas           # curated (db, table, column) -> hint table, no network call (v0.4 Wave 5)
mbx schema --gotchas coupons   # filtered to one table
```

**API**: same as `query` — runs `SELECT … FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = '<arg>'`. `--gotchas` makes no request at all — it renders `internal/commands/gotchas.go`'s in-binary table.

For ClickHouse (DB 35/133), errors out and points users to `mbx tables list/get`.

Table-name arg is validated against an identifier allowlist (`^[A-Za-z_][A-Za-z0-9_]*$`, v0.4 Wave 5) before being spliced into the `INFORMATION_SCHEMA` query — an invalid name is rejected with a clear error, not silently stripped down to something else (was a denylist character-stripper before Wave 5).

### databases

```bash
mbx databases list
mbx databases get 2
```

**API**: `GET /api/database`, `GET /api/database/:id`. Response is unwrapped from `{data: [...]}` envelope. `get`'s `<id>` is validated as a bare non-negative integer before being spliced into the path (v0.4 Wave 5, `kind: "validation"`/exit 3 on rejection, zero network calls) — e.g. `mbx databases get "1;DROP"` fails closed instead of sending a malformed request.

### tables

```bash
mbx tables list                       # filtered to default DB by default
mbx tables list --database ops        # by named alias
mbx tables list --database 0          # all DBs
mbx tables get 12345                  # full schema (columns, types) for table id
```

**API**: `GET /api/table` (bare array), `GET /api/table/:id/query_metadata` (preferred) with fallback to `GET /api/table/:id`. `get`'s `<id>` is validated numeric-only before either path is spliced (v0.4 Wave 5, same as `databases get`).

### cards

```bash
mbx cards list                                                  # all saved questions
mbx cards get 53                                                # by numeric ID
mbx cards get orders-by-product                                 # by alias
mbx cards execute orders-by-product                             # run the saved question
mbx cards execute funnel --param from_date=2026-05-01 \
                        --param to_date=2026-05-08              # parameterized (v0.2)
mbx cards export 53 --format csv --param from_date=2026-05-01   # parameterized export
mbx cards export 53 --format csv                                # csv | json | xlsx
mbx cards dashboards 53                                         # dashboards using this card
mbx cards revisions orders-by-product                           # revision history (v0.2)
mbx cards create --name "orders" --sql "SELECT 1" --database prod --yes         # write — gated (v0.4)
mbx cards create --name "orders" --sql "SELECT 1" --database prod --alias o     # + register a local alias
mbx cards create --name "orders" --sql "SELECT 1" --database prod --dry-run    # preview only
mbx cards metadata 53                                           # query_metadata, fallback to plain card (v0.4 Wave 5)
mbx cards metadata orders-by-product                            # by alias
```

Aliases live in `~/.config/mbx/aliases.toml` and resolve to `card_id` + `database_id`. Run `mbx aliases sync` to refresh. `execute` echoes the card's authoritative `database_id` to stderr the same way `--database` resolution does (v0.4) — `database: 2 (Example Replica)` — since a card silently pointed at the wrong DB was otherwise indistinguishable from "no data".

On execution failure, `execute` probes the card's own database with a cheap `SELECT 1` (v0.4) — if that also fails, the error gains a `"card's database_id=N unreachable — MySQL KPIs still available via ordini-kpi"` hint, so a dead database (one that has been down for days, say) doesn't read as "the card/SQL is broken". Probe only runs on the failure path — no extra round-trip when the query succeeds.

`--param slug=value` is repeatable. Slugs come from the card's `parameters[].slug`; `mbx cards get <id> --jq 'parameters'` to inspect. Unknown slug → useful error listing all available slugs.

`mbx cards create` (v0.4, W10a) POSTs a new native-SQL saved question. `--database` resolves through the same alias plumbing as `query`/`schema` (it's the persistent root flag) and echoes the resolved DB to stderr. Requires `--yes` (or `--confirm`/`MBX_CONFIRM=1`); `--dry-run` prints the would-be `POST /api/card` body without sending it. The request body is exactly `{name, display, visualization_settings: {}, dataset_query: {database, type: "native", native: {query}}}` plus optional `collection_id`/`description` — verified against the Metabase `v0.53.6.1` source (`api/card.clj`) and re-checked live on `v0.58.22`. With `--alias`, registers the new card in `~/.config/mbx/aliases.toml` (same `Set`/`Save` plumbing as `aliases sync`).

**Partial-success handling**: the card POST and the alias save are two separate steps — if the card is created but the alias save then fails (disk full, bad `XDG_CONFIG_HOME`, etc.), the card already exists in Metabase. The command still prints the created card's `id`/`name`/`database_id` to stdout via the normal output path (so an agent loop isn't left with no id to reference), then returns an error that explicitly says **do not retry create** (retrying would create a second card) and names the recovery command: `mbx aliases set <alias> --card-id <id> --database-id <id>` (see [aliases](#aliases)).

`mbx cards metadata <id|alias>` (v0.4 Wave 5, W13) is a one-call "explain this card" shortcut: `GET /api/card/:id/query_metadata` returns field/database/table metadata together — up to three separate calls otherwise — falling back to plain `GET /api/card/:id` if that 404s (`query_metadata` was added in Metabase v0.51; the fallback keeps this working against an older/misconfigured instance too, same "richer endpoint, fall back" shape as `tables get`). Read-only, no confirm gate.

**API**: `GET /api/card`, `GET /api/card/:id`, `GET /api/card/:id/query_metadata` (v0.4 Wave 5, `metadata`), `POST /api/card/:id/query`, `POST /api/card/:id/query/:format`, `GET /api/card/:id/dashboards`, `GET /api/revision?entity=card&id=:id`, `POST /api/card` (v0.4, write).

### dashboards

```bash
mbx dashboards list
mbx dashboards get 100
mbx dashboards cards 100              # cards on a dashboard
mbx dashboards related 100            # related dashboards
mbx dashboards revisions 100          # revision history
mbx dashboards create --name "Foo" --description "Bar" --yes         # write — gated
mbx dashboards add-card 100 53 --yes                                 # attach card 53 (v0.4, write — gated)
mbx dashboards add-card 100 orders-by-product --row 4 --yes          # explicit row, card by alias
mbx dashboards add-card 100 53 --dry-run                             # preview only (still GETs, no PUT)
mbx dashboards subscriptions 100                                     # email/Slack subscriptions (v0.4, read-only)
```

`mbx dashboards create`/`add-card` are gated writes. Refuse without `--yes` (or `--confirm`/`MBX_CONFIRM=1`) — exit **6** (`write_locked`, v0.4; was exit 1). Uses `MBX_WRITE_API_KEY` if set, otherwise falls back to `METABASE_API_KEY`. `--dry-run` previews the request without sending it.

`get`/`cards`/`related`/`revisions`/`add-card` (its `<dashboard>` arg)/`subscriptions` all validate their id/`<dashboard>` argument as a bare non-negative integer before splicing it into a path or query param (v0.4 Wave 5, `kind: "validation"`/exit 3, zero network calls on rejection) — `<card>` on `add-card` is exempt from this specific check since it legitimately accepts a non-numeric local alias (resolved through the existing `resolveCardID` plumbing, which already only ever produces a validated int either way).

`mbx dashboards add-card <dashboard> <card>` (v0.4, W10b) attaches an existing card to a dashboard. `<card>` accepts a numeric id or a local alias. **This is the dangerous write in the fleet**: `PUT /api/dashboard/:id` has full-replacement semantics for the `dashcards` array — a PUT body that omits an existing dashcard *deletes* it. mbx therefore does a mandatory read-modify-write: `GET /api/dashboard/:id` first, hard-asserts the response has a `dashcards` key (aborts with no PUT otherwise, same for a failed GET), passes every existing dashcard through as raw JSON (never decoded into a typed struct — this is what preserves fields like `parameter_mappings`/`series`/`action_id` byte-for-byte), appends exactly one new dashcard (`{id: -1, card_id, row, col, size_x, size_y, parameter_mappings: [], series: []}`), and PUTs the whole thing back. Without `--row`, the new card auto-places below every existing one (`row = max(existing row + size_y)`, 0 for an empty dashboard). If the dashboard has tabs, the new card targets the first tab unless `--tab <id>` names another (aborts listing valid tab ids if `--tab` doesn't match). A safety-belt assertion (`len(new dashcards) == len(existing)+1`) runs immediately before the PUT. `<dashboard>` is validated numeric-only first (v0.4 Wave 5), then a plain refusal (no `--yes`, no `--dry-run`) is checked **before** the GET, so an invalid-id or unconfirmed invocation makes zero network calls; `--dry-run` still needs the GET for an accurate preview.

**Not atomic — this is a time-of-check/time-of-use gap, not something id validation touches.** The read-modify-write has no locking: a concurrent `add-card` run, or a human editing the dashboard in the Metabase UI, between this command's GET and PUT gets silently overwritten by the PUT (last write wins). Don't run concurrent `add-card`s against the same dashboard. (v0.4 Wave 5's id-validation and scheme-guard hardening are orthogonal to this — they reject malformed *input* before any request, they don't add locking around the GET-then-PUT window itself.)

`mbx dashboards subscriptions <id>` (v0.4, W12) is read-only: `GET /api/pulse?dashboard_id=N` — **not** `GET /api/dashboard/:id/subscriptions`, which does not exist (a prior research pass fabricated it). Renders per-subscription name plus per-channel `channel_type`/schedule (`schedule_type`/`hour`/`day`/`frame` — these fields live on the channel, not the pulse root)/recipients (email channels) or `slack_channel` (Slack). Empty list → `[]`, exit 0. The pulse namespace is deprecated upstream (Metabase v0.54 moved alerts to `/api/notification`); `subscriptions` was verified working on `v0.58.22` (2026-11-12) — re-test before the next Metabase upgrade and plan the move to `/api/notification`.

**API**: `GET /api/dashboard`, `GET /api/dashboard/:id`, `GET /api/dashboard/:id/related`, `GET /api/dashboard/:id/revisions`, `POST /api/dashboard`, `PUT /api/dashboard/:id` (v0.4, write — `add-card`'s read-modify-write target; stable through v0.62.5, unlike the deprecated `PUT /:id/cards`), `GET /api/pulse?dashboard_id=N` (v0.4, `subscriptions`).

### collections

```bash
mbx collections list
mbx collections items 3               # items in collection 3
```

**API**: `GET /api/collection`, `GET /api/collection/:id/items`. `items`'s `<id>` is validated numeric-only before being spliced into the path (v0.4 Wave 5, same as `databases get`).

### search

```bash
mbx search "ordini"                                       # all models
mbx search "Funnel" --models card                         # cards only
mbx search "Funnel" --models card,dashboard --limit 10
```

**API**: `GET /api/search?q=<q>&models=<csv>&limit=<N>`.

### users

```bash
mbx users list
```

**API**: `GET /api/user`. Read-only by design — no create/edit/delete in v0.

### aliases

```bash
mbx aliases list                  # current aliases (alphabetical by alias name)
mbx aliases sync                  # refresh from /api/search → ~/.config/mbx/aliases.toml
mbx aliases set <alias> --card-id <id> [--database-id <id>] [--name <name>]   # manual register (v0.4)
```

`aliases set` (v0.4) writes `alias -> card_id` directly, no network call, bypassing `sync`'s search-based resolution. Its main use is recovering from `cards create --alias` succeeding at the card POST but failing to save the alias — see [cards](#cards).

Seed names (resolved by search query, not hardcoded card IDs — `aliasSeeds` in `internal/commands/aliases.go`; edit them to match saved questions on your instance):

| Alias | Search query |
|---|---|
| `orders-by-product` | "Order Distributions per ProductType" |
| `minsan-by-product` | "Minsan Distributions per ProductType" |
| `funnel` | "Website Funnel Trend" (ClickHouse-backed) |
| `cr-desktop` | "CR Desktop" (ClickHouse-backed) |

`gmv-by-product` was removed from the seed list (v0.4): the card queries the dropped `google_analytics_measures` table and hard-fails live. Use `mbx ordini-kpi`'s `gmv` field instead. A pre-existing local `aliases.toml` may still carry a stale `gmv-by-product` entry from an earlier sync — `mbx aliases sync` no longer re-seeds it but doesn't remove it either; hand-edit the file to drop it.

If a card is renamed/archived in Metabase, `mbx aliases sync` will re-resolve to the new best match (or skip with a stderr warning if no match).

Alias keys are slugified (lowercase, hyphenated). Edit `~/.config/mbx/aliases.toml` directly to add custom aliases.

**API**: `GET /api/search?q=<seed-query>&models=card`, then `GET /api/card/:id` for authoritative `database_id`.

### playground

```bash
mbx playground "SELECT 1"                          # default DB 2
mbx playground "SELECT 1" --database ops           # by named alias
```

Prints a Metabase native-query playground URL with the SQL pre-encoded. Pipe to `xdg-open`/`open`/`pbcopy` to use.

### activity (v0.2)

```bash
mbx activity recent                                    # last 25 query executions
mbx activity recent --since "24 hours" --limit 100
mbx activity recent --jq '#.{when:0,ms:1,rows:2,user:10,card:9}'
```

The Metabase REST `/api/activity` endpoint was removed in newer versions. mbx instead reads query-execution history from Metabase's own application database (Postgres), which only works if your instance exposes that database as a Metabase data source. Its database id is a compile-time constant (`auditDB` in `internal/commands/activity.go`) — set it to the id on your instance.

Returns rows with: `started_at`, `running_time` (ms), `result_rows`, `context`, `error`, `cache_hit`, `card_id`, `dashboard_id`, `database_id`, `card_name`, `executor_email`, `database_name`.

**API**: `POST /api/dataset` against the `auditDB` database with native SQL (Postgres).

TTY table columns: `WHEN`/`MS`/`ROWS`/`CONTEXT`/`ERROR`/`CARD`/`USER`/`DB` (v0.4 — before this the TTY table hard-failed with `no array at "@this"` on every invocation, since the column map expected the old, long-removed `/api/activity` REST shape; any future column-map drift on any command now degrades soft to JSON with a stderr note instead of failing the command).

`--since` is validated against an allowlist regex (`^[0-9a-zA-Z ]+$`, v0.4 Wave 5) before being spliced into the Postgres `INTERVAL` literal — an invalid value is rejected with a clear error, not silently stripped down to something else (was a denylist character-stripper before Wave 5).

### overview

```bash
mbx overview                  # parallel: DB list + per-DB health + named cards + auth (<2 s)
mbx overview --deep            # also round-trip-execute every named card (5-30 s)
```

`database_health` (v0.4) is a bounded-parallel (4 at a time) `SELECT 1` liveness probe against every database `GET /api/database` lists — each entry carries `id`/`name`/`engine`/`ok`, plus `kind: "database_unreachable"` and `error` on failure. Distinguishes "this DB is down" from "no data" (without it, a database that stays down for days looks the same as an empty result).

**API**: parallel goroutines:
- `GET /api/database`, then a bounded-parallel `POST /api/dataset` (`SELECT 1`, ~3s timeout) per database (v0.4)
- `GET /api/user/current`
- For each alias in `aliases.toml`: `GET /api/card/:id` (cheap metadata — last_query_start, average_query_time, archived, database_id)
- With `--deep`: also `POST /api/card/:id/query`

## Output Conventions

- **TTY**: table when a column layout is registered for the command (databases, tables, cards, dashboards, collections, users, search, aliases, `schema.gotchas` (v0.4 Wave 5), `activity.recent`). Otherwise JSON.
- **Pipe / `--json`**: JSON.
- **`--jq <expr>`**: applied on the JSON path; gjson syntax (NOT real jq).

## Exit Codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | API or runtime error (network, timeout, 5xx, decode failure) |
| 2 | auth error (401/403, or missing credentials — `kind: "auth_missing"`, v0.4) |
| 3 | validation — a Metabase 400, OR (v0.4 Wave 5) a client-side rejection before any request is sent: `validateID` on a malformed positional id (`kind: "validation"`, `*commands.validationError`), or `ValidateURLScheme` on a non-`https://`/non-local URL (`kind: "validation"`, `*config.InvalidURLSchemeError`) |
| 4 | not found (404) |
| 5 | rate limited (429) |
| 6 | write refused — `--yes`/`--confirm`/`MBX_CONFIRM=1` not given (`kind: "write_locked"`, v0.4) |

Fleet-canonical table (`clicore/cierrors.ExitCodeFor`). mbx previously exited `1` for every failure (no `ExitCode()`); it now emits the canonical 2/3/4/5/6. **v0.4:** missing-credentials failures (`ErrNoCredentials`) were a bare untyped sentinel exiting `1`; now a typed `NoCredentialsError` (`Kind()` = `"auth_missing"`) exits `2` like any other auth failure. **v0.4 (W7):** transport-layer failures (dial errors, DNS misses, TLS handshake failures, client timeouts) are now typed too — `kind: "timeout"` or `kind: "network"` in the error envelope — but still exit `1`, same as any other non-HTTP-status error; this doesn't add a new exit code, it just makes the existing `1` classifiable. **v0.4 (Wave 4):** the confirm gate was rebuilt on the fleet-shared `clicore/confirm` package — a refusal is now a `*cierrors.APIError{Kind:"write_locked"}` (a different concrete type than mbx's own `*client.APIError`, but the same `errors.As(err, &interface{ ExitCode() int })` probe in `main.go` picks it up with no dispatch-code changes) and exits **6**, not the old bare exit `1`. **v0.4 (Wave 5):** exit `3` gained two purely client-side sources that were previously either untyped (exit 1) or not checked at all: a non-numeric positional id (`mbx databases get "1;DROP"`) is now rejected before any network call, and a non-`https://`/non-local URL reaching `LoadCredentials` from ANY resolution path (not just `config add`) is rejected the same way.

A write refusal (no `--yes`/`--confirm`/`MBX_CONFIRM=1`) exits **6**. `--dry-run` bypasses the confirm requirement entirely and exits `0` after printing the would-be request.

## Schema Reality (verified 2026-05-08; extended 2026-07-20)

`ordini.stato` and `ordini.totale_ordine` don't exist. Additional findings from `mbx schema ordini`:

- The actual order-status column on `ordini` is `state` (varchar) with English values.
- Lifecycle timestamps live in dedicated columns (`pagato_il`, `spedito_il`, `confermato_il`, `annullato_il`, `customer_confirm_at`, etc.).
- No revenue column. Use `mbx ordini-kpi`'s `gmv` field, or `SUM(op.prezzo * op.quantita)` via `ordine_prodotti` joined on `ordini.id`, windowed on `customer_confirm_at` (placed orders — a `created_at` window over-counts ~2x). The old `JSON_EXTRACT(google_analytics_measures.request_data, ...)` recipe is **dead**: that table has since been dropped.

**v0.4 Wave 5** widened this from an `ordini`-only hardcoded check into a curated table covering three more tables (`internal/commands/gotchas.go` — run `mbx schema --gotchas` for the full list, always up to date):

- `prodotti.nome` exists but is ~87% NULL — the populated name column is `name` (100% filled), the opposite of what the Italian-named column suggests.
- `prodotti.prezzo_min`/`prezzo_max`/`prezzo_web` are ~97-99% NULL or zero — none is a reliable current-price signal.
- `pharmacy_product_listings` has no `price`/`quantity` column at all — it's a lean `id`/`pharmacy_code`/`minsan`/`fulfillment_type`/`created_at`/`updated_at` mapping only.
- `coupons.status = 1` alone does NOT mean a coupon is live — 99.5% of `status=1` rows have `published=false`; `published` and `visibility` gate usability separately.

## Sandbox

Under an agent sandbox (e.g. Claude Code's Bash tool), the Metabase host is usually outside the default network allowlist: run `mbx` with `dangerouslyDisableSandbox: true` or add the host to the allowlist. The `network`-kind sandbox hint only fires for the host hardcoded in `sandboxAgentHost` (`internal/commands/network_hint.go`).

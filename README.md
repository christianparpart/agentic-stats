# agentic-stats

[![CI](https://github.com/christianparpart/agentic-stats/actions/workflows/ci.yml/badge.svg)](https://github.com/christianparpart/agentic-stats/actions/workflows/ci.yml)

Fleet-wide analytics and a durable archive for AI coding assistants.

You use Claude Code on a laptop, a desktop and a couple of VMs. Each machine keeps a
detailed record of that work — token counts, models, git branches, every file edit as a
unified diff, tool timings — **locally, and only for about 30 days.** Then it is deleted.

`agentic-stats` runs a small daemon on each machine that ships those logs to a server you
control, so the history survives, and gives you a dashboard over the whole fleet.

## Why

- **The logs are deleted on a rolling window.** Anything older than the retention period is
  gone. There is no export and no server-side copy of the local transcripts.
- **Per-machine tools cannot see the whole picture.** Existing analyzers read one machine's
  directory. If you work across several, none of them can answer "how much did I code this
  year".
- **The data is genuinely rich** — exact per-request token accounting by type, git branch
  per line, diffs of every edit, and records that bind a session directly to the pull
  request it produced.

## What you get

- **A durable archive.** Raw transcript lines, stored verbatim. Every derived table is a
  cache that can be dropped and rebuilt, so metrics invented years from now still apply to
  today's data.
- **Cost.** API-equivalent cost from real token counts against a versioned price table,
  including the cache-read/cache-write split — and what prompt caching actually saved you.
- **Time.** Active coding time, busy hours, busy days, streaks, trends by day/week/month/year.
- **Projects.** What you actually worked on over time, across every machine.
- **Delivery.** Sessions joined to pull requests and issues: cost per merged PR, lead time,
  AI-written lines versus total.
- **Fleet health.** Which machines are reporting, and whether the archive has gaps.

## Status

Working end to end: the collector discovers transcripts, ships them to the server, and the
server stores, summarises and displays them. The dashboard is served at `/` from the server
binary -- one file, nothing to deploy alongside it.

## Quickstart

Requires Go and a PostgreSQL 17 you can reach.

```sh
# 1. Prepare the database. The application role must NOT be a superuser:
#    superusers bypass row-level security, which is what isolates tenants.
./scripts/setup-test-db.sh "postgres://postgres@localhost:5432/agentic_stats?sslmode=disable"
export AGENTIC_STATS_DATABASE_URL="postgres://agentic_app:agentic_app@localhost:5432/agentic_stats?sslmode=disable"

# 2. Build, create a user, and start the server (migrations run automatically).
make build
AGENTIC_STATS_PASSWORD=... ./bin/server create-user --email you@example.com --admin
./bin/server serve --addr 127.0.0.1:8080

# 3. Enroll this machine and collect.
./bin/server enroll-code --email you@example.com          # prints a one-time code
./bin/agent enroll --server http://127.0.0.1:8080 --code <code>
./bin/agent once                                          # or: ./bin/agent run

# 4. Open http://127.0.0.1:8080/ and sign in, or read the JSON directly.
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/v1/summary
```

The dashboard shows cost, cache savings, the cache hit rate, cost per active day and a
per-model breakdown. `/v1/summary` and `/v1/daily` return the same figures as JSON, and
accept either a device token or a browser session -- one API, not two. Ingest stays
device-only: a browser session can read the archive but never write to it.

## Getting the numbers right

Claude Code writes one JSONL line per content block, and every line repeats the complete
usage object for the whole API response. Summing them naively over-counts, and by a different
factor per metric, so it cannot be corrected afterwards. Measured on a real archive:

| metric | naive sum | folded by `requestId` | over-count |
|---|---|---|---|
| output tokens | 13,095,501 | 5,249,480 | 2.49x |
| thinking tokens | 6,278,373 | 2,071,020 | 3.03x |
| cache reads | 4,477,680,713 | 2,347,702,854 | 1.91x |

The fold happens once, in `internal/derive`, and the `request_usage` uniqueness constraint
makes double-counting structurally impossible rather than a rule to remember.

## Design principles

1. **Getting bytes to safety outranks parsing them well.** Collection is simple and robust;
   analysis happens server-side over the archive and can be redone at any time.
2. **Raw lines are the source of truth.** Facts are derived, never authoritative.
3. **Parsers accumulate, never replaced.** Old formats stay readable forever.
4. **Nothing personal in this repository.** All paths, hosts and credentials are
   configuration. Test fixtures are synthetic. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Privacy

The daemon ships **full transcripts** by default — that is the point, since it doubles as
the backup for data your machines are about to delete. Transcripts contain your prompts and
your source code.

Run the server somewhere you control, keep it behind TLS and authentication, and use
`privacy.exclude_paths` to withhold projects that must never leave the machine. The server
is multi-tenant with per-user isolation enforced by Postgres row-level security.

## License

Apache-2.0

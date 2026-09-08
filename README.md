# agentic-stats

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

Early. See `docs/` for the design. Not yet usable.

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

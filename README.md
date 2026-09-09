# agentic-stats

[![CI](https://github.com/christianparpart/agentic-stats/actions/workflows/ci.yml/badge.svg)](https://github.com/christianparpart/agentic-stats/actions/workflows/ci.yml)

A peer-to-peer archive and analytics for AI coding assistants.

You use Claude Code on a laptop, a desktop and a couple of VMs. Each machine keeps a
detailed record of that work — token counts, models, git branches, every file edit as a
unified diff, tool timings — **locally, and only for about 30 days.** Then it is deleted.

`agentic-stats` runs one daemon on each machine. Daemons find each other on any network they
share, authenticate from a shared key, and sync both ways until **every machine holds
everything**. There is no server. Each node serves its own dashboard.

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

- **A durable archive, replicated.** Raw transcript lines, stored verbatim on every node.
  The more machines you have, the safer the data — losing one loses nothing.
- **Encrypted at rest.** Record bodies are sealed with a key derived from your mesh key, so
  a stolen laptop or a copied database yields nothing.
- **Cost.** API-equivalent cost from real token counts against a versioned price table,
  including the cache-read/cache-write split — and what prompt caching actually saved you.
- **Time.** Active coding time, busy hours, busy days, streaks, trends by day/week/month/year.
- **Projects.** What you actually worked on over time, across every machine.
- **Delivery.** Cost per pull request, joined exactly: the assistant records the pull
  request each session opened, so this is not branch-name guessing. Where one session
  produced several, its cost is split evenly between them and labelled as an allocation.
- **Fleet health.** Which machines are reporting, and whether the archive has gaps.
- **No account system.** You join a mesh by holding its key. That is the whole membership
  model.

## Status

Working end to end. Nodes collect, discover each other, converge both ways, bridge through
external storage, and each serves its own dashboard.

## Quickstart

Requires only Go. No database server, no cloud account, no certificates.

```sh
make build

# First machine: generate a mesh key.
./bin/agentic-stats init          # prints the key -- keep it, there is no recovery

# Every other machine: adopt the same key.
./bin/agentic-stats join --key <key>

# Collect and serve.
./bin/agentic-stats run           # dashboard on http://127.0.0.1:8899
./bin/agentic-stats once          # or a single pass and exit
./bin/agentic-stats status        # what this node holds, and its peers
```

Nodes on the same network find each other automatically. For machines separated by a
WireGuard or Tailscale tunnel, list them under `mesh.peers` -- multicast cannot cross a
point-to-point link, so discovery genuinely cannot reach them. For machines that never share
a network at all, point both at the same folder:

```toml
[bridge]
kind = "filesystem"
path = "~/Dropbox/agentic-stats"
```

Any synced folder, NAS or SMB share works. Bundles written there are encrypted, including
their metadata, because the storage provider is not trusted.

Open the dashboard and sign in with the mesh key. It shows cost, cache savings, the cache
hit rate, cost per active day and a per-model breakdown; `/v1/summary` and `/v1/daily`
return the same figures as JSON.

The dashboard binds to loopback by default, where `http://127.0.0.1` is already a secure
context in every browser -- no certificate to manage and no warning. Binding it elsewhere
turns on HTTPS with a self-signed certificate.

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

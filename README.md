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

Requires only Go. No database server, no cloud account, no certificates, no root.

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

# Start automatically at login.
./bin/agentic-stats install       # launchd, systemd --user, or Task Scheduler
./bin/agentic-stats service       # is it installed and running?
./bin/agentic-stats uninstall     # remove; the archive is untouched
```

`install` registers a **user** service on all three platforms: the daemon reads one
person's home directory, so what runs is never root or an administrator.

Installing it is free of that on Linux and macOS — a systemd `--user` unit and a launchd
user agent both go in your own home directory. Windows is the exception: registering a
logon task writes to the root Task Scheduler folder, which is administrator-only. That is
a property of the folder, not of the daemon, and the task is still created with a limited
run level. `install` notices, explains itself, and asks Windows for consent once, so you
do not have to open a second elevated prompt and retype the command. Decline the prompt
and nothing is installed.

On Linux `install` also attempts `loginctl enable-linger`, without which a user service
stops the moment you log out — exactly wrong on a headless VM. If that fails it says so,
along with the command to fix it, rather than leaving you with a collector that only runs
while you are connected.

Once installed, the service is controlled without any further prompting:

```sh
agentic-stats start      # start it
agentic-stats stop       # stop it, leaving it installed
agentic-stats restart    # both
agentic-stats service    # is it installed, is it running
```

## Where a background node logs

A service started at login has no terminal, so its log goes to a file beside the archive
and configuration — `logs/agentic-stats.log`, rotated at 8 MB, three kept. Run the daemon
in a terminal and it prints there as well, because a log that vanishes when you run the
thing by hand is the wrong kind of quiet.

On Windows the service additionally writes to the Application event log, under the source
`agentic-stats`, registered by `install` while it is already elevated. **Only warnings and
errors go there.** Event Viewer is where someone looks to find out whether anything is
wrong, and a "pass complete" every poll interval would bury the entries that matter; the
full detail is in the file.

The Windows binary is linked for the GUI subsystem so the service runs without a console
window appearing at every login. The command line still prints normally — it reattaches to
whatever console launched it — with one visible consequence: a shell does not wait for a
GUI-subsystem program, so output can arrive just after your prompt returns.

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

The mesh key can be the generated one or any passphrase of at least 20 characters --
passphrases are stretched with argon2id, since a memorable phrase carries far less entropy
than its length suggests and this archive's ciphertext is exposed by design.

Open the dashboard and sign in with the mesh key. It shows cost, cache savings, the cache
hit rate, cost per active day and a per-model breakdown; `/v1/summary` and `/v1/daily`
return the same figures as JSON.

## Knowing it works

`agentic-stats status` and `/v1/health` answer the question that matters for an
archive: not whether peers are reachable, but whether data is actually moving. For
each peer they report when it last **converged**, how far **behind or ahead** this
node is as of that exchange, and why the last attempt failed if it did.

Last-seen is deliberately not the measure. A peer announcing itself every thirty
seconds while every exchange with it fails is the failure worth catching, and it
looks perfectly healthy by any other signal.

```sh
./bin/agentic-stats status         # per-peer convergence, and a warning if one has stalled
curl .../v1/health                 # the same, as JSON, with an overall ok/degraded verdict
curl .../healthz                   # liveness only, and deliberately uninformative
```

`/v1/health` needs the mesh key, because peer identities, addresses and lag amount
to a map of your fleet. `/healthz` does not, which is why it says nothing else.

A peer that is switched off is not a fault — that is a laptop in a bag — so it is
reported, not alarmed about. A node whose exchanges are failing, or that holds
quarantined records, reads as `degraded`.

## Surviving the network

The daemon assumes the network misbehaves, because on laptops and VMs it does:

- **Suspend and resume.** A wall-clock jump is treated as a resume: peer backoff is
  discarded and a sweep runs immediately, rather than waiting out a timer learned
  on a network the machine may no longer be attached to. Wall clock rather than
  monotonic because the three platforms disagree about whether monotonic time
  advances across a suspend.
- **Half-open connections.** Every read and write carries an idle deadline, and TCP
  keepalive is set explicitly rather than inherited, so a peer that vanished
  mid-exchange is reaped in about a minute instead of hours.
- **Subsystem failure.** Collection, the dashboard, peering and the bridge are
  supervised separately and restart with backoff. A mesh listener that cannot bind
  after resume no longer takes collection down with it. Genuine misconfiguration
  still exits immediately rather than looping.
- **A bad frame from a peer.** Contained to that exchange and logged with its stack,
  rather than ending the process that is holding your only off-machine copy.

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

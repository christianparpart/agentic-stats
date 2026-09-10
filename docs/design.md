# Design

## Goals

1. **Preserve.** Assistant transcripts are deleted locally on a rolling retention window
   (Claude Code defaults to 30 days). Get them somewhere durable before that happens.
2. **Aggregate.** One view across every machine.
3. **Explain.** Cost, time, projects, and delivery outcomes.

Preservation outranks the other two. A crude collector running today beats a sophisticated
one next month, because the data it would have collected no longer exists.

## Shape

Every node is the same program. There is no server and no special role.

```
   ┌────────────── node ──────────────┐        ┌───── node ─────┐
   │ collect  source adapters         │        │ collect …      │
   │ store    SQLite, bodies sealed   │◄──────►│ store …        │
   │ mesh     beacon · dial · sync    │  TLS   │ mesh …         │
   │ serve    dashboard               │  +PSK  │ serve …        │
   └──────────────────────────────────┘        └────────────────┘
              ▲                                        ▲
              └──── encrypted bundles ── store ────────┘
```

Records are immutable, append-only and never deleted, which makes the dataset a grow-only
set: convergence needs no merge rule and no conflict resolution. Anti-entropy is a version
vector — each record carries `(origin_id, seq)`, and sync is "send me everything above my
watermark per origin".

Membership is a pre-shared key. There are no accounts, invites or tenants: you are in the
mesh if you hold the key, and that same key seals every record and unlocks the dashboard.

## Parsing traps

These are properties of the Claude Code JSONL format that silently corrupt results if
missed. Each is covered by a synthetic fixture.

### 1. Usage is repeated on every content block

`message.content` holds exactly one block per JSONL line. A single API response containing
thinking + text + three tool calls is written as five lines, **each repeating the complete
`usage` object**, all sharing `message.id` and `requestId`.

Summing naively over-counts. Measured on a real corpus: output tokens **2.9×**, thinking
tokens **4.2×**, cache reads **1.9×** — and the inflation factor differs per metric, so it
cannot be corrected with a constant.

Fold by `requestId` before doing anything else. The `request_usage` table is unique on
`(user_id, request_id)`, which makes double-counting structurally impossible rather than a
rule someone has to remember. Lines with `model == "<synthetic>"` or `isApiErrorMessage`
carry a null `requestId` and all-zero usage; exclude them.

### 2. Subagent transcripts are separate files

`<sessionId>/subagents/agent-<id>.jsonl`, typically outnumbering top-level session files,
each with its own token usage. A parent-only scrape undercounts badly.

### 3. Session relocation duplicates whole files

Entering a git worktree copies the transcript into a different project directory and
continues appending there. The same `sessionId` and `uuid`s then exist in two files.
Deduplicate on `(session_id, uuid)`, never on path, and attribute the project from the
record's own `cwd`.

Both halves are implemented: the dedup as the `records_semantic_uuid` index, and the
attribution as `derive.Identify` → the `records.cwd` column → `derive`'s project fold. The
fold also collapses the worktree *directory* back to its project, by three rules, tried in
order:

1. The assistant's own `<project>/.claude/worktrees/<name>` layout. Exact — the tool created
   the directory.
2. The repository the directory's sessions opened pull requests against, where that
   repository's name appears in the path. Also exact, and the only rule that can identify a
   worktree named after its work rather than after an issue, or tell a build tree deep inside
   a repository from a project of its own. It may only *shorten* a path to something the path
   already names, never rename it — working in one repository and shipping to another is
   ordinary. Repositories that disagree fold nothing.
3. A sibling directory suffixed with an issue or worktree marker. A convention, and the
   dimension's note says so.

None consults the filesystem or the local path separator, and none depends on what else the
archive holds beyond the pull requests of the sessions in question — so a `cwd` collected on
another machine folds identically here. The tables live in `derive`, deliberately not in
configuration: configuration is per machine and nothing in the mesh exchanges it, so two
nodes with different tables would draw different project legends from identical archives.

### 4. The project directory name is lossy

Project directories encode the working directory with both `/` and `.` mapped to `-`, so the
encoding is not invertible. Use the `cwd` field inside records as ground truth.

Because records replicate between machines, the `cwd` fold must also treat `/` and `\` as
equivalent on every host. `path/filepath` follows the local OS and would therefore give two
nodes two different answers for the same string.

### 5. Timestamps do not order the file

Timestamps are not monotonic within a file — concurrent subagents interleave. Sidecar line
types carry no timestamp at all, so the first and last lines often have none, and file mtime
can run many hours ahead of the last real message. Reconstruct causality from `parentUuid`;
derive session bounds from the first and last records that actually carry a timestamp.

## Storage

- **`records`** — the archive. Bodies sealed with an AEAD before they touch the disk.
  Primary key `(origin_id, seq)` for replication; unique on `(origin_id, session_id, uuid)`
  for semantic identity, falling back to a content-addressed key for lines with no uuid.
  Scoping to the origin is what stops two machines that share a username and a transcript
  path from silently merging — which the previous, path-keyed schema did.
- **`request_usage`** — one row per real API request, unique on `(user_id, request_id)`.
  No cost column: pricing is applied at query time from a versioned table, so history can be
  re-priced.
- **Derived** — `sessions`, `tool_calls`, `file_edits`, `compactions`, `pr_links`,
  `projects`, `daily_rollup`. All droppable and rebuildable from `raw_lines`.

`reprocess` re-reads sealed bodies and rewrites the columns extracted from them, which is what
makes "the raw line is the source of truth" a fact rather than a promise: a build that learns
to read a new field can fill it in for history it collected before it could. It is tested,
because it is the guarantee the archive rests on.

It walks each origin from a watermark held in the `extraction` table, advanced in the same
transaction as the batch it covers — the same discipline a replication watermark follows, and
for the same reason. The watermark is keyed by an extraction version, so a new extracted
column costs a bump of one constant rather than a migration of data. It rewrites only the
derived columns; it never touches the sealed body, the content hash, or anything a peer keys
or compares on, so a node that has reprocessed and one that has not still agree on every
digest. `run` does a pass at startup and periodically thereafter; `agentic-stats reprocess`
does one in the foreground.

## Parser lifecycle

Formats change. Parsers **accumulate**; they are never replaced or deleted.

```go
type Parser interface {
    ID() ParserID                  // e.g. "claudecode/v2.1.220+"
    Handles(RawLine) Confidence    // structural probe first, version hint second
    Parse(RawLine) (Facts, error)
    KnownLineTypes() []string
}
```

Selection is by structural detection first: a version field cannot be trusted alone, since
sidecar lines carry none and other sources will not share the numbering scheme.

Every derived row records the `parser_id` that produced it, so a fixed parser can re-derive
only the rows it touched.

Lines no parser claims are stored anyway, marked `unrecognized`, counted, and alerted on.
When a parser appears later, re-extraction backfills the history: bump the extraction version
and every archive re-walks itself against the new parser.
**A format we cannot parse yet costs nothing permanently; a format we failed to store is
gone.**

## Multi-tenancy

One instance serves several users with strict per-user isolation. Every tenant-scoped table
carries a non-null `user_id` and has Postgres **row-level security** enabled, with the
current user set per transaction. Application queries scope themselves too, but RLS is what
guarantees a forgotten `WHERE` cannot leak one user's source code to another.

Registration is invite-only. Devices enroll by redeeming a short-lived code for a
revocable per-device token; `user_id` comes from the token server-side and is never trusted
from the payload.

## Correlation

- **Sessions → pull requests** is exact where the transcript records a PR link.
- **Sessions → issues** via issue numbers embedded in branch names, using a small set of
  configurable patterns. Unmatched branches map to no issue rather than a guess.
- **Sessions → tracker tickets** (Jira and similar) is only a time-window correlation unless
  ticket keys appear in branch names. Where it is a guess, the UI says so.

Detached-HEAD and empty branch values mean "unknown", not a branch named `HEAD`. Assistant-
generated worktree branches fold back to the originating branch.

The branch is extracted into `records.git_branch` and stacks the daily chart. The directory
fold described in trap 3 exists; the branch fold does not yet, and neither does the
branch→issue mapping above.

## Timezones

Everything is stored UTC. Each device reports its IANA zone at capture time, because VMs
usually run UTC while laptops do not — a raw UTC hour histogram would smear "busy hours"
into noise. The dashboard offers a fixed display timezone (default), device-local, or UTC,
and always states which is in use.

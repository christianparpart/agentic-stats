# agentic-stats — project guidelines

A peer-to-peer archive of AI coding-assistant transcripts. One daemon per machine; no
server. Machines find each other, authenticate from a shared key, and sync both ways until
every node holds everything.

**Precedence, highest first:**

1. `.golangci.yml`, `gofmt`, `eslint.config.js` — machine-enforced, they win over prose here.
2. This document.
3. Surrounding code. Match its idiom, naming, and comment density.

---

## Design principles

These are **load-bearing**. Depart from them only for a strong reason, stated at the
declaration. They are the same principles the author's C++ projects use; the language
changed, the reasoning did not.

### Dependency injection

Anything touching **I/O, time, randomness, the filesystem, the network, or the environment**
is reached through an interface — never a concrete type, a package-level singleton, or a free
function with hidden state.

This project is almost entirely I/O, so this is the difference between a testable system and
one that can only be tested by owning a fleet of machines.

```go
// Yes: the collector states what it needs; tests supply fakes.
type Clock interface{ Now() time.Time }
type FileSystem interface {
    Open(name string) (fs.File, error)
    Stat(name string) (fs.FileInfo, error)
}
type Uploader interface{ Upload(context.Context, Batch) error }

// No: time.Now(), os.Open() and http.Post() called directly from logic.
```

Declare interfaces **where they are consumed**, keep them small, and return concrete types.
No `init()` side effects. No exported mutable package variables.

### Configuration at construction time

**A constructed object is a usable object.** Everything a type needs — collaborators, policy,
limits — is supplied to its constructor and fixed thereafter. No `Init()`/`Setup()` second
phase, no zero-value-then-setters, no global knob poked at startup.

- Constructors are `New<Type>(Config, deps...) (*Type, error)`. Fallible setup returns an
  error; it never leaves a half-built object.
- Configuration fields are unexported and have no setters.
- A long constructor is a fact about the data: group parameters into a `Config` struct, which
  data-driven design wants anyway.
- *Configuration is not state.* A method mutating the domain state the type exists to manage
  is fine. A method installing policy read once at startup is not.

Ask: *would two differently-configured instances be two different objects, or one object in
two states?* Different objects → constructor.

### Named types over `bool`

**A `bool` in an API is an anonymous enum whose two values are named after their
representation instead of their meaning.** `parse(line, true, false)` tells a reader nothing.

```go
// Yes
type Confidence uint8
const (
    ConfidenceNone Confidence = iota  // zero value is the "no/absent" case
    ConfidenceWeak
    ConfidenceStrong
)

// No
func Handles(l RawLine) bool
```

- Order enumerators so the off/absent/default case is zero.
- A `bool` **return** is right when the function name asks the question — `IsEmpty()`,
  `Contains()`, `HasPrefix()`. It is a finding when it reports success or failure (return
  `error`, which carries the reason) or selects between two named outcomes (named type).
- Two or more `bool` fields in a struct are usually a state machine hiding in flags.
- Carve-outs, documented at the declaration: `encoding/json` struct tags and other wire
  boundaries, and signatures we do not own (`sort.Interface`, `io` contracts). Convert at the
  boundary; keep the named type inside it.

TypeScript: string-literal union types (`type Scope = "user" | "device" | "fleet"`), never a
boolean parameter.

### Data-driven design

**Behaviour is described by data; code interprets that data.** Adding a log format, a price,
a branch-naming convention or a source adapter must be *adding a row to a table*, not editing
logic scattered across the codebase.

This project is unusually well suited to it, and the design already depends on it:

| Extension point | The table |
|---|---|
| A new transcript format | a `Parser` registered in `derive`'s registry |
| A new assistant to collect from | a `Source` implementation registered in `source` |
| A new model's pricing | a row in `prices.toml` |
| A new external storage backend | a `Backend` implementation |
| A new branch→issue convention | a pattern in config |

When in doubt: *if a sixth case showed up tomorrow, how many places would I edit?* More than
one means it is not data-driven enough yet.

### Errors

Idiomatic Go `(T, error)`, with a **typed error per subsystem, introduced as the need arises**
— do not invent a taxonomy up front.

- Wrap with `%w` and add context: `fmt.Errorf("tail %s: %w", path, err)`.
- Callers discriminate with `errors.Is` / `errors.As`, never by string matching.
- `panic` is for programmer errors (contract violation), never for expected failures. A
  daemon must not die because one file went missing mid-tail.
- Never discard an error to satisfy a linter. If it is genuinely ignorable, say why in a
  comment.

### Testability

**Every package is testable and new code lands with tests.** If something is hard to test,
that is a design smell — inject the dependency, extract the decision.

- **Table-driven tests** are the default. They are the natural companion to data-driven design.
- Fixtures are **synthetic**, generated by `internal/testfixture`. Never real transcripts —
  see `CONTRIBUTING.md`. This is not a style preference; real transcripts contain prompts,
  source code and customer identifiers.
- Parsing correctness is the project's core risk. Every parser owns a **frozen** fixture set:
  written once, never edited, so a refactor cannot quietly change how old data is read.
- Prefer `testing/fstest.MapFS` and injected clocks over touching the real filesystem or
  sleeping.

---

## Zero-warning policy

**A warning is a build break.** `make check` must be clean.

- `gofmt`, `go vet` and `golangci-lint run` clean. Fix the cause; no `//nolint`.
- TypeScript compiles under `strict`. No `any`, no `@ts-ignore`.
- CI runs the same commands. A clean local build that fails CI is a bug in the Makefile.

---

## Language guidelines

**Go** (1.27+)

- `context.Context` is the first parameter of anything that blocks, and is honoured.
- No naked returns; no `interface{}`/`any` where a type parameter or concrete type serves.
- Use `errors.Join` for multi-error aggregation, `sync.OnceValue` over hand-rolled once.
- Prefer `filepath` over `path` for anything touching the filesystem — the agent ships on
  Windows too, and transcript directory names are path-derived.
- All timestamps are `time.Time` in **UTC** at the boundary; local zones are a display concern.
- No third-party dependency without strong justification. The agent must stay a small static
  binary that is trivial to drop onto a VM.
- Document exported identifiers with standard Go doc comments beginning with the name.

**TypeScript / React**

- Function components, hooks, no class components.
- Server response types are generated from the API schema, not hand-written twice.
- Charting is centralized; pages do not each invent their own axis and colour conventions.

---

## Architecture

Layered bottom-up. **Lower layers must not import higher ones.**

```
cmd/agentic-stats/    the only binary -- wiring only, no logic
internal/
  source/             adapter interface: discovers append-only streams.
                      Knows nothing about what the lines mean.
    claudecode/       Claude Code JSONL roots, incl. subagents and desktop
    sourcetest/       in-memory FileSystem for tests
  cursor/             durable tail positions. No knowledge of formats.
  wire/               record shape, gzip-NDJSON codec. No knowledge of formats.
  seal/               THE ONLY place the pre-shared key is used
  store/              the local replica: SQLite, sequences, fork detection
  ingest/             the local write path: seal, extract, append
  derive/             raw lines -> facts. THE ONLY place a transcript is interpreted.
  pricing/            price tables, model-id normalization
  api/                dashboard and read API
    assets/           the dashboard, embedded
```

**Rules that follow from the layering, and are enforced by `.golangci.yml`:**

- `source`, `cursor` and `wire` must not import `derive`, `store` or `ingest`. Collection
  ships bytes; it does not understand them. This is what lets collection keep working when
  the transcript format changes.
- **Only `ingest`, `api` and the mesh may import `seal`.** A grep for the pre-shared key
  outside `seal` should find nothing. Everything else asks for a key or hands over a
  plaintext.
- `derive` is the single place transcript semantics live. The `requestId` fold happens there,
  once.

## Workflow

- `make check` before every commit — `fmt`, `vet`, `lint`, `test`.
- `gitleaks` runs as a pre-commit hook (`git config core.hooksPath .githooks`). Never
  `--no-verify`.
- After changes, look for duplication and simplify (`/simplify`).
- Sign off commits (`git commit -s`).
- In change summaries report **risk assessment** and **test coverage**; note performance
  impact where relevant.

## Invariants worth restating

Violating any of these is a correctness bug, not a style issue:

1. **Fold assistant lines by `requestId` before summing usage.** The same `usage` object is
   repeated on every content block; naive summing inflates output tokens ~2.9x and thinking
   tokens ~4.2x, by a factor that differs per metric. Ties in the fold must be impossible,
   not merely unlikely: two nodes running the same query must return the same row, or
   replicas that agree will look like they diverge.
2. **Raw lines are the source of truth.** Every extracted column must be recomputable from
   the sealed body. Never write a fact that cannot be re-derived.
3. **Parsers accumulate, never replaced.** Deleting an old parser breaks re-derivation of
   every year it covered.
4. **Unrecognized input is stored, never dropped.** A format we cannot parse yet costs
   nothing permanently; one we failed to store is gone.
5. **Sequence numbers are gap-free**, allocated inside the same transaction as the insert.
   Anti-entropy asks for "everything after my watermark", which is only correct without
   holes. Never keep the counter in memory or in a separate row.
6. **A watermark advances only in the transaction that commits the records it covers.**
   Advance-then-crash loses data permanently and invisibly.
7. **A conflicting `(origin_id, seq)` is quarantined, never merged and never discarded.** It
   is proof that two machines share an origin id, and the alternative is silent data loss
   that reports itself as healthy convergence.
8. **Nothing is written to the archive unsealed.** The database file must never contain
   transcript bodies in the clear -- there is a test that greps for exactly that.

# Contributing

## Test fixtures must be synthetic — this is not negotiable

`agentic-stats` parses AI coding-assistant transcripts. Real transcripts contain prompts,
source code, file paths, customer identifiers and repository names. **None of that may ever
enter this repository.**

- Fixtures live in `testdata/synthetic/` and are produced by `internal/testfixture`
  (`go run ./internal/testfixture/cmd/fixturegen`). They use fake UUIDs, `/home/user/example`
  paths and placeholder text.
- `testdata/real/` is gitignored. Validate against your own corpus locally; never commit it.
- Numbers asserted in tests come from synthetic fixtures, not from anyone's real usage.

## Parsers accumulate — they are never replaced

Transcript formats change. When a new format appears, add a **new** parser and leave the old
one in place, with its fixtures untouched. The archive is meant to be re-derivable for years;
deleting an old parser silently breaks every year of data it covered.

Each parser owns a frozen fixture set. Once written, those fixtures and their expected
outputs are not edited — they pin that parser's behaviour permanently. A new format means
new fixtures alongside the old ones.

Unrecognized lines are stored and quarantined, never dropped. A format we cannot parse yet
costs nothing permanently; a format we failed to store is gone forever.

## Before committing

    make check     # fmt, vet, lint, test
    gitleaks protect --staged

`gitleaks` runs as a pre-commit hook and in CI. If it fires, do not `--no-verify`.

## Commits

Sign off your commits (`git commit -s`).

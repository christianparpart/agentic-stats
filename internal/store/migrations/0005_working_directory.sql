-- Attribute work to a project, from the record's own working directory.
--
-- Until now the only project identity the archive held was the repository a
-- session's pull requests went to, so work that opened none had no project at
-- all -- which is most work. On this author's archive that was 11,489 of the
-- 11,880 records captured in a day, all of it in one segment of the daily chart
-- labelled "No pull request".
--
-- Every user, attachment and assistant line carries the working directory it
-- was written in, and the daily grid only admits lines with a request id, which
-- are assistant lines. So cwd covers effectively every billable record and
-- project attribution stops being a heuristic. The transcript's own directory
-- name encodes the same path with separators and dots both mapped to '-', an
-- encoding that cannot be inverted, which is why the field inside the record is
-- the only ground truth available.
--
-- git_branch comes from the same envelope and is extracted in the same pass so
-- the archive is walked once rather than twice. Note that both are plaintext
-- columns, as `path` already was: bodies are sealed, this metadata is not.
ALTER TABLE records ADD COLUMN cwd        TEXT;
ALTER TABLE records ADD COLUMN git_branch TEXT;

-- The fold reads both, so records_fold has to carry both.
--
-- This is not an optimisation to protect, it is the difference between a
-- dashboard and a timeout: `records` is WITHOUT ROWID with the sealed body
-- inline, so an index entry that has to be resolved against the table costs a
-- fresh descent into a multi-gigabyte b-tree, landing on a leaf page holding a
-- handful of rows because the bodies crowd everything else out. 0003 measured
-- 6.4 seconds against 0.28 for exactly this. derive/plan_test.go asserts the
-- plan, so the moment the fold reads a column this index lacks, the tests say
-- so rather than a user noticing a slow page.
--
-- Same leading columns and same partial predicate as the index it replaces, so
-- nothing that used the old one loses its plan.
--
-- Recreating it scans the whole table once. On a 2.3 GB, 488k-record archive
-- that is seconds to a couple of minutes, inside the transaction that applies
-- this migration and therefore inside store.Open -- which is why migrate logs
-- each migration by name before running it.
DROP INDEX records_fold;
CREATE INDEX records_fold ON records (
    request_id, origin_id, seq,
    model, session_id, captured_at, cwd, git_branch,
    input_tokens, output_tokens, think_tokens,
    cache_read, cache_write5m, cache_write1h
) WHERE request_id IS NOT NULL;

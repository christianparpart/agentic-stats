-- Make the dashboard's queries index-only.
--
-- `records` is WITHOUT ROWID keyed on (origin_id, seq), so every row lives in
-- one b-tree together with its sealed body -- several kilobytes each. A
-- secondary index therefore does not point at a cheap row: resolving one entry
-- is a fresh descent into a multi-gigabyte tree, landing on a leaf page that
-- holds only a handful of rows because the bodies crowd everything else out.
--
-- The fold reads twelve columns from every assistant record. Measured on a
-- 480k-record, 2.1 GB archive, doing that through `records_request` cost 6.4
-- seconds for the summary and the same again for the daily breakdown, entirely
-- in those lookups. Widening the index so it carries the columns instead makes
-- the fold a sequential scan of one small structure: 0.28 seconds, and roughly
-- 30 MB of index for it.
--
-- These supersede the indexes they replace rather than sitting alongside them
-- -- same leading columns, same partial predicate -- so nothing that used the
-- old ones loses its plan, and keeping both would pay for the same keys twice.

CREATE INDEX records_fold ON records (
    request_id, origin_id, seq,
    model, session_id, captured_at,
    input_tokens, output_tokens, think_tokens,
    cache_read, cache_write5m, cache_write1h
) WHERE request_id IS NOT NULL;

DROP INDEX records_request;

-- The pull-request join needs the session alongside the pull request, and it
-- is the session that was missing: without it the planner reached the same
-- 480k-row table through `records_session` and filtered on pr_repo row by row.
CREATE INDEX records_pr_session
    ON records (pr_repo, pr_number, session_id) WHERE pr_repo IS NOT NULL;

DROP INDEX records_pr;

-- How far identity extraction has walked each origin.
--
-- Columns like cwd are extracted at write time because SQL cannot read inside a
-- sealed body. Records already in the archive were written by a build that did
-- not know about them, so they carry NULL and the only way to fill them is to
-- unseal each body and re-derive -- which needs a resumable notion of progress
-- over a table with half a million rows.
--
-- A watermark rather than a "WHERE cwd IS NULL" probe, for three reasons:
--
--   * NULL is a legitimate answer. A line that genuinely carried no working
--     directory is indistinguishable from one not yet examined, so the predicate
--     is not actually a test of whether there is work left to do.
--   * pr_repo needs the same treatment and cannot be found that way at all: it
--     is set on pr-link lines, whose request_id is NULL, so they are outside
--     records_fold entirely and the probe would be a full table scan.
--   * Work remaining becomes the version vector minus these marks, so the pass
--     reuses the two most heavily tested read paths in the store and costs one
--     cheap grouped query once the archive is complete, rather than an index
--     scan at every startup forever.
--
-- The mark advances in the same transaction as the rows it covers, for the same
-- reason a replication watermark does: advancing first and crashing loses the
-- work permanently and invisibly, and reports itself as finished.
--
-- version is what makes a future column cheap. Extract something new, bump the
-- constant, and the archive re-walks itself; nothing here has to change.
--
-- Deliberately not named records_extraction: derive/plan_test.go decides whether
-- a query plan touches the archive by testing for the prefix "SEARCH records",
-- which any table whose name starts with "records" would match, failing a test
-- that has nothing to do with it.
CREATE TABLE extraction (
    origin_id  TEXT    NOT NULL,
    version    INTEGER NOT NULL,
    seq        INTEGER NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (origin_id, version)
) WITHOUT ROWID;

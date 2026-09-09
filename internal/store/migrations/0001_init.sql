-- The archive, as a peer replica.
--
-- There is no tenant column and no row-level security: membership is the
-- pre-shared key, and a node holds exactly one mesh's data. What replaced RLS
-- is that record bodies are sealed before they are written, so the file is
-- worthless without the key.

-- Exactly one row. The node's own identity lives in the database it labels --
-- not in a config file, which gets rsynced between machines, and not derived
-- from a hostname or MAC, which repeat. A duplicated origin id is the one
-- failure mode that corrupts silently, so the id travels with its data.
CREATE TABLE node (
    singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
    origin_id  TEXT    NOT NULL,
    created_at TEXT    NOT NULL
);

CREATE TABLE records (
    origin_id    TEXT    NOT NULL,
    -- Gap-free and monotonic per origin, allocated inside the insert
    -- transaction so the database is the counter. Gap-freeness is
    -- load-bearing: "everything after my watermark" is only correct
    -- without holes.
    seq          INTEGER NOT NULL,

    source       TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    byte_offset  INTEGER NOT NULL,
    -- Also the fork detector: the same (origin, seq) arriving with a
    -- different hash is proof that an origin id was duplicated.
    content_hash TEXT    NOT NULL,

    -- Extracted for identity and for the metric layer, because a sealed body
    -- cannot be read by SQL. The sealed line remains authoritative and every
    -- one of these is recomputable from it by reprocess.
    session_id   TEXT,
    line_uuid    TEXT,
    captured_at  TEXT,

    -- The metric fields, extracted once at write time.
    --
    -- A sealed body cannot be read by SQL, so re-parsing per query is not an
    -- option here as it was under Postgres. Promoting them to columns is also
    -- faster and avoids SQLite's expression-index trap, where an index over a
    -- JSON expression is only used when the query text matches it exactly.
    -- request_id is NULL on everything that is not a billable assistant
    -- response, which is what makes the fold's WHERE clause trivial.
    request_id    TEXT,
    model         TEXT,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    think_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read    INTEGER NOT NULL DEFAULT 0,
    cache_write5m INTEGER NOT NULL DEFAULT 0,
    cache_write1h INTEGER NOT NULL DEFAULT 0,

    sealed       BLOB    NOT NULL,
    received_at  TEXT    NOT NULL,

    PRIMARY KEY (origin_id, seq)
) WITHOUT ROWID;

-- Semantic identity, scoped to the origin.
--
-- Scoping matters: it is what stops two machines that happen to share a
-- username and a transcript path from silently merging into one row, which the
-- previous schema did. Within one origin, a session relocated into a worktree
-- and appended under a second path still converges, because the session and
-- uuid pair is stable.
CREATE UNIQUE INDEX records_semantic_uuid
    ON records (origin_id, session_id, line_uuid)
    WHERE line_uuid IS NOT NULL;

CREATE UNIQUE INDEX records_semantic_hash
    ON records (origin_id, source, path, byte_offset, content_hash)
    WHERE line_uuid IS NULL;

CREATE INDEX records_captured ON records (captured_at);

-- Serves the fold directly: one row per request, ordered so ties cannot occur.
CREATE INDEX records_request ON records (request_id, origin_id, seq)
    WHERE request_id IS NOT NULL;

-- How far each peer has confirmed, per origin. Advanced in the same
-- transaction that commits the records it covers: advance-then-crash loses
-- data permanently and invisibly.
CREATE TABLE watermarks (
    peer_id   TEXT    NOT NULL,
    origin_id TEXT    NOT NULL,
    seq       INTEGER NOT NULL,
    PRIMARY KEY (peer_id, origin_id)
) WITHOUT ROWID;

-- Configured and learned peer addresses, persisted so a cold start does not
-- need the bootstrap node to be up.
CREATE TABLE peers (
    peer_id   TEXT PRIMARY KEY,
    addrs     TEXT NOT NULL,
    last_seen TEXT,
    static    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- Fork evidence. A conflicting (origin, seq) is never silently discarded and
-- never merged: it is kept here and surfaced, because the alternative is
-- invisible data loss that reports itself as healthy convergence.
CREATE TABLE quarantine (
    origin_id    TEXT    NOT NULL,
    seq          INTEGER NOT NULL,
    content_hash TEXT    NOT NULL,
    sealed       BLOB    NOT NULL,
    noticed_at   TEXT    NOT NULL
);

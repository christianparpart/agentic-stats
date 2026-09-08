-- Tenancy and the raw-line archive.
--
-- Row-level security is established here, in the first migration, rather than
-- added later: this database holds complete transcripts, including source code,
-- and isolation must never depend on remembering a WHERE clause.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL UNIQUE,
    display_name  text        NOT NULL DEFAULT '',
    password_hash text        NOT NULL,
    role          text        NOT NULL DEFAULT 'user'
                              CHECK (role IN ('admin', 'user')),
    -- Reserved so a future team view is additive rather than a rewrite.
    org_id        uuid,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Registration is invite-only: open signup on an internet-facing instance
-- holding this payload is not sensible.
CREATE TABLE invites (
    code_hash   text        PRIMARY KEY,
    email       text,
    created_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    redeemed_at timestamptz,
    redeemed_by uuid        REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE devices (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    hostname      text        NOT NULL,
    os            text        NOT NULL DEFAULT '',
    arch          text        NOT NULL DEFAULT '',
    -- The machine's IANA zone, so "busy hours" can be rendered in local time
    -- rather than smeared across zones by VMs that run UTC.
    timezone      text        NOT NULL DEFAULT '',
    agent_version text        NOT NULL DEFAULT '',
    first_seen    timestamptz NOT NULL DEFAULT now(),
    last_seen     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, hostname, os, arch)
);

-- A device redeems a short-lived enrollment code once, for a long-lived
-- revocable token. Revoking one machine never touches the others.
CREATE TABLE enrollment_codes (
    code_hash   text        PRIMARY KEY,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at  timestamptz NOT NULL,
    redeemed_at timestamptz
);

CREATE TABLE device_tokens (
    token_hash text        PRIMARY KEY,
    device_id  uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used  timestamptz,
    revoked_at timestamptz
);

-- The archive. Lines are stored verbatim and never modified.
--
-- The primary key is (user_id, source, path, byte_offset, content_hash) rather
-- than anything derived from the line's meaning: ingest must not interpret its
-- payload. content_hash participates so that a file replaced in place, whose
-- new content occupies the same offsets, does not collide with the old.
--
-- Semantic deduplication -- the same session relocated into a worktree and
-- appended to under a second path -- happens in derive, on (session, uuid).
CREATE TABLE raw_lines (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id    uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    source       text        NOT NULL,
    path         text        NOT NULL,
    byte_offset  bigint      NOT NULL,
    content_hash text        NOT NULL,
    -- The line exactly as it appeared on disk.
    raw          text        NOT NULL,
    -- Parsed form when the payload is JSON, for querying without re-parsing.
    -- NULL is not a failure: a line we cannot parse today is still archived,
    -- and reprocessing can fill this in once a parser exists.
    body         jsonb,
    ingested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, source, path, byte_offset, content_hash)
);

CREATE INDEX raw_lines_user_ingested_idx ON raw_lines (user_id, ingested_at);
CREATE INDEX raw_lines_device_idx        ON raw_lines (device_id);

-- Tenant isolation, enforced by the database.
--
-- Two tiers, and the distinction is deliberate.
--
-- Payload tables (raw_lines, devices) carry FORCE. FORCE matters because
-- without it the table owner -- which is the role the application connects as
-- -- silently bypasses every policy. These hold complete transcripts including
-- source code, so isolation must not depend on remembering a WHERE clause.
--
-- Credential tables (users, invites, enrollment_codes, device_tokens) have RLS
-- enabled but not FORCE. They must be readable before a tenant is known: an
-- ingest request arrives with only a bearer token, and resolving that token to
-- a user is precisely how the tenant gets established. A non-owner role is
-- still constrained by the policies below; the owner may read them, and the
-- application does so only through the narrow lookup path in internal/auth,
-- which sets app.user_id immediately afterwards.
--
-- current_setting(..., true) returns NULL when unset, so a connection that
-- forgot to establish a tenant sees nothing rather than everything.

ALTER TABLE users            ENABLE ROW LEVEL SECURITY;
ALTER TABLE invites          ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices          ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_tokens    ENABLE ROW LEVEL SECURITY;
ALTER TABLE enrollment_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE raw_lines        ENABLE ROW LEVEL SECURITY;

ALTER TABLE devices   FORCE ROW LEVEL SECURITY;
ALTER TABLE raw_lines FORCE ROW LEVEL SECURITY;

CREATE POLICY users_self ON users
    USING (id = nullif(current_setting('app.user_id', true), '')::uuid);

CREATE POLICY devices_tenant ON devices
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
    WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

CREATE POLICY device_tokens_tenant ON device_tokens
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

CREATE POLICY enrollment_codes_tenant ON enrollment_codes
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

-- WITH CHECK is what stops a compromised or buggy writer from filing lines
-- under another tenant's id.
CREATE POLICY raw_lines_tenant ON raw_lines
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
    WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

-- Postgres has no TRY_CAST, and raw lines are archived before anything knows
-- whether they are JSON. This lets queries interpret the archive without a
-- second stored copy of every line, and without a malformed line aborting a
-- whole query.
CREATE FUNCTION try_jsonb(t text) RETURNS jsonb
    LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    RETURN t::jsonb;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$;

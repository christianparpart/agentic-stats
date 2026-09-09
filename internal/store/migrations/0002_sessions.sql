-- Browser sessions for the dashboard.
--
-- A credential table, so RLS is enabled but not forced: resolving a session
-- cookie to its owner is how the tenant gets established, and therefore cannot
-- itself require a tenant. Tokens are stored only as digests.
CREATE TABLE sessions (
    token_hash text        PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_used  timestamptz
);

CREATE INDEX sessions_user_idx ON sessions (user_id);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;

CREATE POLICY sessions_tenant ON sessions
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

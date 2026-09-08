#!/bin/sh
# Prepare a database for the integration tests.
#
# The application role must NOT be a superuser: superusers bypass row-level
# security entirely, which would make the tenant-isolation tests pass without
# testing anything.
#
# Usage: setup-test-db.sh <superuser-psql-url>
set -eu

ADMIN_URL="${1:?usage: setup-test-db.sh <superuser-psql-url>}"

psql "$ADMIN_URL" -v ON_ERROR_STOP=1 <<'SQL'
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentic_app') THEN
        CREATE ROLE agentic_app LOGIN PASSWORD 'agentic_app'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END $$;
SQL

psql "$ADMIN_URL" -v ON_ERROR_STOP=1 \
    -c "GRANT ALL ON SCHEMA public TO agentic_app;" \
    -c "ALTER SCHEMA public OWNER TO agentic_app;"

echo "test database prepared: role agentic_app owns schema public"

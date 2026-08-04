CREATE TABLE tenants (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    daily_token_limit   INTEGER,                    -- NULL = unlimited
    max_concurrent_runs INTEGER NOT NULL DEFAULT 2,
    created_at          TEXT NOT NULL
);

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    name         TEXT NOT NULL,
    token_sha256 TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);

-- Pre-M1 rows carry tenant_id 't_default'; give it a real tenant row.
INSERT INTO tenants (id, name, max_concurrent_runs, created_at)
VALUES ('t_default', 'default', 2, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

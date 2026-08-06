-- M4: the human-login surface (D1). Users are the canonical identity; the
-- linking table maps authenticator-issued identities onto them, so a future
-- IdP migration never orphans attribution (components.md §11).
CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    color       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    disabled_at TEXT
);
CREATE INDEX users_by_tenant ON users(tenant_id);

CREATE TABLE user_identities (
    issuer     TEXT NOT NULL,   -- 'static' | an IdP issuer URL (M5+)
    subject    TEXT NOT NULL,   -- account id | OIDC sub
    user_id    TEXT NOT NULL REFERENCES users(id),
    created_at TEXT NOT NULL,
    PRIMARY KEY (issuer, subject)
);

-- Browser sessions minted by the IdentityService after a completed login.
-- Same at-rest discipline as api_tokens: plaintext shown to the browser once
-- (as a cookie), only the SHA-256 stored.
CREATE TABLE web_sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    token_sha256 TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    revoked_at   TEXT
);

-- Attribution: which principal caused an event (R-SEC-2). Empty for events
-- emitted by the platform itself or by pre-M4 rows.
ALTER TABLE events ADD COLUMN actor TEXT NOT NULL DEFAULT '';

-- D2: per-tenant storage cap. NULL = unlimited.
ALTER TABLE tenants ADD COLUMN max_storage_bytes INTEGER;

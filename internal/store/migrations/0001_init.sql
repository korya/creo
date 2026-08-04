CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL DEFAULT 't_default',
    name       TEXT NOT NULL,
    profile_id TEXT NOT NULL DEFAULT 'websites-v0',
    created_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL DEFAULT 't_default',
    project_id TEXT NOT NULL REFERENCES projects(id),
    created_at TEXT NOT NULL
);

CREATE TABLE events (
    session_id TEXT    NOT NULL REFERENCES sessions(id),
    seq        INTEGER NOT NULL,
    id         TEXT    NOT NULL UNIQUE,
    run_id     TEXT,
    type       TEXT    NOT NULL,
    user_text  TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    PRIMARY KEY (session_id, seq)
);

CREATE TABLE runs (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL REFERENCES sessions(id),
    project_id       TEXT NOT NULL REFERENCES projects(id),
    trigger_event_id TEXT NOT NULL,
    status           TEXT NOT NULL, -- queued|running|recovering|completed|failed
    lease_worker     TEXT NOT NULL DEFAULT '',
    lease_gen        INTEGER NOT NULL DEFAULT 0,
    lease_expires_at TEXT NOT NULL DEFAULT '',
    outcome          TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX runs_by_project ON runs(project_id, status);

CREATE TABLE idempotency_keys (
    session_id TEXT NOT NULL,
    key        TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (session_id, key)
);

CREATE TABLE versions (
    id                TEXT NOT NULL,
    project_id        TEXT NOT NULL REFERENCES projects(id),
    seq               INTEGER NOT NULL,
    parent_id         TEXT NOT NULL DEFAULT '',
    produced_by_event TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    PRIMARY KEY (project_id, id)
);

CREATE TABLE version_files (
    project_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    path       TEXT NOT NULL,
    blob_sha   TEXT NOT NULL,
    size       INTEGER NOT NULL,
    PRIMARY KEY (project_id, version_id, path)
);

CREATE TABLE usage (
    run_id        TEXT NOT NULL,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    created_at    TEXT NOT NULL
);

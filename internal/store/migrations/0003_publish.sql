-- The publish pointer: one live version per project, addressed publicly by slug.
CREATE TABLE published (
    project_id   TEXT PRIMARY KEY REFERENCES projects(id),
    version_id   TEXT NOT NULL,
    slug         TEXT NOT NULL UNIQUE,
    published_at TEXT NOT NULL
);

-- Per-project capability secret for preview URLs (T1 posture). Backfilled on
-- first preview request; new projects get one at creation.
ALTER TABLE projects ADD COLUMN preview_secret TEXT NOT NULL DEFAULT '';

-- Live discussions of an MR as GitLab has them (C-19): threads on diff lines, threads on the MR and plain comments,
-- with every note. Refreshed by sync; the «Обсуждения» tab and the review prompt read them from here.
CREATE TABLE IF NOT EXISTS mr_discussions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mr_id INTEGER NOT NULL REFERENCES merge_requests(id) ON DELETE CASCADE,
    gitlab_id TEXT NOT NULL,
    author TEXT NOT NULL DEFAULT '',
    file TEXT NOT NULL DEFAULT '',
    old_path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    old_line INTEGER NOT NULL DEFAULT 0,
    head_sha TEXT NOT NULL DEFAULT '',
    base_sha TEXT NOT NULL DEFAULT '',
    resolvable INTEGER NOT NULL DEFAULT 0,
    resolved INTEGER NOT NULL DEFAULT 0,
    resolved_by TEXT NOT NULL DEFAULT '',
    notes_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL DEFAULT '',
    UNIQUE (mr_id, gitlab_id)
);
CREATE INDEX IF NOT EXISTS mr_discussions_mr ON mr_discussions (mr_id);
-- When the discussions of the MR were last read from GitLab ('' = never).
ALTER TABLE merge_requests ADD COLUMN discussions_synced_at TEXT NOT NULL DEFAULT '';
-- The AI assessment of a reviewer thread names the GitLab discussion it is about ('' = runs before 0.32).
ALTER TABLE discussions ADD COLUMN gitlab_id TEXT NOT NULL DEFAULT '';

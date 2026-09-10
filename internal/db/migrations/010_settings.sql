-- Settings changed from the UI (skill overrides per action). Keys are dotted: skill.<kind>.name / .custom / .text.
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

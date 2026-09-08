CREATE TABLE IF NOT EXISTS merge_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    gitlab_host TEXT NOT NULL,
    project_path TEXT NOT NULL,
    iid INTEGER NOT NULL,
    web_url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL DEFAULT '',
    source_branch TEXT NOT NULL DEFAULT '',
    target_branch TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    unresolved INTEGER NOT NULL DEFAULT 0,
    gitlab_updated_at TEXT NOT NULL DEFAULT '',
    synced_at TEXT NOT NULL DEFAULT '',
    added_at TEXT NOT NULL,
    UNIQUE (gitlab_host, project_path, iid)
);

CREATE TABLE IF NOT EXISTS issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    gitlab_host TEXT NOT NULL,
    project_path TEXT NOT NULL,
    iid INTEGER NOT NULL,
    web_url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT '',
    labels TEXT NOT NULL DEFAULT '',
    gitlab_updated_at TEXT NOT NULL DEFAULT '',
    synced_at TEXT NOT NULL DEFAULT '',
    added_at TEXT NOT NULL,
    UNIQUE (gitlab_host, project_path, iid)
);

-- One table for every agent run: reviews, verifies, plans, implementations.
CREATE TABLE IF NOT EXISTS runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,                     -- review_full | review_verify | plan | implement
    mr_id INTEGER REFERENCES merge_requests(id) ON DELETE CASCADE,
    issue_id INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    base_run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
    head_sha TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,                   -- queued | running | done | failed | cancelled
    runner TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    skill_identifier TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    verdict TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    raw_result TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    log_path TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    work_dir TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    cost_usd REAL NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_mr ON runs(mr_id, id);
CREATE INDEX IF NOT EXISTS idx_runs_issue ON runs(issue_id, id);

CREATE TABLE IF NOT EXISTS findings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    origin_finding_id INTEGER REFERENCES findings(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'open',    -- open | fixed | obsolete
    severity TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',
    file TEXT NOT NULL DEFAULT '',
    line INTEGER,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    suggestion TEXT NOT NULL DEFAULT '',
    verify_note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_findings_run ON findings(run_id, ordinal);

CREATE TABLE IF NOT EXISTS discussions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    author TEXT NOT NULL DEFAULT '',
    file TEXT NOT NULL DEFAULT '',
    line INTEGER,
    body TEXT NOT NULL DEFAULT '',
    assessment TEXT NOT NULL DEFAULT '',
    addressed INTEGER NOT NULL DEFAULT 0
);

-- Follow-up conversation with the agent that produced a run (resumed session).
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role TEXT NOT NULL,                     -- user | assistant | error
    content TEXT NOT NULL,
    cost_usd REAL NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_run ON messages(run_id, id);

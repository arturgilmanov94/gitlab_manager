-- Permission prompts of an agent run, answered from the dashboard (like the terminal would ask).
CREATE TABLE IF NOT EXISTS approvals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    input_json TEXT NOT NULL DEFAULT '',
    suggestions_json TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',   -- pending | allowed | denied | expired
    remember INTEGER NOT NULL DEFAULT 0,      -- allowed together with the agent's suggested rules
    note TEXT NOT NULL DEFAULT '',            -- developer's message to the agent when denied
    created_at TEXT NOT NULL,
    decided_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_approvals_run ON approvals(run_id, id);

-- What the agent is doing right now (last tool call), what it was refused, and the exported plan file.
ALTER TABLE runs ADD COLUMN progress TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN denials_json TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN plan_path TEXT NOT NULL DEFAULT '';

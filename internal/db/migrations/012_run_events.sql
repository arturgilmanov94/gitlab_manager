-- Tool calls of a run grouped into phases (Phase 4 structural progress): what the agent did, in order, without percentages.
CREATE TABLE IF NOT EXISTS run_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    at TEXT NOT NULL,
    phase TEXT NOT NULL,       -- workspace | analysis | plan | implement | tests | commands | subagent | question
    tool TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, id);

-- Questions an agent asks the developer instead of guessing (Phase 4 «Нужен ваш ответ» по существу). The run stops
-- in status `waiting`, the developer answers in the dashboard, the same agent session continues with the answers.
CREATE TABLE IF NOT EXISTS run_questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round INTEGER NOT NULL DEFAULT 1,
    ordinal INTEGER NOT NULL,
    question TEXT NOT NULL,
    options_json TEXT NOT NULL DEFAULT '[]',
    why TEXT NOT NULL DEFAULT '',
    answer TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    answered_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_run_questions_run ON run_questions(run_id, id);

-- Context window fill of the agent session: tokens in the context at the last turn of the main agent and the
-- model's window size (from Claude Code's modelUsage.contextWindow). Only runs started from 0.21.0 have them.
ALTER TABLE runs ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0;

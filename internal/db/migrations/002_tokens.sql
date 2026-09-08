-- Token accounting per run and per follow-up message (the user is on a subscription: tokens matter, not dollars).
ALTER TABLE runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0;

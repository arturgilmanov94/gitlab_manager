-- The finished run whose agent session this run continues, chosen by the developer before the start
-- ("Продолжить сессию #N" instead of a new chat). NULL = new session with an empty context.
ALTER TABLE runs ADD COLUMN continue_run_id INTEGER;

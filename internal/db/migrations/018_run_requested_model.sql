-- The model the developer picked for the run (alias or full name, "" = the agent's default), as opposed to
-- `model`, which is filled after the run with the models the agent actually used. Kept for retry/continuation.
ALTER TABLE runs ADD COLUMN requested_model TEXT NOT NULL DEFAULT '';

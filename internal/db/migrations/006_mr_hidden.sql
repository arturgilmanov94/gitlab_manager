-- "Убрать из dashboard" no longer deletes: the MR is hidden into the history and can be brought back.
ALTER TABLE merge_requests ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;

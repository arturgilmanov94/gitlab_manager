-- "Просмотрено": the developer marks an MR as seen; it dims and sinks to the end of the list. The mark is cleared
-- by the next sync/refresh that brings new commits or new/changed discussions (notes_count backs that check).
ALTER TABLE merge_requests ADD COLUMN notes_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN seen_at TEXT NOT NULL DEFAULT '';

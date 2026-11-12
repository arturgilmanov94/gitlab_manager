-- The head the developer had looked at when they pressed «Просмотрено». It stays after new commits clear the mark:
-- the «Изменения» tab shows what arrived since ('' = never marked).
ALTER TABLE merge_requests ADD COLUMN seen_sha TEXT NOT NULL DEFAULT '';
UPDATE merge_requests SET seen_sha = head_sha WHERE seen_at != '';

-- Head pipeline of a merge request (Phase 6, CI screen): id for the jobs API and the link for the UI.
ALTER TABLE merge_requests ADD COLUMN pipeline_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN pipeline_url TEXT NOT NULL DEFAULT '';

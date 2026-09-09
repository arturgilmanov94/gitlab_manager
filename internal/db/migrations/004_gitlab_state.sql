-- GitLab state of a merge request shown next to the AI-review state: pipeline, approvals, drift from target, draft.
ALTER TABLE merge_requests ADD COLUMN pipeline_status TEXT NOT NULL DEFAULT '';
ALTER TABLE merge_requests ADD COLUMN approvals_given INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN approvals_required INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN diverged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN changes_count TEXT NOT NULL DEFAULT '';

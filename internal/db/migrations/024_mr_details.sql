-- People and merge readiness of an MR as GitLab reports them (JSON: approved_by, reviewers, assignees,
-- has_conflicts, merge_status); '' = not read yet. Shown on the MR page, not used for filtering in SQL.
ALTER TABLE merge_requests ADD COLUMN gitlab_details TEXT NOT NULL DEFAULT '';

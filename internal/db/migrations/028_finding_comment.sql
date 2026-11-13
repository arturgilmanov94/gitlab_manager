-- The ready GitLab text of a finding, written by the agent in the developer's style (short), while description and
-- suggestion stay detailed for the dashboard. Empty for older findings: the text is then built from them.
ALTER TABLE findings ADD COLUMN gitlab_comment TEXT NOT NULL DEFAULT '';

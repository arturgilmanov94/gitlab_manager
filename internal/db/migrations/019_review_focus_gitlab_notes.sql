-- What the developer asked the next reviews of this MR to pay attention to («На что обратить внимание»).
-- Not touched by GitLab syncs; every review run takes a snapshot of it into runs.notes.
ALTER TABLE merge_requests ADD COLUMN review_focus TEXT NOT NULL DEFAULT '';
-- Link to the GitLab note a finding was posted as («Отправить в GitLab»); '' = not posted.
ALTER TABLE findings ADD COLUMN gitlab_note_url TEXT NOT NULL DEFAULT '';

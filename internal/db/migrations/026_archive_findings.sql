-- Findings of merged and closed MRs are archived: nobody is going to fix them in that MR any more, so they must
-- not count as open in the lists and the history. New ones are archived when a sync sees the MR closed.
UPDATE findings SET status = 'archived'
WHERE status = 'open' AND run_id IN (SELECT r.id FROM runs r JOIN merge_requests m ON m.id = r.mr_id WHERE m.state IN ('merged', 'closed'));

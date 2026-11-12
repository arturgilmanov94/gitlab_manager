-- Two edit runs never work in one workspace at the same time (C-11). A leftover duplicate from before (only
-- possible after a race) is cancelled first, oldest one kept, so the index can be created.
UPDATE runs SET status = 'cancelled', error = 'Cancelled: another run was active in the same workspace', finished_at = COALESCE(NULLIF(finished_at, ''), created_at)
WHERE status IN ('queued', 'running', 'waiting') AND work_dir != ''
  AND kind IN ('implement', 'fix_comments', 'ci_fix', 'fix_findings', 'stand_test')
  AND id NOT IN (SELECT MIN(id) FROM runs WHERE status IN ('queued', 'running', 'waiting') AND work_dir != ''
                 AND kind IN ('implement', 'fix_comments', 'ci_fix', 'fix_findings', 'stand_test') GROUP BY work_dir);
CREATE UNIQUE INDEX IF NOT EXISTS runs_active_edit_work_dir ON runs (work_dir)
WHERE status IN ('queued', 'running', 'waiting') AND work_dir != ''
  AND kind IN ('implement', 'fix_comments', 'ci_fix', 'fix_findings', 'stand_test');

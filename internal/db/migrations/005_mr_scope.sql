-- Whether the MR still concerns the developer: roles in the MR (author / assignee / reviewer), own approval, and
-- whether it was added by hand (kept until removed by hand). Irrelevant MRs move to the history or are pruned on sync.
ALTER TABLE merge_requests ADD COLUMN my_roles TEXT NOT NULL DEFAULT '';
ALTER TABLE merge_requests ADD COLUMN approved_by_me INTEGER NOT NULL DEFAULT 0;
ALTER TABLE merge_requests ADD COLUMN manual INTEGER NOT NULL DEFAULT 0;

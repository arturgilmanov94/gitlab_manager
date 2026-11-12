-- The stage of a task, stored so that lists and the page agree (T20): computed from runs and linked MRs
-- ('' = not computed yet), or set by the developer (stage_manual = 1) until they put it back to automatic.
ALTER TABLE issues ADD COLUMN stage TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN stage_manual INTEGER NOT NULL DEFAULT 0;
-- MRs linked to the task as GitLab knows them (related_merge_requests, closed_by) and the comments of the task;
-- JSON, refreshed with the task.
ALTER TABLE issues ADD COLUMN related_mrs TEXT NOT NULL DEFAULT '[]';
ALTER TABLE issues ADD COLUMN notes_json TEXT NOT NULL DEFAULT '[]';

-- Mode of a run within its kind: plan runs distinguish a task plan ('') from a bug analysis ('bug').
ALTER TABLE runs ADD COLUMN mode TEXT NOT NULL DEFAULT '';

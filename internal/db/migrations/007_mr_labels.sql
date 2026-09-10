-- GitLab labels of a merge request (comma separated, as for issues): shown as badges and used by the list filters.
ALTER TABLE merge_requests ADD COLUMN labels TEXT NOT NULL DEFAULT '';

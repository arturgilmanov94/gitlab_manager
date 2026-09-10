-- «Проверить замечание»: a separate read-only run re-examines one finding. Its outcome lives on the finding
-- (check_status: confirmed | false_positive | obsolete | unclear, check_run_id) and the run knows its finding.
ALTER TABLE findings ADD COLUMN check_status TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN check_run_id INTEGER;
ALTER TABLE runs ADD COLUMN finding_id INTEGER;

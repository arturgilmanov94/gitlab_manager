-- «Исправить выбранные»: which findings and discussions a fix run was asked to address ({"findings":[ids],"discussions":[ids]}).
ALTER TABLE runs ADD COLUMN selection_json TEXT NOT NULL DEFAULT '';

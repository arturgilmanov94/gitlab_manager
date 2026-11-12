-- Projects (repositories) the dashboard works with. The first one is created at start from .env (PROJECT_ROOT,
-- GITLAB_HOST/GITLAB_PROJECT, BASE_BRANCH, STAND_SKILL, GITLAB_ISSUE_PROJECTS); .env stays the default for
-- empty fields.
CREATE TABLE IF NOT EXISTS projects (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    gitlab_host    TEXT NOT NULL DEFAULT '',
    project_path   TEXT NOT NULL DEFAULT '',
    repo_root      TEXT NOT NULL,
    base_branch    TEXT NOT NULL DEFAULT '',
    stand_skill    TEXT NOT NULL DEFAULT '',
    issue_projects TEXT NOT NULL DEFAULT '',  -- comma-separated GitLab paths whose issues are this project's tasks
    created_at     TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS projects_repo_root ON projects (repo_root);

-- NULL = not assigned yet (filled at start for existing rows) or an MR of a GitLab project without a repository here.
ALTER TABLE merge_requests ADD COLUMN project_id INTEGER REFERENCES projects (id);
ALTER TABLE issues ADD COLUMN project_id INTEGER REFERENCES projects (id);
ALTER TABLE runs ADD COLUMN project_id INTEGER REFERENCES projects (id);
CREATE INDEX IF NOT EXISTS merge_requests_project ON merge_requests (project_id);
CREATE INDEX IF NOT EXISTS issues_project ON issues (project_id);
CREATE INDEX IF NOT EXISTS runs_project ON runs (project_id);

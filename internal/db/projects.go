package db

import (
	"database/sql"
	"errors"
	"strings"
)

// Project is a repository the dashboard works with: where its checkout is, which GitLab project it is, which
// branch new work starts from. Empty fields fall back to .env (see app.Service.ForProject).
type Project struct {
	ID            int64
	Name          string
	GitLabHost    string
	ProjectPath   string
	RepoRoot      string
	BaseBranch    string
	StandSkill    string
	IssueProjects string // comma-separated GitLab paths whose issues are tasks of this project ("" = the project itself / .env)
	CreatedAt     string
}

// IssueProjectList is IssueProjects as a list.
func (p Project) IssueProjectList() []string {
	var out []string
	for _, item := range strings.Split(p.IssueProjects, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

const projectColumns = "id, name, gitlab_host, project_path, repo_root, base_branch, stand_skill, issue_projects, created_at"

func scanProject(s scanner) (*Project, error) {
	var p Project
	if err := s.Scan(&p.ID, &p.Name, &p.GitLabHost, &p.ProjectPath, &p.RepoRoot, &p.BaseBranch, &p.StandSkill, &p.IssueProjects, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects returns all projects, oldest first (the first one is the default).
func (d *DB) ListProjects() ([]Project, error) {
	rows, err := d.sql.Query("SELECT " + projectColumns + " FROM projects ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetProject returns a project or nil.
func (d *DB) GetProject(id int64) (*Project, error) {
	p, err := scanProject(d.sql.QueryRow("SELECT "+projectColumns+" FROM projects WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// CreateProject stores a new project and returns it.
func (d *DB) CreateProject(p Project) (*Project, error) {
	res, err := d.sql.Exec(`INSERT INTO projects (name, gitlab_host, project_path, repo_root, base_branch, stand_skill, issue_projects, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, p.Name, p.GitLabHost, p.ProjectPath, p.RepoRoot, p.BaseBranch, p.StandSkill, p.IssueProjects, Now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return d.GetProject(id)
}

// UpdateProject stores the editable fields of a project.
func (d *DB) UpdateProject(p Project) error {
	_, err := d.sql.Exec(`UPDATE projects SET name = ?, gitlab_host = ?, project_path = ?, base_branch = ?, stand_skill = ?, issue_projects = ? WHERE id = ?`,
		p.Name, p.GitLabHost, p.ProjectPath, p.BaseBranch, p.StandSkill, p.IssueProjects, p.ID)
	return err
}

// DeleteProject removes a project that has no MRs, tasks or runs.
func (d *DB) DeleteProject(id int64) error {
	var n int
	if err := d.sql.QueryRow(`SELECT (SELECT COUNT(*) FROM merge_requests WHERE project_id = ?) + (SELECT COUNT(*) FROM issues WHERE project_id = ?)
		+ (SELECT COUNT(*) FROM runs WHERE project_id = ?)`, id, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("the project still has merge requests, tasks or runs")
	}
	_, err := d.sql.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

// AssignProjects fills project_id where it is still empty: MRs by their GitLab project, tasks by the project's
// issue trackers (else the default project), runs by their MR or task (else the default project). It returns
// how many rows of each kind got a project. Rows that already have one are never touched.
func (d *DB) AssignProjects(projects []Project) (mrs, issues, runs int64, err error) {
	if len(projects) == 0 {
		return 0, 0, 0, nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	count := func(res sql.Result, err error) (int64, error) {
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	for _, p := range projects {
		n, err := count(tx.Exec("UPDATE merge_requests SET project_id = ? WHERE project_id IS NULL AND gitlab_host = ? AND project_path = ?", p.ID, p.GitLabHost, p.ProjectPath))
		if err != nil {
			return 0, 0, 0, err
		}
		mrs += n
		for _, tracker := range append(p.IssueProjectList(), p.ProjectPath) {
			n, err := count(tx.Exec("UPDATE issues SET project_id = ? WHERE project_id IS NULL AND project_path = ?", p.ID, tracker))
			if err != nil {
				return 0, 0, 0, err
			}
			issues += n
		}
	}
	def := projects[0].ID
	n, err := count(tx.Exec("UPDATE issues SET project_id = ? WHERE project_id IS NULL", def))
	if err != nil {
		return 0, 0, 0, err
	}
	issues += n
	n, err = count(tx.Exec(`UPDATE runs SET project_id = COALESCE(
		(SELECT project_id FROM merge_requests m WHERE m.id = runs.mr_id),
		(SELECT project_id FROM issues i WHERE i.id = runs.issue_id), ?) WHERE project_id IS NULL`, def))
	if err != nil {
		return 0, 0, 0, err
	}
	runs = n
	return mrs, issues, runs, tx.Commit()
}

// SetMRProject / SetIssueProject / SetRunProject assign an object to a project.
func (d *DB) SetMRProject(id int64, projectID *int64) error {
	_, err := d.sql.Exec("UPDATE merge_requests SET project_id = ? WHERE id = ?", nullInt(projectID), id)
	return err
}

func (d *DB) SetIssueProject(id int64, projectID *int64) error {
	_, err := d.sql.Exec("UPDATE issues SET project_id = ? WHERE id = ?", nullInt(projectID), id)
	return err
}

func (d *DB) SetRunProject(id int64, projectID *int64) error {
	_, err := d.sql.Exec("UPDATE runs SET project_id = ? WHERE id = ?", nullInt(projectID), id)
	return err
}

// ProjectOfMR / ProjectOfIssue / ProjectOfRun return the project id of an object (0 = none).
func (d *DB) ProjectOfMR(id int64) int64    { return d.projectOf("merge_requests", id) }
func (d *DB) ProjectOfIssue(id int64) int64 { return d.projectOf("issues", id) }
func (d *DB) ProjectOfRun(id int64) int64   { return d.projectOf("runs", id) }

func (d *DB) projectOf(table string, id int64) int64 {
	var pid sql.NullInt64
	_ = d.sql.QueryRow("SELECT project_id FROM "+table+" WHERE id = ?", id).Scan(&pid)
	return pid.Int64
}

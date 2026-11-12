package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/worktree"
)

// Several repositories (G8). Each project has its checkout, GitLab project, base branch, stand skill and issue
// trackers. The root service is the view of the default project (the first one, created from .env); an object
// (MR, task, run) of another project is handled by that project's view, which differs only in Settings (root,
// branch, GitLab project…), Worktrees and the cached GitLab project, and shares everything else (core).

// ensureProjects creates the default project from .env on the first start and gives existing MRs, tasks and runs
// their project. Nothing is deleted or changed for rows that already have one.
func (s *Service) ensureProjects() {
	if s.DB == nil {
		return
	}
	projects, err := s.DB.ListProjects()
	if err != nil {
		return
	}
	if len(projects) == 0 && s.Settings.ProjectRoot != "" {
		host, path := s.DefaultProject()
		// Only the identity is stored: branch, stand skill and issue trackers stay empty and keep following .env until
		// they are set on the projects page (a later .env edit must still take effect).
		p, err := s.DB.CreateProject(db.Project{Name: filepath.Base(s.Settings.ProjectRoot), GitLabHost: host, ProjectPath: path,
			RepoRoot: s.Settings.ProjectRoot})
		if err != nil {
			return
		}
		projects = []db.Project{*p}
	}
	if len(projects) == 0 {
		return
	}
	s.ProjectID = projects[0].ID
	s.applyDefaultProject(projects[0])
	// Tasks of the default project come from its trackers, else GITLAB_ISSUE_PROJECTS.
	assign := append([]db.Project(nil), projects...)
	if assign[0].IssueProjects == "" {
		assign[0].IssueProjects = strings.Join(s.Settings.GitLabIssueProjects, ",")
	}
	_, _, _, _ = s.DB.AssignProjects(assign)
}

// Projects lists the projects (the default one first).
func (s *Service) Projects() []db.Project {
	list, _ := s.DB.ListProjects()
	return list
}

// DefaultProjectID is the project of the root service (0 when there is none).
func (s *Service) DefaultProjectID() int64 { return s.root.ProjectID }

// ForProject returns the service working for a project: its checkout, base branch, GitLab project, stand skill,
// issue trackers and worktrees. Unknown ids and the default project give the root service.
func (s *Service) ForProject(id int64) *Service {
	root := s.root
	if id == 0 || id == root.ProjectID {
		return root
	}
	p, _ := root.DB.GetProject(id)
	if p == nil {
		return root
	}
	view := *root
	settings := *root.Settings
	settings.ProjectRoot = p.RepoRoot
	settings.ProjectRootError = ""
	settings.BaseBranch = firstOf(p.BaseBranch, settings.BaseBranch)
	settings.StandSkill = p.StandSkill
	settings.GitLabHost = firstOf(p.GitLabHost, settings.GitLabHost)
	settings.GitLabProject = p.ProjectPath
	settings.GitLabIssueProjects = p.IssueProjectList()
	view.Settings = &settings
	view.ProjectID = id
	view.project = nil
	root.mu.Lock()
	m := root.views[id]
	if m == nil || m.Root != p.RepoRoot || m.BaseBranch != settings.BaseBranch {
		// Worktrees of other projects live next to the default project's, one directory per project.
		m = &worktree.Manager{Root: p.RepoRoot, Dir: filepath.Join(root.Settings.WorktreeDir, fmt.Sprintf("project-%d", id)), BaseBranch: settings.BaseBranch}
		root.views[id] = m
	}
	root.mu.Unlock()
	view.Worktrees = m
	return &view
}

// scopedTo is the service for an object of projectID: s itself when it already works for that project (or the
// object has none).
func (s *Service) scopedTo(projectID int64) *Service {
	if projectID == 0 || projectID == s.ProjectID {
		return s
	}
	return s.ForProject(projectID)
}

func (s *Service) forMR(id int64) *Service    { return s.scopedTo(s.DB.ProjectOfMR(id)) }
func (s *Service) forIssue(id int64) *Service { return s.scopedTo(s.DB.ProjectOfIssue(id)) }
func (s *Service) forRun(id int64) *Service   { return s.scopedTo(s.DB.ProjectOfRun(id)) }

// projectForMRRef is the project whose GitLab project an MR belongs to (0 = no repository here).
func (s *Service) projectForMRRef(host, path string) int64 {
	for _, p := range s.Projects() {
		ph, pp := s.ForProject(p.ID).DefaultProject()
		if strings.EqualFold(pp, path) && (ph == "" || strings.EqualFold(ph, host)) {
			return p.ID
		}
	}
	return 0
}

// projectForIssuePath is the project whose tasks live in the issue tracker path: listed in its issue projects,
// else the project itself, else the default project.
func (s *Service) projectForIssuePath(path string) int64 {
	projects := s.Projects()
	for _, p := range projects {
		trackers := p.IssueProjectList()
		if len(trackers) == 0 && p.ID == s.root.ProjectID {
			trackers = s.root.Settings.GitLabIssueProjects // the default project follows .env until set
		}
		for _, tracker := range trackers {
			if strings.EqualFold(tracker, path) {
				return p.ID
			}
		}
	}
	for _, p := range projects {
		if strings.EqualFold(p.ProjectPath, path) {
			return p.ID
		}
	}
	return s.root.ProjectID
}

// assignMR / assignIssue give a stored object its project when it has none yet.
func (s *Service) assignMR(mr *db.MergeRequest) {
	if mr == nil || s.DB.ProjectOfMR(mr.ID) != 0 {
		return
	}
	if id := s.projectForMRRef(mr.GitLabHost, mr.ProjectPath); id != 0 {
		_ = s.DB.SetMRProject(mr.ID, &id)
	}
}

func (s *Service) assignIssue(issue *db.Issue) {
	if issue == nil || s.DB.ProjectOfIssue(issue.ID) != 0 {
		return
	}
	if id := s.projectForIssuePath(issue.ProjectPath); id != 0 {
		_ = s.DB.SetIssueProject(issue.ID, &id)
	}
}

// forFinding is the service of the project a finding's review run belongs to.
func (s *Service) forFinding(id int64) *Service {
	f, _ := s.DB.GetFinding(id)
	if f == nil {
		return s
	}
	return s.forRun(f.RunID)
}

// otherViews are the services of every project but the default one (the root service itself).
func (s *Service) otherViews() []*Service {
	var out []*Service
	for _, p := range s.Projects() {
		if p.ID != s.root.ProjectID {
			out = append(out, s.ForProject(p.ID))
		}
	}
	return out
}

// ForMR / ForIssue / ForRun are the services of an object's project, for pages that show project-dependent
// things (skills, worktrees, review directories).
func (s *Service) ForMR(id int64) *Service    { return s.forMR(id) }
func (s *Service) ForIssue(id int64) *Service { return s.forIssue(id) }
func (s *Service) ForRun(id int64) *Service   { return s.forRun(id) }

// AddProject adds a repository by the path of its checkout: the top of the git work tree, its origin remote gives
// the GitLab project, the remote's default branch (else BASE_BRANCH) the base branch.
func (s *Service) AddProject(repoPath string) (*db.Project, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" || !filepath.IsAbs(repoPath) {
		return nil, userErr("укажите абсолютный путь к рабочей копии репозитория")
	}
	top, err := gitOutput(repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, userErr("%s — не git-репозиторий: %v", repoPath, err)
	}
	for _, p := range s.Projects() {
		if filepath.Clean(p.RepoRoot) == filepath.Clean(top) {
			return nil, userErr("репозиторий %s уже добавлен как «%s»", top, p.Name)
		}
	}
	host, path, ok := gitlab.ProjectFromGitRemote(top)
	if !ok {
		return nil, userErr("у %s нет remote origin на GitLab: проект не связать с MR и задачами", top)
	}
	base := s.root.Settings.BaseBranch
	if head, err := gitOutput(top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		base = strings.TrimPrefix(head, "origin/")
	}
	return s.DB.CreateProject(db.Project{Name: filepath.Base(top), GitLabHost: host, ProjectPath: path, RepoRoot: top, BaseBranch: base})
}

// UpdateProject stores the editable settings of a project (the checkout path is fixed: add another project
// instead). The default project's changes apply to the running dashboard at once.
func (s *Service) UpdateProject(id int64, fields map[string]string) error {
	p, _ := s.DB.GetProject(id)
	if p == nil {
		return userErr("project #%d not found", id)
	}
	if name := strings.TrimSpace(fields["name"]); name != "" {
		p.Name = name
	}
	for key, dst := range map[string]*string{"gitlab_host": &p.GitLabHost, "project_path": &p.ProjectPath, "base_branch": &p.BaseBranch,
		"stand_skill": &p.StandSkill, "issue_projects": &p.IssueProjects} {
		if v, ok := fields[key]; ok {
			*dst = strings.Trim(strings.TrimSpace(v), "/")
		}
	}
	if err := s.DB.UpdateProject(*p); err != nil {
		return err
	}
	if id == s.root.ProjectID {
		s.root.applyDefaultProject(*p)
	}
	return nil
}

// DeleteProject removes a project that has no MRs, tasks or runs; the default project stays.
func (s *Service) DeleteProject(id int64) error {
	if id == s.root.ProjectID {
		return userErr("проект по умолчанию (из .env) удалить нельзя")
	}
	if err := s.DB.DeleteProject(id); err != nil {
		return &UserError{err.Error()}
	}
	return nil
}

// applyDefaultProject puts the stored settings of the default project over .env in the running root service.
func (s *Service) applyDefaultProject(p db.Project) {
	if p.BaseBranch != "" {
		s.Settings.BaseBranch = p.BaseBranch
		if s.Worktrees != nil {
			s.Worktrees.BaseBranch = p.BaseBranch
		}
	}
	if p.StandSkill != "" {
		s.Settings.StandSkill = p.StandSkill
	}
	if list := p.IssueProjectList(); len(list) > 0 {
		s.Settings.GitLabIssueProjects = list
	}
	if p.GitLabHost != "" {
		s.Settings.GitLabHost = p.GitLabHost
	}
	if p.ProjectPath != "" {
		s.Settings.GitLabProject = p.ProjectPath
	}
	s.project = nil // DefaultProject re-reads them
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Package testutil provides fakes for GitLab and runners shared by app and web tests.
package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mr-review/internal/config"
	"mr-review/internal/gitlab"
	"mr-review/internal/runner"
)

// MRPayload builds a GitLab-like MR payload.
func MRPayload(iid int64, sha string) map[string]any {
	return map[string]any{
		"iid": float64(iid), "title": fmt.Sprintf("MR %d", iid),
		"web_url":       fmt.Sprintf("https://gitlab.example.com/group/sub/project/-/merge_requests/%d", iid),
		"author":        map[string]any{"username": "alice"},
		"assignees":     []any{map[string]any{"username": "alice"}},
		"reviewers":     []any{map[string]any{"username": "bob"}},
		"source_branch": "feature", "target_branch": "develop", "state": "opened", "sha": sha,
		"diff_refs":     map[string]any{"head_sha": sha},
		"head_pipeline": map[string]any{"status": "failed"}, "diverged_commits_count": 3.0, "draft": false, "changes_count": "7",
		"labels":     []any{"High", "backend"},
		"references": map[string]any{"full": fmt.Sprintf("group/sub/project!%d", iid)},
		"updated_at": "2026-09-01T10:00:00+03:00",
	}
}

// IssuePayload builds a GitLab-like issue payload.
func IssuePayload(iid int64) map[string]any {
	return map[string]any{
		"iid": float64(iid), "title": fmt.Sprintf("Task %d", iid), "description": "Do the thing",
		"web_url": fmt.Sprintf("https://gitlab.example.com/group/sub/project/-/issues/%d", iid),
		"author":  map[string]any{"username": "bob"}, "state": "opened", "labels": []any{"backend"},
		"references": map[string]any{"full": fmt.Sprintf("group/sub/project#%d", iid)},
		"updated_at": "2026-09-01T10:00:00+03:00",
	}
}

// FakeGitLab is an in-memory gitlab.Client.
type FakeGitLab struct {
	mu         sync.Mutex
	MRs        map[int64]map[string]any
	Issues     map[int64]map[string]any
	Unresolved map[int64]int64
	ApprovedBy map[int64][]string // overrides the approvals of an MR
	Listed     []int64            // MRs returned by ListOpenMRs; nil = every MR in MRs
	Calls      []string
	CreatedMR  string
}

// NewFakeGitLab seeds one MR (!42, authored by alice = the current user) and one issue (#7).
func NewFakeGitLab() *FakeGitLab {
	return &FakeGitLab{MRs: map[int64]map[string]any{42: MRPayload(42, "sha-1")}, Issues: map[int64]map[string]any{7: IssuePayload(7)}, Unresolved: map[int64]int64{42: 2}, ApprovedBy: map[int64][]string{}}
}

// AddProjectSkill writes a project skill (.claude/skills/<name>/SKILL.md) into the fixture project.
func AddProjectSkill(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, ".claude", "skills", name)
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+description+"\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AddProjectAgent writes a project agent (.claude/agents/<name>.md) into the fixture project.
func AddProjectAgent(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, ".claude", "agents")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("---\nname: "+name+"\ndescription: "+description+"\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *FakeGitLab) Detect() (string, string, bool) { return "/fake/glab", "fake", true }
func (f *FakeGitLab) AuthStatus(host string) (bool, string) {
	return true, "Logged in to gitlab.example.com as me"
}
func (f *FakeGitLab) CurrentUser(host string) (string, error) { return "alice", nil }
func (f *FakeGitLab) GetMR(ref gitlab.Ref) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("mr %s!%d@%s", ref.ProjectPath, ref.IID, ref.Host))
	if mr, ok := f.MRs[ref.IID]; ok {
		return mr, nil
	}
	return nil, errors.New("404 Not Found")
}
func (f *FakeGitLab) CountUnresolved(ref gitlab.Ref) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Unresolved[ref.IID], nil
}
func (f *FakeGitLab) GetApprovals(ref gitlab.Ref) (gitlab.Approvals, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if by, ok := f.ApprovedBy[ref.IID]; ok {
		return gitlab.Approvals{Given: int64(len(by)), Required: 2, ApprovedBy: by}, nil
	}
	if ref.IID == 42 {
		return gitlab.Approvals{Given: 1, Required: 2, ApprovedBy: []string{"carol"}}, nil
	}
	return gitlab.Approvals{}, nil
}
func (f *FakeGitLab) ListOpenMRs(host, username string, roles []string, projectPath string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("list-mrs %s %v", projectPath, roles))
	var out []map[string]any
	for iid, mr := range f.MRs {
		if f.Listed != nil && !containsInt(f.Listed, iid) {
			continue
		}
		out = append(out, mr)
	}
	return out, nil
}
func (f *FakeGitLab) GetIssue(ref gitlab.Ref) (map[string]any, error) {
	if issue, ok := f.Issues[ref.IID]; ok {
		return issue, nil
	}
	return nil, errors.New("404 Not Found")
}
func (f *FakeGitLab) ListOpenIssues(host, username, projectPath string) ([]map[string]any, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "issues project="+projectPath)
	f.mu.Unlock()
	var out []map[string]any
	for _, issue := range f.Issues {
		out = append(out, issue)
	}
	return out, nil
}

// Compare returns a fixed summary of "what changed" between two SHAs.
func (f *FakeGitLab) Compare(ref gitlab.Ref, from, to string) (gitlab.Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "compare "+from+".."+to)
	return gitlab.Changes{Commits: 2, Files: 1, Additions: 10, Deletions: 3}, nil
}

func (f *FakeGitLab) CreateMR(host, projectPath, sourceBranch, targetBranch, title, description string) (string, error) {
	f.CreatedMR = fmt.Sprintf("%s|%s|%s|%s|%s", projectPath, sourceBranch, targetBranch, title, description)
	return "https://gitlab.example.com/group/sub/project/-/merge_requests/100", nil
}

func containsInt(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// FakeRunner returns canned structured outputs in order and records requests.
type FakeRunner struct {
	mu        sync.Mutex
	Outputs   []map[string]any
	Requests  []runner.Request
	Block     chan struct{} // when non-nil, Run waits for it (or cancellation)
	FailWith  string
	Texts     []string // plain-text answers for follow-ups
	Session   string
	Ask       []runner.PermissionRequest // permission prompts the fake agent raises before answering
	Decisions []runner.PermissionDecision
}

// AskWrite makes the next run ask for a permission to write a file (like Claude Code outside the working dir).
func (f *FakeRunner) AskWrite(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Ask = append(f.Ask, runner.PermissionRequest{ToolName: "Write", Description: path, Input: json.RawMessage(`{"file_path":"` + path + `"}`),
		Reason: "Path is outside allowed working directories", Suggestions: json.RawMessage(`[{"type":"setMode","mode":"acceptEdits","destination":"session"}]`)})
}

// Lock/Unlock guard Requests for tests that inspect them while runs are in flight.
func (f *FakeRunner) Lock()   { f.mu.Lock() }
func (f *FakeRunner) Unlock() { f.mu.Unlock() }

func (f *FakeRunner) Name() string                   { return "claude" }
func (f *FakeRunner) Detect() (string, string, bool) { return "/fake/claude", "fake", true }
func (f *FakeRunner) Run(ctx context.Context, req runner.Request) (*runner.Result, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	block := f.Block
	f.mu.Unlock()
	if req.Log != nil {
		fmt.Fprintln(req.Log, "fake run")
	}
	if req.Progress != nil {
		req.Progress("Bash: fake command")
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, runner.ErrCancelled
		}
	}
	f.mu.Lock()
	ask := f.Ask
	f.Ask = nil
	f.mu.Unlock()
	var denials []map[string]any
	for _, prompt := range ask {
		decision := runner.PermissionDecision{Message: "no permission callback"}
		if req.Permission != nil {
			decision = req.Permission(ctx, prompt)
		}
		if ctx.Err() != nil {
			return nil, runner.ErrCancelled
		}
		f.mu.Lock()
		f.Decisions = append(f.Decisions, decision)
		f.mu.Unlock()
		if !decision.Allow {
			denials = append(denials, map[string]any{"tool_name": prompt.ToolName, "tool_input": map[string]any{"file_path": prompt.Description}})
		}
	}
	if f.FailWith != "" {
		return nil, errors.New(f.FailWith)
	}
	session := f.Session
	if session == "" {
		session = "sess-1"
	} else if session == "-" { // "-" = the agent reports no session id (Codex/Cursor may not)
		session = ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(req.Schema) == 0 {
		text := "answer"
		if len(f.Texts) > 0 {
			text, f.Texts = f.Texts[0], f.Texts[1:]
		}
		return &runner.Result{Text: text, SessionID: session, CostUSD: 0.01, Usage: runner.Usage{Input: 100, Output: 50}, DurationMs: 3}, nil
	}
	if len(f.Outputs) == 0 {
		return nil, errors.New("fake runner: no canned output left")
	}
	out := f.Outputs[0]
	f.Outputs = f.Outputs[1:]
	raw, _ := json.Marshal(out)
	if req.Mode == runner.ModeEdit {
		_ = os.WriteFile(filepath.Join(req.Dir, "CHANGED.txt"), []byte("changed by fake agent\n"), 0o644)
	}
	res := &runner.Result{Structured: raw, SessionID: session, CostUSD: 0.1, Usage: runner.Usage{Input: 1000, Output: 200, CacheRead: 50000, CacheWrite: 3000}, DurationMs: 5, Raw: []byte(`{"ok":1}`)}
	if len(denials) > 0 {
		res.Denials, _ = json.Marshal(denials)
	}
	return res, nil
}

// FullReviewOutput is a canned full review.
func FullReviewOutput(sha string) map[string]any {
	return map[string]any{
		"summary": "Looks mostly fine.", "verdict": "request_changes", "reviewed_sha": sha,
		"findings": []any{
			map[string]any{"severity": "HIGH", "category": "bug", "file": "src/A.php", "line": 10.0, "title": "Null deref", "description": "d", "suggestion": "s"},
			map[string]any{"severity": "LOW", "category": "style", "file": "src/B.php", "line": nil, "title": "Naming", "description": "d2", "suggestion": ""},
		},
		"unresolved_discussions": []any{
			map[string]any{"author": "bob", "file": "src/A.php", "line": 12.0, "body": "why?", "assessment": "not addressed", "addressed": false},
		},
	}
}

// VerifyOutput is a canned verify result for two previous findings.
func VerifyOutput(first, second int64, sha string) map[string]any {
	return map[string]any{
		"summary": "One fixed.", "verdict": "approve_with_comments", "reviewed_sha": sha,
		"verified": []any{
			map[string]any{"finding_id": float64(first), "status": "fixed", "note": "guard added"},
			map[string]any{"finding_id": float64(second), "status": "open", "note": "unchanged"},
		},
		"new_findings": []any{
			map[string]any{"severity": "MEDIUM", "category": "logic", "file": "src/C.php", "line": 1.0, "title": "New", "description": "n", "suggestion": ""},
		},
		"unresolved_discussions": []any{},
	}
}

// PlanOutput is a canned plan.
func PlanOutput() map[string]any {
	return map[string]any{"summary": "Plan summary", "steps": []any{"one", "two"}, "files": []any{map[string]any{"path": "a.php", "change": "x"}}, "risks": []any{}, "questions": []any{}, "estimate": "S"}
}

// ImplementOutput is a canned implementation report.
func ImplementOutput() map[string]any {
	return map[string]any{"summary": "Implemented", "changes": []any{map[string]any{"path": "CHANGED.txt", "description": "added"}}, "tests": "none", "todo": []any{}, "commit_message": "group/sub/project#7 do the thing"}
}

// GitRepo creates a git repo with an initial commit on `develop` and an `origin` remote pointing at a bare clone.
func GitRepo(t *testing.T, dir string) string {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	run("init", "-q")
	run("symbolic-ref", "HEAD", "refs/heads/develop") // `git init -b` needs git 2.28+

	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# rules\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".claude", "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".claude", "agents", "mr-review.md"), []byte("---\nname: mr-review\ndescription: Review merge requests\n---\nbody\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	bare := dir + ".git-remote"
	if out, err := exec.Command("git", "clone", "-q", "--bare", dir, bare).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v %s", err, out)
	}
	run("remote", "add", "origin", "ssh://git@gitlab.example.com:22122/group/sub/project.git")
	run("remote", "set-url", "origin", bare)
	return dir
}

// Settings builds settings for a dashboard directory next to a project.
func Settings(t *testing.T) (*config.Settings, string) {
	t.Helper()
	// CI runners have no git identity; the worktree commit step needs one.
	for _, kv := range [][2]string{{"GIT_AUTHOR_NAME", "mr-review test"}, {"GIT_AUTHOR_EMAIL", "test@example.com"}, {"GIT_COMMITTER_NAME", "mr-review test"}, {"GIT_COMMITTER_EMAIL", "test@example.com"}} {
		t.Setenv(kv[0], kv[1])
	}
	tmp := t.TempDir()
	project := GitRepo(t, filepath.Join(tmp, "project"))
	dash := filepath.Join(tmp, "dash")
	_ = os.MkdirAll(dash, 0o755)
	s := config.Load(dash, map[string]string{"PROJECT_ROOT": project, "RUN_TIMEOUT_SEC": "30", "GITLAB_HOST": "gitlab.example.com", "GITLAB_PROJECT": "group/sub/project"}, false)
	if s.ProjectRoot == "" {
		t.Fatalf("project root not resolved: %s", s.ProjectRootError)
	}
	return s, project
}

// WaitFor polls until fn returns true.
func WaitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

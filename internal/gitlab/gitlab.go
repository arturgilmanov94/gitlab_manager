// Package gitlab talks to GitLab through the already-authenticated glab CLI. No tokens here.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	mrURLRe    = regexp.MustCompile(`^/(?P<project>.+?)/-/merge_requests/(?P<iid>\d+)(?:[/?#].*)?$`)
	issueURLRe = regexp.MustCompile(`^/(?P<project>.+?)/-/issues/(?P<iid>\d+)(?:[/?#].*)?$`)
	shortRe    = regexp.MustCompile(`^(?P<project>[\w./-]+)(?P<sep>[!#])(?P<iid>\d+)$`)
	remoteSCP  = regexp.MustCompile(`^(?:[^@]+@)?(?P<host>[^:/]+):(?P<path>.+)$`)
)

// Ref identifies an MR or an issue.
type Ref struct {
	Host        string
	ProjectPath string
	IID         int64
}

// EncodedProject is the URL-encoded project path for the API.
func (r Ref) EncodedProject() string { return url.PathEscape(r.ProjectPath) }

// MRURL builds the web URL of a merge request.
func (r Ref) MRURL() string {
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", r.Host, r.ProjectPath, r.IID)
}

// IssueURL builds the web URL of an issue.
func (r Ref) IssueURL() string {
	return fmt.Sprintf("https://%s/%s/-/issues/%d", r.Host, r.ProjectPath, r.IID)
}

func parseRef(text string, pathRe *regexp.Regexp, sep byte, defaultHost, defaultProject string) (Ref, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Ref{}, errors.New("empty reference")
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		u, err := url.Parse(text)
		if err != nil || u.Host == "" {
			return Ref{}, fmt.Errorf("not a GitLab URL: %s", text)
		}
		m := pathRe.FindStringSubmatch(u.Path)
		if m == nil {
			return Ref{}, fmt.Errorf("not a GitLab %s URL: %s", kindName(sep), text)
		}
		iid, _ := strconv.ParseInt(m[2], 10, 64)
		return Ref{u.Host, strings.Trim(m[1], "/"), iid}, nil
	}
	if m := shortRe.FindStringSubmatch(text); m != nil && m[2][0] == sep {
		if defaultHost == "" {
			return Ref{}, errors.New("GitLab host is unknown; use a full URL")
		}
		iid, _ := strconv.ParseInt(m[3], 10, 64)
		return Ref{defaultHost, m[1], iid}, nil
	}
	bare := strings.TrimLeft(text, "!#")
	if iid, err := strconv.ParseInt(bare, 10, 64); err == nil {
		if defaultHost == "" || defaultProject == "" {
			return Ref{}, errors.New("project is unknown; use a full URL")
		}
		return Ref{defaultHost, defaultProject, iid}, nil
	}
	return Ref{}, fmt.Errorf("cannot parse %s reference: %s", kindName(sep), text)
}

func kindName(sep byte) string {
	if sep == '#' {
		return "issue"
	}
	return "merge request"
}

// ParseMRRef accepts a full MR URL, group/project!123, !123 or 123 (with defaults).
func ParseMRRef(text, defaultHost, defaultProject string) (Ref, error) {
	return parseRef(text, mrURLRe, '!', defaultHost, defaultProject)
}

// ParseIssueRef accepts a full issue URL, group/project#123, #123 or 123 (with defaults).
func ParseIssueRef(text, defaultHost, defaultProject string) (Ref, error) {
	return parseRef(text, issueURLRe, '#', defaultHost, defaultProject)
}

// ParseGitRemote extracts (host, project path) from ssh://, scp-like or https remotes.
func ParseGitRemote(remote string) (string, string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", "", false
	}
	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	} else {
		m := remoteSCP.FindStringSubmatch(remote)
		if m == nil {
			return "", "", false
		}
		host, path = m[1], m[2]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return "", "", false
	}
	return host, path, true
}

// ProjectFromGitRemote reads the origin remote of the repository at root.
func ProjectFromGitRemote(root string) (string, string, bool) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}
	return ParseGitRemote(string(out))
}

// Client is the GitLab access used by the application (implemented by Glab, faked in tests).
type Client interface {
	Detect() (path, version string, ok bool)
	AuthStatus(host string) (bool, string)
	CurrentUser(host string) (string, error)
	GetMR(ref Ref) (map[string]any, error)
	GetApprovals(ref Ref) (Approvals, error)
	CountUnresolved(ref Ref) (int64, error)
	ListOpenMRs(host, username string, roles []string, projectPath string) ([]map[string]any, error)
	GetIssue(ref Ref) (map[string]any, error)
	ListOpenIssues(host, username, projectPath string) ([]map[string]any, error)
	CreateMR(host, projectPath, sourceBranch, targetBranch, title, description string) (string, error)
	Compare(ref Ref, from, to string) (Changes, error)
	FailedJobs(ref Ref, pipelineID int64) ([]Job, error)
	JobTrace(ref Ref, jobID int64, tailLines int) (string, error)
}

// Job is one CI job of a pipeline.
type Job struct {
	ID            int64
	Name          string
	Stage         string
	Status        string
	WebURL        string
	FailureReason string
	AllowFailure  bool
}

// Changes summarises what happened between two commits of an MR (the compare API): the "что изменилось" line.
type Changes struct {
	Commits   int
	Files     int
	Additions int
	Deletions int
}

// Glab runs the glab CLI.
type Glab struct {
	Bin     string
	Timeout time.Duration
}

// NewGlab creates a client.
func NewGlab(bin string) *Glab { return &Glab{Bin: bin, Timeout: 90 * time.Second} }

// Detect reports whether glab is available.
func (g *Glab) Detect() (string, string, bool) {
	path, err := exec.LookPath(g.Bin)
	if err != nil {
		return "", "", false
	}
	out, _ := exec.Command(path, "--version").CombinedOutput()
	version := firstLine(string(out))
	return path, version, true
}

// AuthStatus runs `glab auth status`; the summary never contains the token line.
func (g *Glab) AuthStatus(host string) (bool, string) {
	args := []string{"auth", "status"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.Bin, args...).CombinedOutput()
	text := string(out)
	var logged, other string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(strings.TrimSpace(line), "✓✗x! ")
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "token") {
			continue
		}
		if strings.Contains(lower, "logged in to") && logged == "" {
			logged = regexp.MustCompile(`\s*\([^)]*\)\s*$`).ReplaceAllString(trimmed, "")
		} else if trimmed != "" {
			other = trimmed
		}
	}
	if err == nil {
		if logged != "" {
			return true, logged
		}
		return true, "glab auth status: OK"
	}
	if host == "" && logged != "" {
		// Without a host glab checks every configured instance; one being logged in is what matters here.
		return true, logged
	}
	if other == "" {
		other = "glab auth status failed: " + err.Error()
	}
	return false, other
}

// API performs a GET through `glab api`.
func (g *Glab) API(host, path string) (any, error) {
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := lastLine(stderr.String())
		if msg == "" {
			msg = lastLine(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("glab api %s failed: %s", path, msg)
	}
	var data any
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		return nil, fmt.Errorf("glab api %s returned non-JSON output", path)
	}
	return data, nil
}

func (g *Glab) apiObject(host, path string) (map[string]any, error) {
	data, err := g.API(host, path)
	if err != nil {
		return nil, err
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("glab api %s: unexpected payload", path)
	}
	return obj, nil
}

func (g *Glab) apiList(host, path string) ([]map[string]any, error) {
	data, err := g.API(host, path)
	if err != nil {
		return nil, err
	}
	list, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("glab api %s: expected a list", path)
	}
	var out []map[string]any
	for _, item := range list {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out, nil
}

// CurrentUser returns the authenticated username.
func (g *Glab) CurrentUser(host string) (string, error) {
	obj, err := g.apiObject(host, "user")
	if err != nil {
		return "", err
	}
	username := Str(obj, "username")
	if username == "" {
		return "", errors.New("glab api user returned no username")
	}
	return username, nil
}

// GetMR fetches one merge request, including how far the target branch has moved ahead of it.
func (g *Glab) GetMR(ref Ref) (map[string]any, error) {
	obj, err := g.apiObject(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d?include_diverged_commits_count=true", ref.EncodedProject(), ref.IID))
	if err != nil {
		return nil, err
	}
	if _, ok := obj["iid"]; !ok {
		return nil, fmt.Errorf("merge request %s!%d not found", ref.ProjectPath, ref.IID)
	}
	return obj, nil
}

// PipelineID / PipelineURL read the head pipeline identity from an MR payload.
func PipelineID(obj map[string]any) int64 {
	if id := Int(Nested(obj, "head_pipeline"), "id"); id != 0 {
		return id
	}
	return Int(Nested(obj, "pipeline"), "id")
}

func PipelineURL(obj map[string]any) string {
	if u := Str(Nested(obj, "head_pipeline"), "web_url"); u != "" {
		return u
	}
	return Str(Nested(obj, "pipeline"), "web_url")
}

// FailedJobs lists the failed jobs of a pipeline (allow_failure jobs are included and flagged).
func (g *Glab) FailedJobs(ref Ref, pipelineID int64) ([]Job, error) {
	list, err := g.apiList(ref.Host, fmt.Sprintf("projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=100", ref.EncodedProject(), pipelineID))
	if err != nil {
		return nil, err
	}
	return ParseJobs(list), nil
}

// ParseJobs converts a jobs payload.
func ParseJobs(list []map[string]any) []Job {
	var out []Job
	for _, item := range list {
		out = append(out, Job{ID: Int(item, "id"), Name: Str(item, "name"), Stage: Str(item, "stage"), Status: Str(item, "status"),
			WebURL: Str(item, "web_url"), FailureReason: Str(item, "failure_reason"), AllowFailure: Bool(item, "allow_failure")})
	}
	return out
}

// JobTrace returns the last tailLines lines of a job log (the raw trace endpoint is plain text).
func (g *Glab) JobTrace(ref Ref, jobID int64, tailLines int) (string, error) {
	raw, err := g.APIRaw(ref.Host, fmt.Sprintf("projects/%s/jobs/%d/trace", ref.EncodedProject(), jobID))
	if err != nil {
		return "", err
	}
	return TailLines(raw, tailLines), nil
}

// TailLines keeps the last n lines of text (ANSI colour codes and CI section markers stripped).
func TailLines(text string, n int) string {
	text = ansiRe.ReplaceAllString(strings.ReplaceAll(text, "\r\n", "\n"), "")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|section_(start|end):\d+:[a-z_]+`)

// APIRaw performs a GET through `glab api` and returns the body as text (for non-JSON endpoints such as job traces).
func (g *Glab) APIRaw(host, path string) (string, error) {
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := firstNonEmpty(lastLine(stderr.String()), lastLine(stdout.String()), err.Error())
		return "", fmt.Errorf("glab api %s failed: %s", path, msg)
	}
	return stdout.String(), nil
}

// Compare returns commits and line counts between two SHAs (what changed since the last review).
func (g *Glab) Compare(ref Ref, from, to string) (Changes, error) {
	obj, err := g.apiObject(ref.Host, fmt.Sprintf("projects/%s/repository/compare?from=%s&to=%s", ref.EncodedProject(), url.QueryEscape(from), url.QueryEscape(to)))
	if err != nil {
		return Changes{}, err
	}
	return ParseCompare(obj), nil
}

// ParseCompare counts commits, files and +/- lines in a compare payload (diff text is counted line by line).
func ParseCompare(obj map[string]any) Changes {
	var c Changes
	commits, _ := obj["commits"].([]any)
	c.Commits = len(commits)
	diffs, _ := obj["diffs"].([]any)
	for _, item := range diffs {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c.Files++
		for _, line := range strings.Split(Str(d, "diff"), "\n") {
			switch {
			case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			case strings.HasPrefix(line, "+"):
				c.Additions++
			case strings.HasPrefix(line, "-"):
				c.Deletions++
			}
		}
	}
	return c
}

// Approvals is the approval state of a merge request.
type Approvals struct {
	Given      int64
	Required   int64 // 0 when the instance has no approval rules
	ApprovedBy []string
}

// GetApprovals returns who approved the MR and how many approvals it needs.
func (g *Glab) GetApprovals(ref Ref) (Approvals, error) {
	obj, err := g.apiObject(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d/approvals", ref.EncodedProject(), ref.IID))
	if err != nil {
		return Approvals{}, err
	}
	var by []string
	approvedBy, _ := obj["approved_by"].([]any)
	for _, item := range approvedBy {
		entry, _ := item.(map[string]any)
		if name := Str(Nested(entry, "user"), "username"); name != "" {
			by = append(by, name)
		}
	}
	return Approvals{Given: int64(len(approvedBy)), Required: Int(obj, "approvals_required"), ApprovedBy: by}, nil
}

// Usernames lists the usernames in a user array field (assignees, reviewers).
func Usernames(obj map[string]any, key string) []string {
	list, _ := obj[key].([]any)
	var out []string
	for _, item := range list {
		entry, _ := item.(map[string]any)
		if name := Str(entry, "username"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// PipelineStatus reads the head pipeline status of an MR payload ("" when none).
func PipelineStatus(obj map[string]any) string {
	if s := Str(Nested(obj, "head_pipeline"), "status"); s != "" {
		return s
	}
	return Str(Nested(obj, "pipeline"), "status")
}

// Bool reads a boolean field.
func Bool(obj map[string]any, key string) bool {
	v, _ := obj[key].(bool)
	return v
}

// CountUnresolved counts unresolved, resolvable discussions of a merge request.
func (g *Glab) CountUnresolved(ref Ref) (int64, error) {
	items, err := g.apiList(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d/discussions?per_page=100", ref.EncodedProject(), ref.IID))
	if err != nil {
		return 0, err
	}
	var count int64
	for _, disc := range items {
		notes, _ := disc["notes"].([]any)
		if len(notes) == 0 {
			continue
		}
		first, _ := notes[0].(map[string]any)
		resolvable, _ := first["resolvable"].(bool)
		resolved, _ := first["resolved"].(bool)
		if resolvable && !resolved {
			count++
		}
	}
	return count, nil
}

// ListOpenMRs lists open MRs where username has one of the roles (reviewer, assignee, author).
func (g *Glab) ListOpenMRs(host, username string, roles []string, projectPath string) ([]map[string]any, error) {
	base := "merge_requests"
	if projectPath != "" {
		base = "projects/" + url.PathEscape(projectPath) + "/merge_requests"
	}
	seen := map[string]bool{}
	var out []map[string]any
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "reviewer" && role != "assignee" && role != "author" {
			continue
		}
		items, err := g.apiList(host, fmt.Sprintf("%s?state=opened&scope=all&per_page=100&%s_username=%s", base, role, url.QueryEscape(username)))
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			key := Str(item, "web_url")
			if !seen[key] {
				seen[key] = true
				out = append(out, item)
			}
		}
	}
	return out, nil
}

// GetIssue fetches one issue.
func (g *Glab) GetIssue(ref Ref) (map[string]any, error) {
	obj, err := g.apiObject(ref.Host, fmt.Sprintf("projects/%s/issues/%d", ref.EncodedProject(), ref.IID))
	if err != nil {
		return nil, err
	}
	if _, ok := obj["iid"]; !ok {
		return nil, fmt.Errorf("issue %s#%d not found", ref.ProjectPath, ref.IID)
	}
	return obj, nil
}

// ListOpenIssues lists open issues assigned to username.
func (g *Glab) ListOpenIssues(host, username, projectPath string) ([]map[string]any, error) {
	base := "issues"
	if projectPath != "" {
		base = "projects/" + url.PathEscape(projectPath) + "/issues"
	}
	return g.apiList(host, fmt.Sprintf("%s?state=opened&scope=all&per_page=100&assignee_username=%s", base, url.QueryEscape(username)))
}

// CreateMR creates a merge request via the API (POST) and returns its web URL.
func (g *Glab) CreateMR(host, projectPath, sourceBranch, targetBranch, title, description string) (string, error) {
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "--method", "POST", "projects/"+url.PathEscape(projectPath)+"/merge_requests",
		"-f", "source_branch="+sourceBranch, "-f", "target_branch="+targetBranch, "-f", "title="+title,
		"-f", "description="+description, "-f", "remove_source_branch=true")
	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("glab mr create failed: %s", firstNonEmpty(lastLine(stderr.String()), lastLine(stdout.String()), err.Error()))
	}
	var obj map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &obj); err != nil {
		return "", errors.New("unexpected response from GitLab when creating the MR")
	}
	return Str(obj, "web_url"), nil
}

// ---------------------------------------------------------------------------------- payload helpers

// Str reads a string field.
func Str(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

// Int reads an integer field.
func Int(obj map[string]any, key string) int64 {
	switch v := obj[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// Nested reads a nested object.
func Nested(obj map[string]any, key string) map[string]any {
	if v, ok := obj[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// ProjectPathOf derives the project path from references.full or web_url.
func ProjectPathOf(obj map[string]any, sep string) (host, project string, ok bool) {
	full := Str(Nested(obj, "references"), "full")
	if idx := strings.LastIndex(full, sep); idx > 0 {
		project = full[:idx]
	}
	if web := Str(obj, "web_url"); web != "" {
		if u, err := url.Parse(web); err == nil {
			host = u.Host
			if project == "" {
				re := mrURLRe
				if sep == "#" {
					re = issueURLRe
				}
				if m := re.FindStringSubmatch(u.Path); m != nil {
					project = strings.Trim(m[1], "/")
				}
			}
		}
	}
	return host, project, host != "" && project != ""
}

// HeadSHA prefers diff_refs.head_sha, falling back to sha.
func HeadSHA(obj map[string]any) string {
	if sha := Str(Nested(obj, "diff_refs"), "head_sha"); sha != "" {
		return sha
	}
	return Str(obj, "sha")
}

// Labels joins the labels list.
func Labels(obj map[string]any) string {
	list, _ := obj["labels"].([]any)
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

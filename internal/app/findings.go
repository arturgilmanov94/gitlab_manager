package app

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
)

// CodeLine is one line of the code excerpt shown under a finding.
type CodeLine struct {
	N      int64
	Text   string
	Added  bool // added or changed by the MR
	Target bool // the line the finding points at
}

// FindingCode is the excerpt of the reviewed code around a finding, with the MR's own lines marked.
type FindingCode struct {
	File   string
	Line   int64
	SHA    string
	Lines  []CodeLine
	InDiff bool   // the finding's line is part of the MR diff: a GitLab comment lands right on it
	Note   string // why the excerpt is partial ("" = complete)
}

// diffVersion is one MR diff version with the new-file lines each changed file shows.
type diffVersion struct {
	Version gitlab.Version
	Files   map[string]diffFile // by new path
}

type diffFile struct {
	OldPath string
	Lines   map[int64]gitlab.DiffLine
}

const (
	snippetRadius   = 8       // lines shown above and below the finding's line
	maxSnippetBytes = 4 << 20 // larger files are not excerpted
)

// FindingCode returns the code a finding is about: the lines around it in the commit the review saw, the lines the
// MR added marked. Sources: the local git objects (the review worktree fetched the commit), else the GitLab API.
func (s *Service) FindingCode(findingID int64) (*FindingCode, error) {
	if v := s.forFinding(findingID); v != s {
		return v.FindingCode(findingID)
	}
	f, run, mr, err := s.findingTarget(findingID)
	if err != nil {
		return nil, err
	}
	sha := firstOf(run.HeadSHA, mr.HeadSHA)
	out := &FindingCode{File: f.File, SHA: sha}
	if f.Line != nil {
		out.Line = *f.Line
	}
	lines, err := s.fileLines(mr, sha, f.File)
	if err != nil {
		return nil, userErr("не удалось получить %s на коммите %s: %v", f.File, shortID(sha), err)
	}
	var marks map[int64]gitlab.DiffLine
	if v := s.diffVersionFor(mr, sha); v != nil {
		if file, ok := v.Files[f.File]; ok {
			marks = file.Lines
		}
	} else {
		out.Note = "изменённые строки не отмечены: версия diff для этого коммита недоступна"
	}
	from, to := int64(1), int64(len(lines))
	if out.Line > 0 {
		if out.Line > int64(len(lines)) {
			out.Note = fmt.Sprintf("в файле %d строк — строка %d за его пределами (код мог измениться)", len(lines), out.Line)
			from = max(1, int64(len(lines))-2*snippetRadius)
		} else {
			from, to = max(1, out.Line-snippetRadius), min(int64(len(lines)), out.Line+snippetRadius)
		}
	} else {
		to = min(to, 2*snippetRadius)
	}
	for n := from; n <= to && n <= int64(len(lines)); n++ {
		mark := marks[n]
		out.Lines = append(out.Lines, CodeLine{N: n, Text: lines[n-1], Added: mark.Added, Target: n == out.Line})
	}
	if _, ok := marks[out.Line]; ok && out.Line > 0 {
		out.InDiff = true
	}
	return out, nil
}

// PostResult is what «Отправить в GitLab» did.
type PostResult struct {
	URL    string
	OnLine bool // posted as a diff comment on the finding's line (false = a general comment of the MR)
}

// PostFinding posts the (edited) text of a finding to the MR in GitLab: as a diff comment on its line when the line is
// part of the reviewed MR version, otherwise as an MR comment that names the file and line. The note link is stored.
func (s *Service) PostFinding(findingID int64, body string) (*PostResult, error) {
	if v := s.forFinding(findingID); v != s {
		return v.PostFinding(findingID, body)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, userErr("пустой комментарий")
	}
	f, run, mr, err := s.findingTarget(findingID)
	if err != nil {
		return nil, err
	}
	if f.GitLabNoteURL != "" {
		return nil, userErr("замечание уже отправлено в GitLab: %s", f.GitLabNoteURL)
	}
	if mr.Closed() {
		return nil, userErr("MR уже закрыт или влит")
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	sha := firstOf(run.HeadSHA, mr.HeadSHA)
	var pos *gitlab.Position
	if f.File != "" && f.Line != nil && *f.Line > 0 {
		if v := s.diffVersionFor(mr, sha); v != nil {
			if file, ok := v.Files[f.File]; ok {
				if line, ok := file.Lines[*f.Line]; ok {
					pos = &gitlab.Position{BaseSHA: v.Version.BaseSHA, StartSHA: v.Version.StartSHA, HeadSHA: v.Version.HeadSHA,
						OldPath: file.OldPath, NewPath: f.File, NewLine: *f.Line}
					if !line.Added {
						pos.OldLine = line.OldLine
					}
				}
			}
		}
	}
	var noteID int64
	onLine := false
	if pos != nil {
		noteID, err = s.GitLab.CreateDiscussion(ref, body, pos)
		onLine = err == nil
	}
	if !onLine {
		general := body
		if where := findingWhere(f); where != "" {
			general = where + "\n\n" + body
		}
		if noteID, err = s.GitLab.CreateDiscussion(ref, general, nil); err != nil {
			return nil, &UserError{err.Error()}
		}
	}
	link := mr.WebURL
	if noteID > 0 {
		link = fmt.Sprintf("%s#note_%d", mr.WebURL, noteID)
	}
	_ = s.DB.SetFindingGitLabNote(f.ID, link)
	return &PostResult{URL: link, OnLine: onLine}, nil
}

// FindingComment is the default text of a finding posted to GitLab; the developer edits it before sending.
func FindingComment(f db.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** · %s", f.Title, strings.ToLower(f.Severity))
	if f.Category != "" {
		fmt.Fprintf(&b, " · %s", f.Category)
	}
	if d := strings.TrimSpace(f.Description); d != "" {
		b.WriteString("\n\n" + d)
	}
	if sg := strings.TrimSpace(f.Suggestion); sg != "" {
		b.WriteString("\n\n**Предложение:**\n" + sg)
	}
	return b.String()
}

// findingWhere names the file and line of a finding for a general (not on a line) MR comment.
func findingWhere(f *db.Finding) string {
	if f.File == "" {
		return ""
	}
	if f.Line != nil && *f.Line > 0 {
		return fmt.Sprintf("`%s:%d`", f.File, *f.Line)
	}
	return "`" + f.File + "`"
}

func (s *Service) findingTarget(findingID int64) (*db.Finding, *db.Run, *db.MergeRequest, error) {
	f, _ := s.DB.GetFinding(findingID)
	if f == nil {
		return nil, nil, nil, userErr("finding #%d not found", findingID)
	}
	run, _ := s.DB.GetRun(f.RunID)
	if run == nil || run.MRID == nil {
		return nil, nil, nil, userErr("finding #%d is not attached to a merge request", findingID)
	}
	mr, _ := s.DB.GetMR(*run.MRID)
	if mr == nil {
		return nil, nil, nil, userErr("merge request of finding #%d not found", findingID)
	}
	return f, run, mr, nil
}

// fileLines returns a file of the MR's repository at sha, split into lines (cached).
func (s *Service) fileLines(mr *db.MergeRequest, sha, path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("у замечания не указан файл")
	}
	key := fmt.Sprintf("%s/%s %s %s", mr.GitLabHost, mr.ProjectPath, sha, path)
	s.mu.Lock()
	cached, ok := s.snippets[key]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	text, err := s.localFile(sha, path)
	if err != nil {
		text, err = s.GitLab.RawFile(gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}, path, sha)
		if err != nil {
			return nil, err
		}
	}
	if len(text) > maxSnippetBytes {
		return nil, fmt.Errorf("файл больше %d МБ", maxSnippetBytes>>20)
	}
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	s.mu.Lock()
	if s.snippets == nil || len(s.snippets) > 500 {
		s.snippets = map[string][]string{}
	}
	s.snippets[key] = lines
	s.mu.Unlock()
	return lines, nil
}

// localFile reads path at sha from the project's git objects (no network).
func (s *Service) localFile(sha, path string) (string, error) {
	if s.Settings.ProjectRoot == "" || sha == "" {
		return "", fmt.Errorf("no local repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", s.Settings.ProjectRoot, "show", sha+":"+path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// diffVersionFor finds the MR diff version whose head is sha, with its per-file line maps (cached; nil = unknown).
func (s *Service) diffVersionFor(mr *db.MergeRequest, sha string) *diffVersion {
	if sha == "" {
		return nil
	}
	key := fmt.Sprintf("%s/%s!%d %s", mr.GitLabHost, mr.ProjectPath, mr.IID, sha)
	s.mu.Lock()
	cached, ok := s.versions[key]
	s.mu.Unlock()
	if ok {
		return cached
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	versions, err := s.GitLab.MRVersions(ref)
	if err != nil {
		return nil // not cached: GitLab may be back next time
	}
	var found *diffVersion
	for _, v := range versions {
		if v.HeadSHA != sha {
			continue
		}
		diffs, err := s.GitLab.MRVersionDiffs(ref, v.ID)
		if err != nil {
			return nil
		}
		found = &diffVersion{Version: v, Files: map[string]diffFile{}}
		for _, d := range diffs {
			found.Files[d.NewPath] = diffFile{OldPath: d.OldPath, Lines: gitlab.NewFileLines(d.Diff)}
		}
		break
	}
	s.mu.Lock()
	if s.versions == nil || len(s.versions) > 200 {
		s.versions = map[string]*diffVersion{}
	}
	s.versions[key] = found
	s.mu.Unlock()
	return found
}

func shortID(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

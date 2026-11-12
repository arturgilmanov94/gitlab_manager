package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/prompts"
)

// The «Изменения» tab (T14): the diff of an MR inside the dashboard, whole or only what arrived since the
// developer last looked at it or since the last AI review, with the findings and threads marked on their lines.

// Diff modes.
const (
	DiffAll    = "all"    // the whole MR: target branch → head
	DiffSeen   = "seen"   // since «Просмотрено» was pressed
	DiffReview = "review" // since the last finished AI review
)

// DiffMode is one choice of the mode switch.
type DiffMode struct {
	Key, Label string
	From       string // the commit the diff starts at ("" for the whole MR)
	Available  bool
	Hint       string // why it is not available, or what it shows
}

// DiffFile is one changed file in the list.
type DiffFile struct {
	Index                int
	Path, OldPath        string
	Status               string // A added | D deleted | R renamed | M modified
	Additions, Deletions int
	Marks                int  // findings and threads on this file
	Folded               bool // large or deleted: opened by hand
	Empty                bool // GitLab gave no text (binary or too large)
}

// DiffView is the file list of one mode.
type DiffView struct {
	Mode                 string
	Modes                []DiffMode
	From, To             string
	Files                []DiffFile
	Additions, Deletions int
	Note                 string // why the list is empty
}

// DiffMark is a finding or a thread shown under a line of the diff.
type DiffMark struct {
	Kind     string // finding | thread
	ID       int64
	Severity string // findings
	Status   string // findings: open, fixed…; threads: open | resolved
	Author   string // threads
	Text     string
}

// DiffRow is one line of a file's diff.
type DiffRow struct {
	Kind     string // hunk | add | del | ctx
	Old, New int64
	Text     string
	Marks    []DiffMark
}

// DiffBody is the diff of one file.
type DiffBody struct {
	File  DiffFile
	Rows  []DiffRow
	Loose []DiffMark // marks whose line the diff does not show
	Note  string
}

const (
	foldDiffLines = 400  // files with a longer diff start folded
	maxDiffRows   = 5000 // a longer diff is cut (GitLab link for the rest)
)

func (s *Service) diffModes(mr *db.MergeRequest) []DiffMode {
	modes := []DiffMode{{Key: DiffAll, Label: "Весь MR", Available: true, Hint: "все изменения MR относительно " + mr.TargetBranch}}
	seen := DiffMode{Key: DiffSeen, Label: "С моего просмотра", From: s.DB.SeenSHA(mr.ID)}
	switch {
	case seen.From == "":
		seen.Hint = "появится после отметки «Просмотрено»: dashboard запомнит коммит, который вы видели"
	case seen.From == mr.HeadSHA:
		seen.Hint = "после вашего просмотра (" + shortID(seen.From) + ") новых коммитов нет"
	default:
		seen.Available, seen.Hint = true, "что изменилось после вашего просмотра на "+shortID(seen.From)
	}
	review := DiffMode{Key: DiffReview, Label: "С последнего AI-ревью"}
	if latest, _ := s.DB.LatestDoneReview(mr.ID); latest != nil {
		review.From = latest.HeadSHA
	}
	switch {
	case review.From == "":
		review.Hint = "появится после первого AI-ревью"
	case review.From == mr.HeadSHA:
		review.Hint = "после AI-ревью (" + shortID(review.From) + ") новых коммитов нет"
	default:
		review.Available, review.Hint = true, "что изменилось после AI-ревью на "+shortID(review.From)
	}
	return append(modes, seen, review)
}

// mrDiffs returns the per-file diffs of a mode (cached per head and range).
func (s *Service) mrDiffs(mr *db.MergeRequest, from string) ([]gitlab.FileDiff, error) {
	key := fmt.Sprintf("%s/%s!%d %s..%s", mr.GitLabHost, mr.ProjectPath, mr.IID, from, mr.HeadSHA)
	s.mu.Lock()
	cached, ok := s.diffs[key]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	var list []gitlab.FileDiff
	var err error
	if from == "" {
		list, err = s.GitLab.MRDiffs(ref)
	} else {
		list, err = s.GitLab.CompareDiffs(ref, from, mr.HeadSHA)
	}
	if err != nil {
		return nil, &UserError{"не удалось получить изменения из GitLab: " + err.Error()}
	}
	s.mu.Lock()
	if s.diffs == nil || len(s.diffs) > 40 {
		s.diffs = map[string][]gitlab.FileDiff{}
	}
	s.diffs[key] = list
	s.mu.Unlock()
	return list, nil
}

func (s *Service) diffTarget(mrID int64, mode string) (*db.MergeRequest, DiffMode, []DiffMode, error) {
	mr, _ := s.DB.GetMR(mrID)
	if mr == nil {
		return nil, DiffMode{}, nil, userErr("merge request #%d is not in the dashboard", mrID)
	}
	modes := s.diffModes(mr)
	for _, m := range modes {
		if m.Key == mode {
			return mr, m, modes, nil
		}
	}
	return mr, modes[0], modes, nil
}

// MRDiff returns the list of changed files of an MR in a mode.
func (s *Service) MRDiff(mrID int64, mode string) (*DiffView, error) {
	if v := s.forMR(mrID); v != s {
		return v.MRDiff(mrID, mode)
	}
	mr, m, modes, err := s.diffTarget(mrID, mode)
	if err != nil {
		return nil, err
	}
	view := &DiffView{Mode: m.Key, Modes: modes, From: m.From, To: mr.HeadSHA}
	if !m.Available {
		view.Note = m.Hint
		return view, nil
	}
	diffs, err := s.mrDiffs(mr, m.From)
	if err != nil {
		return nil, err
	}
	marks := s.diffMarks(mr)
	for i, d := range diffs {
		f := fileOf(i, d)
		f.Marks = len(marks[f.Path])
		view.Additions, view.Deletions = view.Additions+f.Additions, view.Deletions+f.Deletions
		view.Files = append(view.Files, f)
	}
	if len(view.Files) == 0 {
		view.Note = "изменений нет"
	}
	return view, nil
}

func fileOf(i int, d gitlab.FileDiff) DiffFile {
	f := DiffFile{Index: i, Path: firstOf(d.NewPath, d.OldPath), OldPath: d.OldPath, Status: "M", Empty: strings.TrimSpace(d.Diff) == ""}
	switch {
	case d.New:
		f.Status = "A"
	case d.Deleted:
		f.Status = "D"
	case d.Renamed:
		f.Status = "R"
	}
	lines := 0
	for _, line := range strings.Split(d.Diff, "\n") {
		lines++
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			f.Additions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			f.Deletions++
		}
	}
	f.Folded = lines > foldDiffLines || d.Deleted
	return f
}

// diffMarks collects what is attached to lines of the MR's files: the findings of the latest review that are still
// worth looking at and the live threads, per file.
func (s *Service) diffMarks(mr *db.MergeRequest) map[string][]lineMark {
	out := map[string][]lineMark{}
	if latest, _ := s.DB.LatestDoneReview(mr.ID); latest != nil {
		findings, _ := s.DB.ListFindings(latest.ID)
		for _, f := range findings {
			if f.File == "" || f.Status == "false_positive" || f.Status == "ignored" {
				continue
			}
			var line int64
			if f.Line != nil {
				line = *f.Line
			}
			out[f.File] = append(out[f.File], lineMark{Line: line, Mark: DiffMark{Kind: "finding", ID: f.ID, Severity: f.Severity, Status: f.Status, Text: f.Title}})
		}
	}
	threads, _ := s.DB.ListMRDiscussions(mr.ID)
	for _, d := range threads {
		if d.File == "" || !d.Resolvable {
			continue
		}
		status := "open"
		if d.Resolved {
			status = "resolved"
		}
		out[d.File] = append(out[d.File], lineMark{Line: d.Line, OldLine: d.OldLine, Mark: DiffMark{Kind: "thread", ID: d.ID, Status: status, Author: d.Author, Text: clip(firstLine(d.First().Body), 160)}})
	}
	return out
}

type lineMark struct {
	Line, OldLine int64 // new-file line; OldLine alone = a removed line
	Mark          DiffMark
}

var diffHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// MRDiffFile returns the diff of one file of a mode, line by line, with marks.
func (s *Service) MRDiffFile(mrID int64, mode string, index int) (*DiffBody, error) {
	if v := s.forMR(mrID); v != s {
		return v.MRDiffFile(mrID, mode, index)
	}
	mr, m, _, err := s.diffTarget(mrID, mode)
	if err != nil {
		return nil, err
	}
	if !m.Available {
		return nil, userErr("%s", m.Hint)
	}
	diffs, err := s.mrDiffs(mr, m.From)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(diffs) {
		return nil, userErr("файл не найден: изменения обновились, перезагрузите страницу")
	}
	d := diffs[index]
	body := &DiffBody{File: fileOf(index, d)}
	if body.File.Empty {
		body.Note = "GitLab не отдал текст изменений этого файла: бинарный или слишком большой"
		if d.Renamed {
			body.Note = "файл переименован без изменений: " + d.OldPath + " → " + d.NewPath
		}
	}
	var oldN, newN int64
	for _, line := range strings.Split(strings.TrimRight(d.Diff, "\n"), "\n") {
		if len(body.Rows) >= maxDiffRows {
			body.Note = fmt.Sprintf("показаны первые %d строк изменений — остальное в GitLab", maxDiffRows)
			break
		}
		if g := diffHunkRe.FindStringSubmatch(line); g != nil {
			oldN, _ = strconv.ParseInt(g[1], 10, 64)
			newN, _ = strconv.ParseInt(g[2], 10, 64)
			body.Rows = append(body.Rows, DiffRow{Kind: "hunk", Text: line})
			continue
		}
		if line == "" || oldN+newN == 0 {
			continue
		}
		switch line[0] {
		case '+':
			body.Rows = append(body.Rows, DiffRow{Kind: "add", New: newN, Text: line[1:]})
			newN++
		case '-':
			body.Rows = append(body.Rows, DiffRow{Kind: "del", Old: oldN, Text: line[1:]})
			oldN++
		case ' ':
			body.Rows = append(body.Rows, DiffRow{Kind: "ctx", Old: oldN, New: newN, Text: line[1:]})
			oldN++
			newN++
		}
	}
	for _, mark := range s.diffMarks(mr)[body.File.Path] {
		placed := false
		for i := range body.Rows {
			row := &body.Rows[i]
			if row.Kind == "hunk" {
				continue
			}
			if (mark.Line > 0 && row.New == mark.Line && row.Kind != "del") || (mark.Line == 0 && mark.OldLine > 0 && row.Old == mark.OldLine && row.Kind != "add") {
				row.Marks = append(row.Marks, mark.Mark)
				placed = true
				break
			}
		}
		if !placed {
			loose := mark.Mark
			if n := firstPositive(mark.Line, mark.OldLine); n > 0 {
				loose.Text = fmt.Sprintf("строка %d · %s", n, loose.Text)
			}
			body.Loose = append(body.Loose, loose)
		}
	}
	return body, nil
}

// incrementalContext builds the prefetched context of a verify run: what changed between the previously reviewed
// SHA and the head, limited to the files of the MR (after a rebase the raw range also carries the target
// branch's commits — those are not the MR's changes). nil = not applicable, the full context is used.
func (s *Service) incrementalContext(mr *db.MergeRequest, baseSHA string, full *prompts.MRContext) *prompts.MRContext {
	if full == nil || baseSHA == "" || mr.HeadSHA == "" || baseSHA == mr.HeadSHA {
		return nil
	}
	increment, err := s.mrDiffs(mr, baseSHA)
	if err != nil {
		return nil
	}
	whole, err := s.mrDiffs(mr, "")
	if err != nil {
		return nil
	}
	inMR := map[string]bool{}
	for _, d := range whole {
		inMR[d.NewPath], inMR[d.OldPath] = true, true
	}
	ctx := &prompts.MRContext{Worktree: full.Worktree, Threads: full.Threads, Since: baseSHA}
	var text strings.Builder
	for _, d := range increment {
		if !inMR[d.NewPath] && !inMR[d.OldPath] {
			continue
		}
		change := "M"
		switch {
		case d.New:
			change = "A"
		case d.Deleted:
			change = "D"
		case d.Renamed:
			change = "R " + d.OldPath + " ->"
		}
		ctx.Files = append(ctx.Files, prompts.FileChange{Path: d.NewPath, Change: change})
		fmt.Fprintf(&text, "--- a/%s\n+++ b/%s\n%s\n", d.OldPath, d.NewPath, strings.TrimRight(d.Diff, "\n"))
	}
	if max := s.Settings.PrefetchMaxDiffChars; max > 0 && text.Len() <= max {
		ctx.Diff = strings.TrimSpace(text.String())
	} else {
		ctx.DiffTruncated = true
	}
	return ctx
}

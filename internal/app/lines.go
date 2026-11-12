package app

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A verify run carries the still-open findings of the review it is based on, and those findings point at lines
// of the head that review read. The new head usually has them somewhere else: the MR gained commits, the file
// moved down. Shown next to the current diff, «file:183» then names code nobody wrote there, and the link to
// the blob at the verified commit opens the wrong place — the developer sees 193 in GitLab.
//
// The lines are therefore re-anchored to the head being verified, from the diff between the two commits.

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// shiftLine maps a line of the old file to the new one using the hunk headers of `git diff -U0 old new`.
// A line inside a rewritten block lands on the start of that block in the new file; a line the diff does not
// touch keeps its place plus everything the hunks above it added or removed. An empty diff changes nothing.
// ok is false when the code the line pointed at is gone at the new head: the finding then names the file alone.
func shiftLine(diff string, line int64) (moved int64, ok bool) {
	if line <= 0 {
		return line, true
	}
	delta := int64(0)
	for _, raw := range strings.Split(diff, "\n") {
		m := hunkHeader.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		oldStart, oldCount := atoi(m[1]), hunkCount(m[2])
		newStart, newCount := atoi(m[3]), hunkCount(m[4])
		if line < oldStart {
			break // the line is above this hunk, only the hunks before it moved it
		}
		if oldCount > 0 && line < oldStart+oldCount {
			if newCount == 0 {
				return 0, false // the block was deleted at the new head
			}
			return newStart, true // rewritten in place: the start of what replaced it
		}
		delta += newCount - oldCount
	}
	if line+delta <= 0 {
		return 0, false
	}
	return line + delta, true
}

func atoi(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// hunkCount is the line count of one side of a hunk header; git omits it when it is 1.
func hunkCount(s string) int64 {
	if s == "" {
		return 1
	}
	return atoi(s)
}

// lineShift returns a function that moves a line of baseSHA to headSHA, one git call per file. Anything that
// can go wrong — no git, a commit already pruned, the dashboard's checkout gone — keeps the line as it was:
// a stale number is no worse than today's behaviour, and the note under the finding still explains itself.
func (s *Service) lineShift(baseSHA, headSHA string) func(file string, line *int64) *int64 {
	keep := func(_ string, line *int64) *int64 { return line }
	if s.Worktrees == nil || baseSHA == "" || headSHA == "" || baseSHA == headSHA {
		return keep
	}
	diffs := map[string]string{}
	return func(file string, line *int64) *int64 {
		if file == "" || line == nil || *line <= 0 {
			return line
		}
		diff, ok := diffs[file]
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			diff, _ = s.Worktrees.FileHunks(ctx, baseSHA, headSHA, file)
			cancel()
			diffs[file] = diff
		}
		moved, ok := shiftLine(diff, *line)
		if !ok {
			return nil // the code is gone at this head; the file and the verify note still say enough
		}
		if moved == *line {
			return line
		}
		return &moved
	}
}

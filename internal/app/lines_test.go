package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mr-review/internal/testutil"
	"mr-review/internal/worktree"
)

func TestShiftLine(t *testing.T) {
	// 13 lines added above the finding (the case from !114343: parseRecord moved from 180 to 193).
	added := "@@ -176,0 +177,13 @@\n+a\n"
	// A block rewritten in place, and one removed above it.
	rewritten := "@@ -10,3 +10,5 @@\n-x\n+y\n@@ -40,2 +42,0 @@\n-gone\n"
	cases := []struct {
		name string
		diff string
		line int64
		want int64
		ok   bool
	}{
		{"no diff at all", "", 183, 183, true},
		{"insertion above moves the line down", added, 183, 196, true},
		{"a line above the insertion stays", added, 100, 100, true},
		{"line inside a rewritten block lands on its new start", rewritten, 11, 10, true},
		{"line below both hunks keeps the total shift", rewritten, 100, 100, true},
		{"line between the hunks follows the first one", rewritten, 30, 32, true},
		{"a deleted line has nowhere to go", rewritten, 41, 0, false},
		{"zero and negative lines are left alone", added, 0, 0, true},
		{"a diff without hunk headers changes nothing", "diff --git a/f b/f\n", 5, 5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shiftLine(c.diff, c.line)
			if got != c.want || ok != c.ok {
				t.Fatalf("shiftLine(%d) = %d, %v; want %d, %v", c.line, got, ok, c.want, c.ok)
			}
		})
	}
}

// The shift runs on real commits: the finding of an earlier review points at the line the code has now.
func TestLineShiftOverGitHistory(t *testing.T) {
	dir := t.TempDir()
	testutil.GitRepo(t, dir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	file := "scanner.php"
	body := func(prefix int) []byte {
		lines := make([]string, 0, prefix+3)
		for i := 0; i < prefix; i++ {
			lines = append(lines, "// filler")
		}
		return []byte(strings.Join(append(lines, "function parseRecord()", "{", "}"), "\n") + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, file), body(10), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "first head")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, file), body(23), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "13 lines more")
	head := git("rev-parse", "HEAD")

	svc := &Service{Worktrees: &worktree.Manager{Root: dir}}
	line := int64(11) // "function parseRecord()" at the base head
	shift := svc.lineShift(base, head)
	if got := shift(file, &line); got == nil || *got != 24 {
		t.Fatalf("line %d of %s must move to 24 at the new head, got %v", line, file, derefLine(got))
	}
	if got := shift("never-touched.php", &line); got == nil || *got != 11 {
		t.Fatal("a file that did not change keeps the line")
	}
	if got := shift(file, &line); got == nil || *got != 24 {
		t.Fatal("the second call for the same file is served from the cache with the same answer")
	}
	// Code deleted at the new head leaves the finding without a line: the file and the note still say enough.
	if err := os.WriteFile(filepath.Join(dir, file), []byte("// filler\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "function gone")
	if got := svc.lineShift(base, git("rev-parse", "HEAD"))(file, &line); got != nil {
		t.Fatalf("a deleted line must not be carried over, got %v", *got)
	}
	// A pruned or unknown commit must not lose the line.
	if got := svc.lineShift("0000000000000000000000000000000000000000", head)(file, &line); got == nil || *got != 11 {
		t.Fatal("an unreachable base commit keeps the line as it was")
	}
	if got := svc.lineShift(head, head)(file, &line); got == nil || *got != 11 {
		t.Fatal("the same commit on both sides keeps the line")
	}
}

func derefLine(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

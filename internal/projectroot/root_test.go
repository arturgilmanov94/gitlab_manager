package projectroot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInit(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func TestExplicitRoot(t *testing.T) {
	tmp := t.TempDir()
	project := gitInit(t, filepath.Join(tmp, "project"))
	dash := filepath.Join(tmp, "elsewhere", "dash")
	_ = os.MkdirAll(dash, 0o755)
	got, err := Resolve(dash, project, tmp)
	if err != nil || got.Source != "env" || got.Path != canonical(project) {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := Resolve(dash, "../../project", tmp); err != nil { // relative to the dashboard dir
		t.Fatalf("relative PROJECT_ROOT: %v", err)
	}
	if _, err := Resolve(dash, "/definitely/missing", tmp); err == nil || !strings.Contains(err.Error(), "missing directory") {
		t.Fatalf("expected missing directory error, got %v", err)
	}
}

func TestInsideProject(t *testing.T) {
	tmp := t.TempDir()
	project := gitInit(t, filepath.Join(tmp, "project"))
	dash := filepath.Join(project, "tools", "dash")
	_ = os.MkdirAll(dash, 0o755)
	got, err := Resolve(dash, "", tmp)
	if err != nil || got.Source != "git:dashboard" || got.Path != canonical(project) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestDashboardOwnRepoInsideProject(t *testing.T) {
	tmp := t.TempDir()
	project := gitInit(t, filepath.Join(tmp, "project"))
	dash := gitInit(t, filepath.Join(project, "tools", "dash"))
	got, err := Resolve(dash, "", tmp)
	if err != nil || got.Path != canonical(project) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestSibling(t *testing.T) {
	tmp := t.TempDir()
	gitInit(t, filepath.Join(tmp, "other")) // no Claude instructions -> ignored
	project := gitInit(t, filepath.Join(tmp, "main"))
	_ = os.WriteFile(filepath.Join(project, "CLAUDE.md"), []byte("# rules"), 0o644)
	dash := gitInit(t, filepath.Join(tmp, "mr-review"))
	got, err := Resolve(dash, "", dash)
	if err != nil || got.Source != "sibling" || got.Path != canonical(project) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestAmbiguousSiblings(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"a", "b"} {
		p := gitInit(t, filepath.Join(tmp, name))
		_ = os.MkdirAll(filepath.Join(p, ".claude", "agents"), 0o755)
	}
	settingsOnly := gitInit(t, filepath.Join(tmp, "c")) // .claude with settings only must not count
	_ = os.MkdirAll(filepath.Join(settingsOnly, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(settingsOnly, ".claude", "settings.local.json"), []byte("{}"), 0o644)
	dash := filepath.Join(tmp, "dash")
	_ = os.MkdirAll(dash, 0o755)
	_, err := Resolve(dash, "", dash)
	if err == nil || !strings.Contains(err.Error(), "Several sibling") || !strings.Contains(err.Error(), filepath.Join(tmp, "a")) || strings.Contains(err.Error(), filepath.Join(tmp, "c")) {
		t.Fatalf("expected ambiguity error without c, got %v", err)
	}
}

func TestNothingFound(t *testing.T) {
	tmp := t.TempDir()
	dash := filepath.Join(tmp, "isolated", "dash")
	_ = os.MkdirAll(dash, 0o755)
	_, err := Resolve(dash, "", filepath.Join(tmp, "isolated"))
	if err == nil || !strings.Contains(err.Error(), "PROJECT_ROOT=/path/to/project") {
		t.Fatalf("expected instructions, got %v", err)
	}
}

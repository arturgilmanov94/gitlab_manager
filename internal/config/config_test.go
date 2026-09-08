package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(path, []byte("# c\nHOST=0.0.0.0\nexport PORT=\"9000\"\nEMPTY=\nBAD LINE\n"), 0o644)
	got := ParseEnvFile(path)
	want := map[string]string{"HOST": "0.0.0.0", "PORT": "9000", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestLoadPrecedenceAndPaths(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("PORT=9999\nDATABASE_PATH=./data/x.sqlite\nCLAUDE_EXTRA_ALLOWED_TOOLS=Bash(php *) WebFetch\nPROJECT_ROOT=/missing/root\n"), 0o644)
	s := Load(dir, map[string]string{"PORT": "1234"}, true)
	if s.Port != 1234 {
		t.Fatalf("environment must win over .env: %d", s.Port)
	}
	if s.DatabasePath != filepath.Join(dir, "data", "x.sqlite") {
		t.Fatalf("relative path not resolved: %s", s.DatabasePath)
	}
	if !reflect.DeepEqual(s.ClaudeExtraTools, []string{"Bash(php *)", "WebFetch"}) {
		t.Fatalf("tools: %v", s.ClaudeExtraTools)
	}
	if s.ProjectRoot != "" || s.ProjectRootError == "" {
		t.Fatalf("missing PROJECT_ROOT must be reported, not fatal: %+v", s)
	}
}

func TestSplitTools(t *testing.T) {
	got := SplitTools("Read Bash(git log *)  Grep")
	if !reflect.DeepEqual(got, []string{"Read", "Bash(git log *)", "Grep"}) {
		t.Fatalf("%v", got)
	}
}

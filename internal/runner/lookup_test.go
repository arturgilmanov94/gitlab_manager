package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookAgentFindsNodeVersionManagerBin(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".nvm", "versions", "node", "v20.20.0", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "empty")) // what a .desktop launch sees: no nvm directory

	path, ok := LookAgent("codex")
	if !ok || path != filepath.Join(bin, "codex") {
		t.Fatalf("LookAgent = %q %v", path, ok)
	}
	if _, ok := LookAgent("nosuchagent"); ok {
		t.Fatal("unknown binary must not be found")
	}
	if _, ok := LookAgent(""); ok {
		t.Fatal("empty name must not be found")
	}
}

func TestLookAgentExplicitPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if path, ok := LookAgent(exe); !ok || path != exe {
		t.Fatalf("LookAgent(%q) = %q %v", exe, path, ok)
	}
	if err := os.Chmod(exe, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookAgent(exe); ok {
		t.Fatal("non-executable path must not be found")
	}
}

func TestAgentEnvPutsBinDirOnPath(t *testing.T) {
	env := agentEnv([]string{"HOME=/home/x", "PATH=/usr/bin:/bin"}, "/opt/node/bin/codex")
	if got := envValue(env, "PATH"); got != "/opt/node/bin:/usr/bin:/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if got := envValue(env, "HOME"); got != "/home/x" {
		t.Fatalf("other variables must be kept, HOME = %q", got)
	}
	same := agentEnv([]string{"PATH=/usr/bin:/opt/node/bin"}, "/opt/node/bin/codex")
	if got := envValue(same, "PATH"); got != "/usr/bin:/opt/node/bin" {
		t.Fatalf("a directory already on PATH must not be added again: %q", got)
	}
	added := agentEnv([]string{"HOME=/home/x"}, "/opt/node/bin/codex")
	if got := envValue(added, "PATH"); got != "/opt/node/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if got := agentEnv([]string{"PATH=/usr/bin"}, "codex"); envValue(got, "PATH") != "/usr/bin" {
		t.Fatal("an unresolved name must leave PATH alone")
	}
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

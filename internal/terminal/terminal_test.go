package terminal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeCommand(t *testing.T) {
	if got := ResumeCommand("claude", "claude", "abc-123"); got != "claude --resume abc-123" {
		t.Fatal(got)
	}
	if got := ResumeCommand("codex", "/opt/codex", "thread_1"); got != "/opt/codex resume thread_1" {
		t.Fatal(got)
	}
	if got := ResumeCommand("cursor", "", "s1"); got != "cursor --resume s1" {
		t.Fatal(got)
	}
	if ResumeCommand("claude", "claude", "x; rm -rf /") != "" || ResumeCommand("claude", "claude", "") != "" {
		t.Fatal("unsafe session ids must be rejected")
	}
}

func TestDetectCustomAndArgs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "term.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECORD\"\n"), 0o755)
	em, problem := Detect(script + " --dir={dir} --run {cmd}")
	if em == nil || problem != "" || em.Name != "term.sh" {
		t.Fatalf("%+v %q", em, problem)
	}
	args := em.Args("/work", "claude --resume 1")
	if strings.Join(args, "|") != "--dir=/work|--run|claude --resume 1" {
		t.Fatalf("%v", args)
	}
	em, _ = Detect(script + " {dir}")
	if got := strings.Join(em.Args("/w", "c"), "|"); got != "/w|bash|-lc|c" {
		t.Fatalf("missing {cmd} must append bash -lc: %s", got)
	}
	if em, problem := Detect("/no/such/terminal {cmd}"); em != nil || !strings.Contains(problem, "not found") {
		t.Fatalf("%+v %q", em, problem)
	}
	if got := ShellCommand("claude --resume 1"); got != `claude --resume 1; exec "${SHELL:-bash}"` {
		t.Fatal(got)
	}
	if Quote("a'b") != `'a'\''b'` {
		t.Fatal(Quote("a'b"))
	}
}

func TestOpenRunsDetached(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "term.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\n"), 0o755)
	em, _ := Detect(script + " {dir} {cmd}")
	if err := em.Open(dir, "echo hi"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if data, err := os.ReadFile(record); err == nil && strings.Contains(string(data), "echo hi") {
			if !strings.Contains(string(data), dir) {
				t.Fatalf("dir not passed: %s", data)
			}
			return
		}
		<-timeAfter()
	}
	t.Fatal("terminal script was not started")
}

func timeAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() { defer close(ch); sleep20ms() }()
	return ch
}

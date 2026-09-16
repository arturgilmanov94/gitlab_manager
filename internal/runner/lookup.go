package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// LookAgent finds an agent CLI by name. It is exec.LookPath plus the usual install locations of the
// node/user-local package managers (nvm, fnm, volta, bun, npm --global).
//
// Those directories are put on PATH by an interactive shell rc file (~/.bashrc and friends), which a process
// started from the application menu, a .desktop launcher or a systemd unit never reads: an agent that works
// in the terminal would otherwise silently disappear from the dashboard. A name containing a separator is
// treated as a path and only checked for being executable.
func LookAgent(bin string) (string, bool) {
	if bin == "" {
		return "", false
	}
	if strings.ContainsRune(bin, filepath.Separator) {
		if abs, err := filepath.Abs(os.ExpandEnv(bin)); err == nil && executable(abs) {
			return abs, true
		}
		return "", false
	}
	if path, err := exec.LookPath(bin); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs, true
		}
		return path, true
	}
	for _, dir := range agentDirs() {
		if cand := filepath.Join(dir, bin); executable(cand) {
			return cand, true
		}
	}
	return "", false
}

// agentBin resolves the configured name to the executable to run, falling back to the plain name so the
// error the OS reports still mentions what was configured.
func agentBin(bin string) string {
	if path, ok := LookAgent(bin); ok {
		return path
	}
	return bin
}

// agentEnv prepends the agent binary's own directory to PATH so that the CLI finds its runtime (a
// `#!/usr/bin/env node` script needs the node it was installed with) and its sibling tools.
func agentEnv(env []string, binPath string) []string {
	dir := filepath.Dir(binPath)
	if dir == "" || dir == "." || !filepath.IsAbs(dir) {
		return env
	}
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == "PATH" {
			found = true
			if !onPath(value, dir) {
				kv = "PATH=" + dir + string(os.PathListSeparator) + value
			}
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

func onPath(path, dir string) bool {
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return true
		}
	}
	return false
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// agentDirs lists the directories agent CLIs are commonly installed into, most specific first.
func agentDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".node_modules", "bin"),
		filepath.Join(home, ".asdf", "shims"),
		filepath.Join(home, ".cargo", "bin"),
	}
	dirs = append(dirs, nodeVersionDirs(home)...)
	return append(dirs, "/usr/local/bin", "/opt/homebrew/bin", "/snap/bin")
}

// nodeVersionDirs returns the bin directories of the installed node versions: nvm's default alias first, then
// the remaining versions newest first (lexically descending, which is what nvm's zero-padded names give us).
func nodeVersionDirs(home string) []string {
	var dirs []string
	for _, m := range []struct{ root, suffix string }{
		{filepath.Join(home, ".nvm", "versions", "node"), "bin"},
		{filepath.Join(home, ".fnm", "node-versions"), filepath.Join("installation", "bin")},
		{filepath.Join(home, ".local", "share", "fnm", "node-versions"), filepath.Join("installation", "bin")},
	} {
		entries, err := os.ReadDir(m.root)
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() {
				versions = append(versions, e.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		if def := nvmDefault(home); def != "" {
			versions = append([]string{def}, versions...)
		}
		for _, v := range versions {
			dirs = append(dirs, filepath.Join(m.root, v, m.suffix))
		}
	}
	return dirs
}

// nvmDefault is the node version nvm's "default" alias points at ("v20.20.0"), when the alias exists.
func nvmDefault(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".nvm", "alias", "default"))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(raw))
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return ""
	}
	return name
}

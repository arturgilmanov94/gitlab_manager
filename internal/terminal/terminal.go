// Package terminal opens a terminal window on the machine the dashboard runs on: the developer continues an
// agent session interactively (`claude --resume <id>`) or gets a shell in a workspace. The dashboard is a
// local tool (browser and server on the same desktop), so this is a plain process start, not a remote shell.
package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Emulator is a detected terminal program and the way it accepts a working directory and a command.
type Emulator struct {
	Name string
	Path string
	// template holds the argument list with {dir} and {cmd} placeholders; {cmd} receives the shell command.
	template []string
}

// known lists the supported emulators in preference order with their argument templates. {cmd} is always
// passed as one argument to `bash -lc`, so the emulator only needs to run a program with arguments.
var known = []struct {
	name string
	args []string
}{
	{"gnome-terminal", []string{"--working-directory={dir}", "--", "bash", "-lc", "{cmd}"}},
	{"ptyxis", []string{"--working-directory={dir}", "--", "bash", "-lc", "{cmd}"}},
	{"konsole", []string{"--workdir", "{dir}", "-e", "bash", "-lc", "{cmd}"}},
	{"xfce4-terminal", []string{"--working-directory={dir}", "-x", "bash", "-lc", "{cmd}"}},
	{"kitty", []string{"--directory", "{dir}", "bash", "-lc", "{cmd}"}},
	{"alacritty", []string{"--working-directory", "{dir}", "-e", "bash", "-lc", "{cmd}"}},
	{"wezterm", []string{"start", "--cwd", "{dir}", "--", "bash", "-lc", "{cmd}"}},
	{"terminator", []string{"--working-directory={dir}", "-x", "bash", "-lc", "{cmd}"}},
	{"xterm", []string{"-e", "bash", "-lc", "{cmd}"}},
	{"x-terminal-emulator", []string{"-e", "bash", "-lc", "{cmd}"}},
}

// Detect finds a terminal emulator. custom is the TERMINAL_CMD template ("" = auto): a program followed by
// arguments where {dir} is the working directory and {cmd} the shell command; without {cmd} the command is
// appended as `bash -lc <cmd>`. Auto detection honours $TERMINAL first, then the known emulators in PATH.
// The second value explains why nothing was found.
func Detect(custom string) (*Emulator, string) {
	if strings.TrimSpace(custom) != "" {
		fields := strings.Fields(custom)
		path, err := exec.LookPath(fields[0])
		if err != nil {
			return nil, fmt.Sprintf("TERMINAL_CMD program %q not found in PATH", fields[0])
		}
		args := fields[1:]
		if !contains(args, "{cmd}") {
			args = append(args, "bash", "-lc", "{cmd}")
		}
		return &Emulator{Name: filepath.Base(fields[0]), Path: path, template: args}, ""
	}
	if env := strings.TrimSpace(os.Getenv("TERMINAL")); env != "" {
		for _, k := range known {
			if filepath.Base(env) == k.name {
				if path, err := exec.LookPath(env); err == nil {
					return &Emulator{Name: k.name, Path: path, template: k.args}, ""
				}
			}
		}
	}
	for _, k := range known {
		if path, err := exec.LookPath(k.name); err == nil {
			return &Emulator{Name: k.name, Path: path, template: k.args}, ""
		}
	}
	return nil, "no terminal emulator found in PATH (gnome-terminal, konsole, xfce4-terminal, kitty, alacritty, wezterm, xterm); set TERMINAL_CMD"
}

// GraphicalSession reports whether a display is reachable from this process.
func GraphicalSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// Args renders the emulator arguments for a directory and a shell command.
func (e *Emulator) Args(dir, command string) []string {
	out := make([]string, 0, len(e.template))
	for _, a := range e.template {
		out = append(out, strings.NewReplacer("{dir}", dir, "{cmd}", command).Replace(a))
	}
	return out
}

// Open starts the terminal detached from the dashboard process (its own session, no inherited stdio).
func (e *Emulator) Open(dir, command string) error {
	if e == nil {
		return errors.New("no terminal emulator available")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("directory %s does not exist", dir)
	}
	cmd := exec.Command(e.Path, e.Args(dir, command)...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start %s: %v", e.Name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ResumeCommand is the interactive command that continues an agent session: `<bin> --resume <id>` for Claude
// Code and Cursor, `<bin> resume <id>` for Codex. "" when the session id is unusable.
func ResumeCommand(runnerName, bin, sessionID string) string {
	if !sessionIDRe.MatchString(sessionID) {
		return ""
	}
	if bin == "" {
		bin = runnerName
	}
	switch runnerName {
	case "codex":
		return bin + " resume " + sessionID
	default:
		return bin + " --resume " + sessionID
	}
}

// ShellCommand wraps the command so the window stays open after the agent exits; "" opens a plain shell.
func ShellCommand(command string) string {
	if strings.TrimSpace(command) == "" {
		return `exec "${SHELL:-bash}"`
	}
	return command + `; exec "${SHELL:-bash}"`
}

// Quote returns a single-quoted shell word.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// Command mr-review is the single-binary MR review dashboard.
//
// Without arguments it starts the web server and opens the browser (double-click friendly).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mr-review/internal/app"
	"mr-review/internal/config"
	"mr-review/internal/db"
	"mr-review/internal/doctor"
	"mr-review/internal/gitlab"
	"mr-review/internal/runner"
	"mr-review/internal/skill"
	"mr-review/internal/web"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func usage() {
	fmt.Print(`Usage: mr-review [command]

  (no command)   start the dashboard in the foreground and open the browser
  serve          start the dashboard in the foreground (no browser)
  start          start in the background (pid in runtime/server.pid)
  stop           stop the background server
  status         is the server running?
  doctor         check the environment (exit code 1 when something FAILs)
  init-db        create/upgrade the SQLite database
  skill          map of dashboard actions to project skills, plus the instruction files found
  config         print the effective settings
  sync           pull your open MRs and issues from GitLab
  add <ref>      add a merge request (URL / !iid) or an issue (URL / #iid)
  review <ref>   run a review from the terminal (--kind quick|full|verify)
  list           list merge requests
  version        print the version

Settings come from .env next to the binary (see .env.example) or the environment.
`)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func baseDir() string {
	if override := os.Getenv("MR_REVIEW_HOME"); override != "" {
		return override
	}
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

func run(args []string) int {
	settings := config.Load(baseDir(), config.EnvironMap(), true)
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "", "open":
		return serve(settings, settings.OpenBrowser)
	case "serve":
		return serve(settings, false)
	case "start":
		return startBackground(settings)
	case "stop":
		return stop(settings)
	case "status":
		return status(settings)
	case "doctor":
		rep := doctor.Run(settings, version, gitlab.NewGlab(settings.GlabBin), runners(settings))
		fmt.Print(rep.Render())
		if rep.Ready() {
			return 0
		}
		return 1
	case "init-db":
		return initDB(settings)
	case "skill":
		return showSkill(settings)
	case "config":
		return showConfig(settings)
	case "sync":
		return withService(settings, func(s *app.Service) error {
			mrs, err := s.SyncMRs()
			if err != nil {
				return err
			}
			fmt.Printf("merge requests: %d synced for @%s (project %s); %d moved to history, %d removed\n", mrs.Synced, mrs.Username, mrs.Project, mrs.Archived, mrs.Pruned)
			issues, err := s.SyncIssues()
			if err != nil {
				return err
			}
			fmt.Printf("issues: %d synced\n", issues.Synced)
			return nil
		})
	case "add":
		if len(args) == 0 {
			return fail("usage: mr-review add <url>")
		}
		return withService(settings, func(s *app.Service) error {
			if strings.Contains(args[0], "/-/issues/") || strings.HasPrefix(args[0], "#") {
				issue, err := s.AddIssue(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("#%d  %s  %s\n", issue.ID, issue.Ref(), issue.Title)
				return nil
			}
			mr, err := s.AddMR(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("#%d  %s  %s  [%s]\n", mr.ID, mr.Ref(), mr.Title, short(mr.HeadSHA))
			return nil
		})
	case "review":
		return review(settings, args)
	case "list":
		return withService(settings, func(s *app.Service) error {
			items, err := s.DB.ListMRs()
			if err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Println("no merge requests yet — use `mr-review sync` or `mr-review add <url>`")
			}
			for _, item := range items {
				last := "-"
				if item.Last != nil {
					last = item.Last.Kind + "/" + item.Last.Status
				}
				stale := ""
				if item.Stale {
					stale = " (new commits)"
				}
				fmt.Printf("#%-4d %-32s %-26s%s  %s\n", item.ID, item.Ref(), last, stale, item.Title)
			}
			return nil
		})
	case "version", "--version", "-v":
		fmt.Println(version)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		return 1
	}
}

func runners(s *config.Settings) []runner.Runner {
	return []runner.Runner{&runner.Claude{Bin: s.ClaudeBin}, &runner.Codex{Bin: s.CodexBin}, &runner.Cursor{Bin: "cursor-agent"}}
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return 1
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func openDB(settings *config.Settings) (*db.DB, error) {
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		return nil, err
	}
	if _, err := database.Migrate(); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func withService(settings *config.Settings, fn func(*app.Service) error) int {
	database, err := openDB(settings)
	if err != nil {
		return fail("%v", err)
	}
	defer database.Close()
	svc := app.New(settings, database, gitlab.NewGlab(settings.GlabBin), runners(settings))
	defer svc.Shutdown()
	if err := fn(svc); err != nil {
		return fail("%v", err)
	}
	return 0
}

func initDB(settings *config.Settings) int {
	for _, dir := range []string{settings.LogDir, settings.RuntimeDir} {
		_ = os.MkdirAll(dir, 0o755)
	}
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		return fail("%v", err)
	}
	defer database.Close()
	applied, err := database.Migrate()
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("database: %s\n", settings.DatabasePath)
	if len(applied) == 0 {
		fmt.Println("applied migrations: none (up to date)")
	} else {
		fmt.Printf("applied migrations: %s\n", strings.Join(applied, ", "))
	}
	return 0
}

func showSkill(settings *config.Settings) int {
	if settings.ProjectRoot == "" {
		return fail("%s", settings.ProjectRootError)
	}
	resolver := skill.NewWithNames(settings.ProjectRoot, settings.SkillNames)
	fmt.Printf("project root: %s (via %s)\n\n", settings.ProjectRoot, settings.ProjectRootSource)
	fmt.Println("actions → project skills (every action first looks for its project skill; see docs/SKILLS.md):")
	for _, res := range resolver.Map() {
		found := "—  (CLAUDE.md / AGENTS.md + dashboard prompt)"
		switch {
		case res.Skill != nil && res.Via != "":
			found = fmt.Sprintf("via %s → %s  (%s)", res.Via, res.Skill.Identifier(), res.Skill.RelPath)
		case res.Skill != nil:
			v := resolver.Validate(*res.Skill)
			found = fmt.Sprintf("%s  (%s)", res.Skill.Identifier(), res.Skill.RelPath)
			if !v.OK {
				found += "  INVALID: " + strings.Join(v.Problems, "; ")
			}
		}
		fmt.Printf("  %-14s wants %-18s %s\n", res.Action.Kind, res.Wanted, found)
		fmt.Printf("  %-14s       %-18s %s\n", "", "", res.Action.Contract)
	}
	fmt.Println("\ncandidates:")
	for _, c := range resolver.Candidates() {
		fmt.Printf("  - %-30s %s\n", c.Identifier(), c.RelPath)
	}
	fmt.Println("instruction files Claude Code loads from the project:")
	for _, f := range resolver.InstructionFiles() {
		fmt.Printf("  - [%s] %s\n", f.Kind, f.RelPath)
	}
	return 0
}

func showConfig(settings *config.Settings) int {
	fmt.Printf("base_dir:        %s\n", settings.BaseDir)
	fmt.Printf("project_root:    %s (via %s)\n", settings.ProjectRoot, settings.ProjectRootSource)
	fmt.Printf("url:             %s\n", settings.URL())
	fmt.Printf("database:        %s\n", settings.DatabasePath)
	fmt.Printf("logs:            %s\n", settings.LogDir)
	fmt.Printf("runtime:         %s\n", settings.RuntimeDir)
	fmt.Printf("worktrees:       %s (base branch %s)\n", settings.WorktreeDir, settings.BaseBranch)
	for _, action := range skill.Actions {
		fmt.Printf("%-20s %s\n", strings.ToLower(action.EnvKey)+":", firstOf(settings.SkillNames[action.Kind], "(auto: "+action.SkillName+")"))
	}
	fmt.Printf("run_concurrency: %d\n", settings.RunConcurrency)
	fmt.Printf("permissions:     %s (approval timeout %ds)\n", settings.ClaudePermissions, settings.ApprovalTimeoutSec)
	fmt.Printf("plans_dir:       %s\n", firstOf(settings.PlansDir, "(auto: <project>/.claude/plans)"))
	fmt.Printf("default_runner:  %s\n", settings.DefaultRunner)
	fmt.Printf("gitlab_host:     %s\n", firstOf(settings.GitLabHost, "(from git remote)"))
	var labels []string
	for _, l := range settings.HighlightLabels {
		labels = append(labels, l.Name+":"+l.Color)
	}
	fmt.Printf("highlight_labels: %s\n", strings.Join(labels, ","))
	return 0
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------- server lifecycle

func serve(settings *config.Settings, openBrowser bool) int {
	for _, dir := range []string{settings.LogDir, settings.RuntimeDir} {
		_ = os.MkdirAll(dir, 0o755)
	}
	if pid, addr, ok := readPid(settings); ok && processAlive(pid) && healthy(addr) {
		fmt.Printf("Already running (pid %d) at http://%s\n", pid, addr)
		if openBrowser {
			launchBrowser("http://" + addr)
		}
		return 0
	}
	database, err := openDB(settings)
	if err != nil {
		return fail("%v", err)
	}
	defer database.Close()
	svc := app.New(settings, database, gitlab.NewGlab(settings.GlabBin), runners(settings))
	if n := svc.Recover(); n > 0 {
		fmt.Printf("marked %d interrupted run(s) as failed\n", n)
	}
	server, err := web.New(svc, version, runners(settings))
	if err != nil {
		return fail("%v", err)
	}
	listener, err := net.Listen("tcp", settings.Address())
	if err != nil {
		if healthy(settings.Address()) {
			return fail("%s is already served by another mr-review instance (an older version or a copy in another directory).\n"+
				"Stop it there (./mr-review stop) or pick another port: PORT=8766 ./mr-review", settings.URL())
		}
		return fail("cannot listen on %s: %v\nSet another port in .env (PORT=8766) or free the address.", settings.Address(), err)
	}
	addr := listener.Addr().String()
	_ = os.WriteFile(settings.PidFile(), []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), addr)), 0o644)
	defer func() {
		if pid, _, ok := readPid(settings); ok && pid == os.Getpid() {
			_ = os.Remove(settings.PidFile())
		}
	}()

	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		svc.Shutdown()
	}()
	fmt.Printf("mr-review %s listening on http://%s (project: %s)\n", version, addr, settings.ProjectRoot)
	if openBrowser {
		launchBrowser("http://" + addr)
	}
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail("%v", err)
	}
	return 0
}

func startBackground(settings *config.Settings) int {
	if pid, addr, ok := readPid(settings); ok && processAlive(pid) {
		fmt.Printf("Already running (pid %d) at http://%s\n", pid, addr)
		return 0
	}
	_ = os.MkdirAll(settings.LogDir, 0o755)
	_ = os.MkdirAll(settings.RuntimeDir, 0o755)
	exe, err := os.Executable()
	if err != nil {
		return fail("%v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(settings.LogDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fail("%v", err)
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), "MR_REVIEW_HOME="+settings.BaseDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail("cannot start: %v", err)
	}
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if pid, addr, ok := readPid(settings); ok && pid == cmd.Process.Pid && healthy(addr) {
			fmt.Printf("Started (pid %d). Open: http://%s\n", pid, addr)
			return 0
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
	}
	fmt.Fprintln(os.Stderr, "server did not become healthy; see logs/server.log")
	return 1
}

func stop(settings *config.Settings) int {
	pid, _, ok := readPid(settings)
	if !ok {
		fmt.Println("not running (no pid file)")
		return 0
	}
	if !processAlive(pid) {
		fmt.Printf("not running (stale pid %d)\n", pid)
		_ = os.Remove(settings.PidFile())
		return 0
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 40; i++ {
		if !processAlive(pid) {
			fmt.Printf("stopped (pid %d)\n", pid)
			_ = os.Remove(settings.PidFile())
			return 0
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(settings.PidFile())
	fmt.Printf("killed (pid %d)\n", pid)
	return 0
}

func status(settings *config.Settings) int {
	pid, addr, ok := readPid(settings)
	if !ok {
		fmt.Println("stopped (no runtime/server.pid)")
		return 3
	}
	if !processAlive(pid) {
		fmt.Printf("stopped (stale pid %d)\n", pid)
		return 3
	}
	if healthy(addr) {
		fmt.Printf("running  pid=%d  http://%s\n", pid, addr)
		return 0
	}
	fmt.Printf("starting/unhealthy  pid=%d  http://%s\n", pid, addr)
	return 1
}

func readPid(settings *config.Settings) (int, string, bool) {
	data, err := os.ReadFile(settings.PidFile())
	if err != nil {
		return 0, "", false
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, "", false
	}
	addr := settings.Address()
	if len(lines) > 1 {
		addr = strings.TrimSpace(lines[1])
	}
	return pid, addr, true
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func healthy(addr string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func launchBrowser(url string) {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("BROWSER") != "":
		cmd = exec.Command(os.Getenv("BROWSER"), url)
	case commandExists("xdg-open"):
		cmd = exec.Command("xdg-open", url)
	case commandExists("open"):
		cmd = exec.Command("open", url)
	default:
		fmt.Printf("Open %s in your browser\n", url)
		return
	}
	_ = cmd.Start()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ---------------------------------------------------------------------------------- cli review

// askInTerminal answers a permission prompt of a CLI-started run the way the terminal would: y / a (allow and
// remember) / n. Without a TTY the prompt is denied so the run does not hang.
func askInTerminal(s *app.Service, pending *db.Approval) {
	fmt.Printf("\nThe agent asks for a permission: %s %s\n", pending.ToolName, pending.Description)
	if pending.Reason != "" {
		fmt.Printf("  reason: %s\n", pending.Reason)
	}
	fmt.Printf("  input:  %s\n", short120(pending.InputJSON))
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("  no terminal to answer -> denied")
		_ = s.Decide(pending.ID, "deny", "no terminal to answer the prompt")
		return
	}
	fmt.Print("Allow? [y]es / [a]lways for this session / [n]o: ")
	var answer string
	_, _ = fmt.Scanln(&answer)
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		_ = s.Decide(pending.ID, "allow", "")
	case "a", "always":
		_ = s.Decide(pending.ID, "allow_always", "")
	default:
		_ = s.Decide(pending.ID, "deny", "denied in the terminal")
	}
}

func short120(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func review(settings *config.Settings, args []string) int {
	if len(args) == 0 {
		return fail("usage: mr-review review <mr-url|id> [--kind quick|full|verify] [--runner claude|codex|cursor]")
	}
	kind, runnerName := db.KindReviewFull, ""
	ref := args[0]
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--kind":
			if i+1 < len(args) {
				i++
				switch args[i] {
				case "quick":
					kind = db.KindReviewQuick
				case "verify":
					kind = db.KindReviewVerify
				default:
					kind = db.KindReviewFull
				}
			}
		case "--runner":
			if i+1 < len(args) {
				i++
				runnerName = args[i]
			}
		}
	}
	return withService(settings, func(s *app.Service) error {
		var mrID int64
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			if mr, _ := s.DB.GetMR(id); mr != nil {
				mrID = id
			}
		}
		if mrID == 0 {
			mr, err := s.AddMR(ref)
			if err != nil {
				return err
			}
			mrID = mr.ID
		}
		runID, err := s.StartReview(mrID, kind, runnerName, 0)
		if err != nil {
			return err
		}
		fmt.Printf("run #%d started (%s); log: %s\n", runID, kind, filepath.Join(settings.RunLogDir(), fmt.Sprintf("run-%d.log", runID)))
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		answered := map[int64]bool{}
		for {
			select {
			case <-sig:
				s.Cancel(runID)
			case <-time.After(2 * time.Second):
			}
			run, _ := s.DB.GetRun(runID)
			if run == nil || !run.Active() {
				break
			}
			if pending, _ := s.DB.PendingApproval(runID); pending != nil && !answered[pending.ID] {
				answered[pending.ID] = true
				askInTerminal(s, pending)
			}
			continue
		}
		run, _ := s.DB.GetRun(runID)
		fmt.Printf("status: %s  verdict: %s  tokens: %d (in %d / out %d / cache read %d / cache write %d)\n",
			run.Status, firstOf(run.Verdict, "-"), run.TotalTokens(), run.InputTokens, run.OutputTokens, run.CacheReadTokens, run.CacheWriteTokens)
		if run.Status != db.StatusDone {
			fmt.Println(run.Error)
			return errors.New("run did not complete")
		}
		fmt.Println(run.Summary)
		findings, _ := s.DB.ListFindings(runID)
		for _, f := range findings {
			loc := "-"
			if f.File != "" {
				loc = f.File
				if f.Line != nil {
					loc += ":" + strconv.FormatInt(*f.Line, 10)
				}
			}
			fmt.Printf("  [%s] %-8s %s  %s\n", f.Severity, f.Status, loc, f.Title)
		}
		return nil
	})
}

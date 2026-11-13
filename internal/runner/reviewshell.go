package runner

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ReviewShellCommand is the wider check for read-only runs whose shell is sandboxed (the project and the review
// worktree are not writable for it): any shell line that only uses local inspection tools passes, whatever its
// shell syntax — variables, $(...), process substitution, loops, awk/xargs, redirections into $TMPDIR. It refuses:
//   - git subcommands that change the repository, refs, config or the working tree (checkout, commit, push, ...);
//   - GitLab writes (glab other than `glab api` GET);
//   - programs outside the local tool list (curl, ssh, php, python, sh -c, eval, ...) and a command name that is
//     itself a variable or a substitution;
//   - output redirections anywhere but $TMPDIR, /tmp and /dev/null; here-docs; awk that runs commands or writes files.
//
// The checks do not rely on the sandbox alone: a line that passes cannot change the repository even unsandboxed.
func ReviewShellCommand(line string) bool {
	return reviewShell(line, 0)
}

func reviewShell(line string, depth int) bool {
	if depth > 4 {
		return false
	}
	segs, ok := scanShell(line, depth)
	if !ok || len(segs) == 0 {
		return false
	}
	for _, words := range segs {
		if !reviewSegment(words, depth) {
			return false
		}
	}
	return true
}

// shellKeywords may open a segment; the words after them are the command (`then git log`, `do cat "$f"`).
var shellKeywords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true,
	"while": true, "until": true, "!": true, "{": true, "}": true, "time": true, "esac": true}

// reviewTools are local programs that read files or transform text (dangerous options are refused below).
var reviewTools = map[string]bool{
	"cat": true, "head": true, "tail": true, "ls": true, "wc": true, "jq": true, "cut": true, "tr": true, "nl": true,
	"diff": true, "comm": true, "stat": true, "file": true, "basename": true, "dirname": true, "realpath": true, "readlink": true,
	"pwd": true, "echo": true, "printf": true, "true": true, "false": true, "test": true, "[": true, "[[": true, "]]": true, "cd": true,
	"column": true, "paste": true, "fold": true, "md5sum": true, "sha1sum": true, "sha256sum": true, "base64": true, "od": true,
	"hexdump": true, "du": true, "date": true, "seq": true, "grep": true, "egrep": true, "fgrep": true, "rev": true, "expand": true,
	"which": true, "type": true, "rg": true, "sort": true, "uniq": true, "find": true, "sed": true, "awk": true, "gawk": true,
	"mawk": true, "xargs": true, "git": true, "glab": true, "zcat": true, "strings": true, "iconv": true, "tac": true, "sleep": true,
}

func reviewSegment(words []string, depth int) bool {
	for len(words) > 0 && shellKeywords[words[0]] {
		words = words[1:]
	}
	if len(words) == 0 {
		return true
	}
	switch words[0] {
	case "for", "case", "in", ";;":
		return true // loop/case header: the words are data
	}
	// Leading assignments: harmless for the local tools below, except variables that change which program runs
	// or how (PATH, GIT_*, LD_*, ...).
	for len(words) > 0 && isAssignmentWord(words[0]) {
		eq := strings.IndexByte(words[0], '=')
		if dangerousVar(words[0][:eq]) || strings.HasPrefix(words[0][eq+1:], "-") {
			return false
		}
		words = words[1:]
	}
	if len(words) == 0 {
		return true
	}
	if strings.ContainsAny(words[0], "$`") || strings.Contains(words[0], substPlaceholder) {
		return false // the program name is computed: cannot know what runs
	}
	name := filepath.Base(words[0])
	args := words[1:]
	if !reviewTools[name] {
		return false
	}
	switch name {
	case "git":
		return reviewGit(args)
	case "glab":
		return readOnlyGlab(args)
	case "xargs":
		return reviewXargs(args, depth)
	case "awk", "gawk", "mawk":
		return safeAwk(args)
	case "sed":
		return safeSed(args)
	case "uniq":
		return len(positional(args)) <= 1
	}
	return safeArgs(name, args)
}

func isAssignmentWord(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j, r := range w[:i] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || j > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func dangerousVar(name string) bool {
	_, ok := safeAssignment(name + "=x")
	return !ok
}

// gitWrites are git subcommands that change the repository, its refs, config, index or working tree, or talk to
// a remote other than to fetch.
var gitWrites = map[string]bool{
	"push": true, "pull": true, "checkout": true, "switch": true, "reset": true, "commit": true, "merge": true, "rebase": true,
	"clean": true, "restore": true, "update-ref": true, "symbolic-ref": true, "gc": true, "prune": true, "repack": true,
	"am": true, "apply": true, "cherry-pick": true, "revert": true, "rm": true, "mv": true, "add": true, "init": true, "clone": true,
	"notes": true, "replace": true, "filter-branch": true, "submodule": true, "send-email": true, "request-pull": true,
	"credential": true, "daemon": true, "update-index": true, "read-tree": true, "checkout-index": true, "maintenance": true,
	"bisect": true, "sparse-checkout": true, "lfs": true, "fast-import": true, "pack-refs": true, "update-server-info": true,
	"upload-pack": true, "receive-pack": true, "archive": true, "bundle": true, "difftool": true, "mergetool": true, "web--browse": true,
	"instaweb": true, "svn": true, "p4": true, "remote-ext": true,
}

// reviewGit accepts git with any inspecting subcommand (log, show, diff, merge-base, merge-tree, ls-tree, fetch,
// cat-file, ...). Subcommands that both list and change (branch, tag, remote, config, stash, worktree, reflog) pass
// only in their listing forms.
func reviewGit(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		switch {
		case a == "-C" || a == "--git-dir" || a == "--work-tree":
			if len(args) < 2 {
				return false
			}
			args = args[2:]
		case a == "--no-pager" || a == "-P" || a == "--no-replace-objects" || a == "--literal-pathspecs" ||
			strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			args = args[1:]
		default:
			return false // -c key=value, --exec-path, --namespace ...: may run or redirect anything
		}
	}
	if len(args) == 0 {
		return false
	}
	sub, rest := args[0], args[1:]
	if gitWrites[sub] {
		return false
	}
	for _, a := range rest {
		if strings.HasPrefix(a, "--output") || a == "--ext-diff" || strings.HasPrefix(a, "--open-files-in-pager") || a == "-O" ||
			strings.HasPrefix(a, "--upload-pack") || strings.HasPrefix(a, "--receive-pack") || strings.HasPrefix(a, "--exec") {
			return false
		}
	}
	switch sub {
	case "fetch":
		for _, a := range rest {
			if a == "-u" || a == "--update-head-ok" || strings.HasPrefix(a, "--recurse-submodules") || strings.HasPrefix(a, "--refmap") {
				return false
			}
		}
	case "branch":
		for _, a := range rest {
			switch a {
			case "-d", "-D", "-m", "-M", "-c", "-C", "-f", "-u", "--delete", "--move", "--copy", "--force", "--unset-upstream", "--edit-description":
				return false
			}
			if strings.HasPrefix(a, "--set-upstream") || strings.HasPrefix(a, "-t") || strings.HasPrefix(a, "--track") {
				return false
			}
		}
		return hasAny(rest, "--list", "-l", "--contains", "--no-contains", "--merged", "--no-merged", "--show-current", "-a", "-r", "--all", "--remotes", "--points-at") ||
			len(positional(rest)) == 0
	case "tag":
		return hasAny(rest, "-l", "--list", "--contains", "--no-contains", "--points-at", "--merged", "--no-merged") || len(rest) == 0
	case "remote":
		return len(rest) == 0 || rest[0] == "-v" || rest[0] == "show" || rest[0] == "get-url"
	case "config":
		return hasAny(rest, "--get", "--get-all", "--get-regexp", "--list", "-l", "--get-urlmatch")
	case "stash", "worktree":
		return len(rest) > 0 && rest[0] == "list" || sub == "stash" && len(rest) > 0 && rest[0] == "show"
	case "reflog":
		return len(rest) == 0 || rest[0] != "expire" && rest[0] != "delete"
	}
	return true
}

func hasAny(args []string, opts ...string) bool {
	for _, a := range args {
		for _, o := range opts {
			if a == o || strings.HasPrefix(a, o+"=") {
				return true
			}
		}
	}
	return false
}

// reviewXargs checks the command xargs runs as a segment of its own.
func reviewXargs(args []string, depth int) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		switch a {
		case "-n", "-I", "-d", "-L", "-P", "-s", "-E", "--max-args", "--delimiter", "--max-procs", "--replace":
			if len(args) < 2 {
				return false
			}
			args = args[2:]
		default:
			if a == "-a" || strings.HasPrefix(a, "--arg-file") || a == "-p" || a == "--interactive" {
				return false
			}
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return true // echo
	}
	return reviewSegment(args, depth+1)
}

var (
	awkSystem = regexp.MustCompile(`\b(system|close)\s*\(|\|\s*getline|\|&`)
	awkPrint  = regexp.MustCompile(`\bprintf?\b[^;}\n]*`)
	awkOutput = regexp.MustCompile(`\||>[^=]|>$`)
)

// safeAwk accepts awk whose program neither runs commands (system, "cmd" | getline, print | "cmd") nor writes
// files (print > "file"); -f program files are refused.
func safeAwk(args []string) bool {
	prog := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-F" || a == "-v":
			i++
		case strings.HasPrefix(a, "-F") || strings.HasPrefix(a, "-v") || a == "--":
		case strings.HasPrefix(a, "-"):
			return false // -f progfile, -i include, -E ...
		default:
			prog = a
			i = len(args)
		}
	}
	if prog == "" || awkSystem.MatchString(prog) {
		return false
	}
	for _, stmt := range awkPrint.FindAllString(prog, -1) {
		if awkOutput.MatchString(stripAwkStrings(stmt)) {
			return false // print > "file", print | "cmd"
		}
	}
	return true
}

// substPlaceholder stands for the output of $(...) / `...` in a word after the inner command was checked.
const substPlaceholder = "\x00subst"

// scanShell splits a line into simple commands (quotes removed, variables kept as text). Inner commands of $(...),
// `...`, <(...) and >(...) are checked recursively. ok is false for here-docs, output redirections outside the
// temp directories, a lone & and unbalanced quotes or parentheses.
func scanShell(line string, depth int) (segs [][]string, ok bool) {
	rs := []rune(line)
	var words []string
	var word strings.Builder
	inWord := false
	redirect := false // the next word is an output redirection target
	input := false    // the next word is an input redirection source
	needCmd := false  // the last operator (|, &&, ||) still needs a command after it
	flushWord := func() bool {
		if !inWord {
			return true
		}
		w := word.String()
		word.Reset()
		inWord = false
		switch {
		case redirect:
			redirect = false
			return tempTarget(w)
		case input:
			input = false
			return true
		}
		words = append(words, w)
		needCmd = false
		return true
	}
	flushSeg := func() bool {
		if !flushWord() || redirect || input {
			return false
		}
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
		return true
	}
	// inner extracts the text of a balanced (...) starting after the opening paren at i; returns the index of ')'.
	inner := func(i int) (string, int, bool) {
		level := 1
		for j := i; j < len(rs); j++ {
			switch rs[j] {
			case '\'':
				k := j + 1
				for k < len(rs) && rs[k] != '\'' {
					k++
				}
				j = k
			case '"':
				k := j + 1
				for k < len(rs) && rs[k] != '"' {
					if rs[k] == '\\' {
						k++
					}
					k++
				}
				j = k
			case '\\':
				j++
			case '(':
				level++
			case ')':
				level--
				if level == 0 {
					return string(rs[i:j]), j, true
				}
			}
		}
		return "", 0, false
	}
	backtick := func(i int) (string, int, bool) {
		for j := i; j < len(rs); j++ {
			if rs[j] == '\\' {
				j++
				continue
			}
			if rs[j] == '`' {
				return string(rs[i:j]), j, true
			}
		}
		return "", 0, false
	}
	// dollar handles '$' at i inside or outside double quotes; returns the index of its last rune.
	dollar := func(i int) (int, bool) {
		if i+1 < len(rs) && rs[i+1] == '(' {
			if i+2 < len(rs) && rs[i+2] == '(' { // $(( arithmetic ))
				_, end, ok := inner(i + 3)
				if !ok || end+1 >= len(rs) || rs[end+1] != ')' {
					return 0, false
				}
				word.WriteString("0")
				return end + 1, true
			}
			text, end, ok := inner(i + 2)
			if !ok || !reviewShell(text, depth+1) {
				return 0, false
			}
			word.WriteString(substPlaceholder)
			return end, true
		}
		word.WriteRune('$')
		return i, true
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			word.WriteString(string(rs[i+1 : j]))
			inWord, i = true, j
		case c == '"':
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				switch {
				case rs[j] == '\\' && j+1 < len(rs):
					if !strings.ContainsRune("\"\\$`", rs[j+1]) {
						word.WriteRune('\\')
					}
					word.WriteRune(rs[j+1])
					j++
				case rs[j] == '$':
					end, ok := dollar(j)
					if !ok {
						return nil, false
					}
					j = end
				case rs[j] == '`':
					text, end, ok := backtick(j + 1)
					if !ok || !reviewShell(text, depth+1) {
						return nil, false
					}
					word.WriteString(substPlaceholder)
					j = end
				default:
					word.WriteRune(rs[j])
				}
			}
			if j >= len(rs) {
				return nil, false
			}
			inWord, i = true, j
		case c == '\\' && i+1 < len(rs):
			if rs[i+1] != '\n' {
				word.WriteRune(rs[i+1])
				inWord = true
			}
			i++
		case c == '$':
			end, ok := dollar(i)
			if !ok {
				return nil, false
			}
			inWord, i = true, end
		case c == '`':
			text, end, ok := backtick(i + 1)
			if !ok || !reviewShell(text, depth+1) {
				return nil, false
			}
			word.WriteString(substPlaceholder)
			inWord, i = true, end
		case (c == '<' || c == '>') && i+1 < len(rs) && rs[i+1] == '(' && !inWord:
			text, end, ok := inner(i + 2)
			if !ok || !reviewShell(text, depth+1) {
				return nil, false
			}
			word.WriteString(substPlaceholder)
			inWord, i = true, end
		case c == '<':
			if i+1 < len(rs) && rs[i+1] == '<' {
				return nil, false // here-doc / here-string
			}
			if inWord && isFD(word.String()) {
				word.Reset()
				inWord = false
			}
			if !flushWord() {
				return nil, false
			}
			input = true
		case c == '>':
			if inWord && (isFD(word.String()) || word.String() == "&") {
				word.Reset()
				inWord = false
			}
			if !flushWord() {
				return nil, false
			}
			if i+1 < len(rs) && rs[i+1] == '>' {
				i++
			}
			if i+1 < len(rs) && rs[i+1] == '&' { // >&2, 2>&1
				j := i + 2
				for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
					j++
				}
				if j == i+2 {
					return nil, false
				}
				i = j - 1
				continue
			}
			redirect = true
		case c == '&' && i+1 < len(rs) && rs[i+1] == '>': // &> file
			if !flushWord() {
				return nil, false
			}
			i++
			redirect = true
		case c == '|' || c == ';' || c == '\n' || c == '(' || c == ')':
			if c == '|' && i+1 < len(rs) && (rs[i+1] == '|' || rs[i+1] == '&') {
				i++
			}
			if c == ';' && i+1 < len(rs) && rs[i+1] == ';' {
				i++
			}
			if !flushSeg() {
				return nil, false
			}
			needCmd = needCmd || c == '|'
		case c == '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				if !flushSeg() {
					return nil, false
				}
				needCmd = true
			} else {
				return nil, false // background job
			}
		case c == '#' && !inWord:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i--
		case c == ' ' || c == '\t':
			if !flushWord() {
				return nil, false
			}
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	if !flushSeg() || needCmd {
		return nil, false
	}
	return segs, true
}

// tempTarget accepts output redirections into the temp directories and /dev/null only.
func tempTarget(w string) bool {
	if w == "/dev/null" || w == "/dev/stderr" || w == "/dev/stdout" {
		return true
	}
	if strings.Contains(w, "..") || strings.Contains(w, substPlaceholder) {
		return false
	}
	for _, p := range []string{"$TMPDIR/", "${TMPDIR}/", "/tmp/"} {
		if strings.HasPrefix(w, p) && len(w) > len(p) {
			return true
		}
	}
	return false
}

// stripAwkStrings drops "..." literals so that a ">" or "|" inside printed text is not taken for a redirection.
func stripAwkStrings(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && in:
			i++
		case s[i] == '"':
			in = !in
		case !in:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

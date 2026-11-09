package solari

// git namespace: safe, non-shell `git` invocations over the command RPC, plus
// the client-side parsers (status/branches/log) and arg construction
// (clone/commit/push auth). Ported to match the reference TypeScript git
// namespace byte-for-byte on the wire (args) and in its parsed outputs.

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// commandRunner runs `cmd` with the given options and returns its result. The
// Sandbox wires this to Commands.Run; tests inject a stub (exactly like the TS
// git suite stubs runCommand) so no guest is needed.
type commandRunner func(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error)

// Git is the version-control namespace on a Sandbox. Every method is a safe,
// non-shell `git` invocation over the command RPC with client-side parsing.
type Git struct {
	run commandRunner
}

func newGit(run commandRunner) *Git { return &Git{run: run} }

var (
	reNoCommits = regexp.MustCompile(`^No commits yet on (.+)$`)
	reBracket   = regexp.MustCompile(`\[(.*)\]`)
	reAhead     = regexp.MustCompile(`ahead (\d+)`)
	reBehind    = regexp.MustCompile(`behind (\d+)`)
)

// runGit runs `git <args>` in the guest via the command RPC (no shell).
func (g *Git) runGit(ctx context.Context, args []string, cwd string, env map[string]string) (*CommandResult, error) {
	opts := CommandOptions{Args: args}
	if cwd != "" {
		opts.Cwd = cwd
	}
	if env != nil {
		opts.Env = env
	}
	return g.run(ctx, "git", opts)
}

// mustGit runs `git <args>` and returns stdout, or an error carrying the
// subcommand, exit code, and trimmed stderr (fallback stdout) on a non-zero exit.
// A non-empty secret is masked in the message.
func (g *Git) mustGit(ctx context.Context, args []string, cwd string, env map[string]string, secret string) (string, error) {
	r, err := g.runGit(ctx, args, cwd, env)
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		detail := strings.TrimSpace(r.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(r.Stdout)
		}
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "***")
		}
		return "", fmt.Errorf("git %s failed (exit %d): %s", gitSubcommand(args), r.ExitCode, detail)
	}
	return r.Stdout, nil
}

// gitCredentialHelper is an inline git credential helper: it answers `get` from the command's environment, ignores
// `store` / `erase`, and holds no secret.
const gitCredentialHelper = `!f() { test "$1" = get && printf "username=%s\npassword=%s\n" "$SOLARI_GIT_USERNAME" "$SOLARI_GIT_PASSWORD"; }; f`

// gitAuth returns the `-c` options and environment for ONE git command that must authenticate to a private http(s)
// remote at remoteURL. The credentials travel as environment variables of that command and git reads them back through
// the inline helper, so they are never in the URL (and so not in .git/config, a snapshot or a template), never in argv
// (/proc/<pid>/cmdline) and never in a config file.
//
// The helper is bound to the remote's origin (`credential.<scheme>://<host>.helper`), so git never hands the password
// to another host: a redirect to a different host, or a downgrade to http, gets nothing. ok is false when there are no
// credentials or the remote is not http(s) (ssh remotes do not use them).
func gitAuth(remoteURL, username, password string) (cfg []string, env map[string]string, ok bool, err error) {
	if username == "" && password == "" {
		return nil, nil, false, nil
	}
	if strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return nil, nil, false, fmt.Errorf("git credentials must not contain line breaks or NUL")
	}
	u, perr := url.Parse(remoteURL)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, false, nil
	}
	origin := u.Scheme + "://" + u.Host
	if strings.ContainsAny(origin, "= \t\r\n") {
		return nil, nil, false, nil
	}
	return []string{
		// The empty value first clears any helper configured on the guest, so only ours answers...
		"-c", "credential.helper=",
		// ...and only for this origin.
		"-c", "credential." + origin + ".helper=" + gitCredentialHelper,
	}, map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"SOLARI_GIT_USERNAME": username,
		"SOLARI_GIT_PASSWORD": password,
	}, true, nil
}

// gitSubcommand is the git subcommand in an argv that may start with `-c k=v` pairs.
func gitSubcommand(args []string) string {
	i := 0
	for i+1 < len(args) && args[i] == "-c" {
		i += 2
	}
	if i < len(args) {
		return args[i]
	}
	return ""
}

// rejectOptionLike refuses user text that git would parse as an option.
func rejectOptionLike(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("git %s must not start with \"-\": %q", what, v)
	}
	return nil
}

// Clone clones url into opts.Path (or git's default dir) under opts.Cwd.
func (g *Git) Clone(ctx context.Context, rawURL string, opts GitCloneOptions) error {
	authCfg, env, _, err := gitAuth(rawURL, opts.Username, opts.Password)
	if err != nil {
		return err
	}
	args := append([]string{}, authCfg...)
	args = append(args, "clone")
	if opts.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(opts.Depth))
	}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}
	// `--` ends options: a url such as "--upload-pack=cmd" is then a (bad) repository, not an option.
	args = append(args, "--", rawURL)
	if opts.Path != "" {
		args = append(args, opts.Path)
	}
	_, err = g.mustGit(ctx, args, opts.Cwd, env, opts.Password)
	return err
}

// gitConflictCodes are the two-letter XY codes `git status --porcelain=v1` uses for an unmerged path.
var gitConflictCodes = map[string]bool{"DD": true, "AU": true, "UD": true, "UA": true, "DU": true, "AA": true, "UU": true}

// Status returns the parsed working-tree status. It runs `git status --porcelain=v1 -z --branch
// --untracked-files=all`, so paths come back verbatim (no quoting of spaces, quotes or non-ASCII), a rename is its new
// path with the old one in OrigPath, a conflicted path is listed once in Conflicted, and an untracked directory lists
// its files.
func (g *Git) Status(ctx context.Context, cwd string) (*GitStatus, error) {
	out, err := g.mustGit(ctx, []string{"status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all"}, cwd, nil, "")
	if err != nil {
		return nil, err
	}
	return parseGitStatusZ(out), nil
}

// parseGitStatusZ parses `git status --porcelain=v1 -z --branch`: a "## branch" record, then "XY path" records ("XY new"
// followed by a separate "old" record for a rename or copy).
func parseGitStatusZ(out string) *GitStatus {
	st := &GitStatus{
		Staged:     []string{},
		Modified:   []string{},
		Untracked:  []string{},
		Conflicted: []string{},
		Entries:    []GitStatusEntry{},
	}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if rec == "" {
			continue
		}
		if strings.HasPrefix(rec, "## ") {
			head := rec[3:]
			if strings.HasPrefix(head, "HEAD (no branch)") {
				st.Detached = true
				continue
			}
			// "main...origin/main [ahead 1, behind 2]" | "main...origin/main [gone]" | "main" | "No commits yet on main"
			body := head
			if m := reNoCommits.FindStringSubmatch(head); m != nil {
				body = m[1]
			}
			ab := reBracket.FindStringSubmatch(head)
			if ab != nil {
				body = strings.TrimSuffix(body, " ["+ab[1]+"]")
			}
			names := strings.SplitN(body, "...", 2)
			st.Branch = names[0]
			if len(names) == 2 && names[1] != "" {
				st.Upstream = names[1]
			}
			if ab != nil {
				if a := reAhead.FindStringSubmatch(ab[1]); a != nil {
					st.Ahead, _ = strconv.Atoi(a[1])
				}
				if b := reBehind.FindStringSubmatch(ab[1]); b != nil {
					st.Behind, _ = strconv.Atoi(b[1])
				}
			}
			continue
		}
		if len(rec) < 3 {
			continue
		}
		xy, path := rec[:2], rec[3:]
		entry := GitStatusEntry{Path: path, Index: xy[:1], Worktree: xy[1:2]}
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			if i+1 < len(recs) {
				i++
				entry.OrigPath = recs[i]
			}
		}
		st.Entries = append(st.Entries, entry)
		switch {
		case xy == "??":
			st.Untracked = append(st.Untracked, path)
		case gitConflictCodes[xy]:
			st.Conflicted = append(st.Conflicted, path)
		case xy != "!!":
			if xy[0] != ' ' {
				st.Staged = append(st.Staged, path)
			}
			if xy[1] != ' ' {
				st.Modified = append(st.Modified, path)
			}
		}
	}
	st.Clean = len(st.Entries) == 0
	return st
}

// Add stages paths (use ["."] for everything). A no-op on empty paths; the `--`
// separator guards paths that look like flags.
func (g *Git) Add(ctx context.Context, paths []string, cwd string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, paths...)
	_, err := g.mustGit(ctx, args, cwd, nil, "")
	return err
}

// Commit commits staged changes and returns the new commit hash. author/email
// scope identity to this one commit without mutating repo/global config.
func (g *Git) Commit(ctx context.Context, message string, opts GitCommitOptions) (string, error) {
	var args []string
	if opts.Author != "" {
		args = append(args, "-c", "user.name="+opts.Author)
	}
	if opts.Email != "" {
		args = append(args, "-c", "user.email="+opts.Email)
	}
	args = append(args, "commit", "-m", message)
	if opts.All {
		args = append(args, "-a")
	}
	if _, err := g.mustGit(ctx, args, opts.Cwd, nil, ""); err != nil {
		return "", err
	}
	out, err := g.mustGit(ctx, []string{"rev-parse", "HEAD"}, opts.Cwd, nil, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Push pushes to a remote (default origin + current branch).
func (g *Git) Push(ctx context.Context, opts GitRemoteOptions) error {
	return g.pushPull(ctx, "push", opts)
}

// Pull pulls from a remote (default origin + current branch).
func (g *Git) Pull(ctx context.Context, opts GitRemoteOptions) error {
	return g.pushPull(ctx, "pull", opts)
}

func (g *Git) pushPull(ctx context.Context, op string, opts GitRemoteOptions) error {
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}
	if err := rejectOptionLike("remote", remote); err != nil {
		return err
	}
	if err := rejectOptionLike("branch", opts.Branch); err != nil {
		return err
	}
	var authCfg []string
	var env map[string]string
	if opts.Username != "" || opts.Password != "" {
		// The helper is bound to the remote's origin, so resolve the remote's URL first.
		plain, err := g.mustGit(ctx, []string{"remote", "get-url", "--", remote}, opts.Cwd, nil, "")
		if err != nil {
			return err
		}
		authCfg, env, _, err = gitAuth(strings.TrimSpace(plain), opts.Username, opts.Password)
		if err != nil {
			return err
		}
	}
	args := append([]string{}, authCfg...)
	args = append(args, op, "--", remote)
	if opts.Branch != "" {
		args = append(args, opts.Branch)
	}
	_, err := g.mustGit(ctx, args, opts.Cwd, env, opts.Password)
	return err
}

// Checkout checks out an existing ref, or creates a branch with create=true.
func (g *Git) Checkout(ctx context.Context, ref string, cwd string, create bool) error {
	// A ref that starts with "-" would be read as an option ("--orphan=x" created a branch). Git never allows one as a
	// branch name, so refuse it rather than guess.
	if err := rejectOptionLike("ref", ref); err != nil {
		return err
	}
	args := []string{"checkout"}
	if create {
		args = append(args, "-b")
	}
	args = append(args, ref, "--")
	_, err := g.mustGit(ctx, args, cwd, nil, "")
	return err
}

// Branches lists local branches (name, short commit, upstream, whether it's HEAD). It uses `for-each-ref refs/heads`,
// so only real branches are listed: a detached HEAD is not a branch and none is current (Status().Detached says so).
func (g *Git) Branches(ctx context.Context, cwd string) ([]GitBranch, error) {
	out, err := g.mustGit(ctx, []string{"for-each-ref", "--format=%(HEAD)%1f%(refname:short)%1f%(objectname:short)%1f%(upstream:short)", "refs/heads"}, cwd, nil, "")
	if err != nil {
		return nil, err
	}
	branches := []GitBranch{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		branches = append(branches, GitBranch{
			Name:     field(fields, 1),
			Commit:   field(fields, 2),
			Upstream: field(fields, 3),
			Current:  field(fields, 0) == "*",
		})
	}
	return branches, nil
}

// Log returns recent commits, newest first. Records end with \x1e and fields with \x1f, so a multi-line body cannot be
// mistaken for the next commit. Message is the subject and Body the rest. A repository with no commit yet has an empty
// history, not an error.
func (g *Git) Log(ctx context.Context, opts GitLogOptions) ([]GitCommit, error) {
	args := []string{"log", "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e"}
	if opts.MaxCount > 0 {
		args = append(args, "--max-count="+strconv.Itoa(opts.MaxCount))
	}
	out, err := g.mustGit(ctx, args, opts.Cwd, nil, "")
	if err != nil {
		if strings.Contains(err.Error(), "does not have any commits yet") {
			return []GitCommit{}, nil
		}
		return nil, err
	}
	commits := []GitCommit{}
	for _, rec := range strings.Split(out, "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(rec, "\n"), "\x1f")
		commits = append(commits, GitCommit{
			Hash:    field(fields, 0),
			Author:  field(fields, 1),
			Email:   field(fields, 2),
			Date:    field(fields, 3),
			Message: field(fields, 4),
			Body:    strings.TrimRight(field(fields, 5), "\n"),
		})
	}
	return commits, nil
}

func field(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

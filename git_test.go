package solari

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// gitCall records one runCommand invocation.
type gitCall struct {
	cmd  string
	args []string
	cwd  string
}

// stubGit builds a Git whose runner records calls and maps `git <args>` to a
// CommandResult via responder (mirrors the TS git suite's stubGit). A nil
// responder result defaults to a clean exit.
func stubGit(responder func(args []string) *CommandResult) (*Git, *[]gitCall) {
	calls := &[]gitCall{}
	run := func(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
		*calls = append(*calls, gitCall{cmd: cmd, args: opts.Args, cwd: opts.Cwd})
		r := responder(opts.Args)
		if r == nil {
			r = &CommandResult{}
		}
		return r, nil
	}
	return newGit(run), calls
}

func TestGitStatusBranchAheadBehindStagedModifiedUntracked(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		if args[0] == "status" {
			return &CommandResult{Stdout: "## main...origin/main [ahead 2, behind 1]\n" +
				"M  staged.ts\n" +
				" M modified.ts\n" +
				"MM both.ts\n" +
				"?? new.txt\n"}
		}
		return nil
	})
	st, err := g.Status(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" {
		t.Errorf("branch: got %q", st.Branch)
	}
	if st.Ahead != 2 || st.Behind != 1 {
		t.Errorf("ahead/behind: %d/%d", st.Ahead, st.Behind)
	}
	if st.Detached {
		t.Error("detached should be false")
	}
	if !reflect.DeepEqual(st.Staged, []string{"staged.ts", "both.ts"}) {
		t.Errorf("staged: %v", st.Staged)
	}
	if !reflect.DeepEqual(st.Modified, []string{"modified.ts", "both.ts"}) {
		t.Errorf("modified: %v", st.Modified)
	}
	if !reflect.DeepEqual(st.Untracked, []string{"new.txt"}) {
		t.Errorf("untracked: %v", st.Untracked)
	}
	if st.Clean {
		t.Error("clean should be false")
	}
}

func TestGitStatusCleanDetachedHead(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		if args[0] == "status" {
			return &CommandResult{Stdout: "## HEAD (no branch)\n"}
		}
		return nil
	})
	st, err := g.Status(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Detached || !st.Clean || st.Branch != "" {
		t.Fatalf("detached/clean/branch wrong: %+v", st)
	}
}

func TestGitStatusFreshRepoNoCommits(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		if args[0] == "status" {
			return &CommandResult{Stdout: "## No commits yet on trunk\n?? a.txt\n"}
		}
		return nil
	})
	st, err := g.Status(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "trunk" {
		t.Errorf("branch: %q", st.Branch)
	}
	if !reflect.DeepEqual(st.Untracked, []string{"a.txt"}) {
		t.Errorf("untracked: %v", st.Untracked)
	}
}

func TestGitBranchesParsing(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if args[0] == "branch" {
			return &CommandResult{Stdout: "*\x1fmain\x1fabc1234\n \x1fdev\x1fdef5678\n"}
		}
		return nil
	})
	branches, err := g.Branches(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []GitBranch{
		{Name: "main", Commit: "abc1234", Current: true},
		{Name: "dev", Commit: "def5678", Current: false},
	}
	if !reflect.DeepEqual(branches, want) {
		t.Fatalf("branches: %+v", branches)
	}
	// --format uses record separators, never the porcelain `* name` layout.
	found := false
	for _, a := range (*calls)[0].args {
		if strings.Contains(a, "%(refname:short)") {
			found = true
		}
	}
	if !found {
		t.Error("expected --format arg with %(refname:short)")
	}
}

func TestGitLogParsing(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		if args[0] == "log" {
			return &CommandResult{Stdout: "h1\x1fAda\x1fada@x\x1f2026-01-01T00:00:00+00:00\x1ffirst\n" +
				"h2\x1fLin\x1flin@x\x1f2026-01-02T00:00:00+00:00\x1fsecond\n"}
		}
		return nil
	})
	log, err := g.Log(context.Background(), GitLogOptions{Cwd: "/repo", MaxCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("len: %d", len(log))
	}
	want := GitCommit{Hash: "h1", Author: "Ada", Email: "ada@x", Date: "2026-01-01T00:00:00+00:00", Message: "first"}
	if !reflect.DeepEqual(log[0], want) {
		t.Fatalf("log[0]: %+v", log[0])
	}
}

func TestGitCommitSetsIdentityThenResolvesHash(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		for _, a := range args {
			if a == "rev-parse" {
				return &CommandResult{Stdout: "deadbeef\n"}
			}
		}
		return nil
	})
	hash, err := g.Commit(context.Background(), "msg", GitCommitOptions{Cwd: "/repo", Author: "Ada", Email: "ada@x", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if hash != "deadbeef" {
		t.Errorf("hash: %q", hash)
	}
	var commit *gitCall
	for i := range *calls {
		for _, a := range (*calls)[i].args {
			if a == "commit" {
				commit = &(*calls)[i]
			}
		}
	}
	if commit == nil {
		t.Fatal("no commit call")
	}
	want := []string{"-c", "user.name=Ada", "-c", "user.email=ada@x", "commit", "-m", "msg", "-a"}
	if !reflect.DeepEqual(commit.args, want) {
		t.Fatalf("commit args: %v", commit.args)
	}
}

func TestGitCloneInjectsBasicAuth(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	err := g.Clone(context.Background(), "https://github.com/acme/repo.git", GitCloneOptions{
		Branch: "main", Depth: 1, Username: "u", Password: "p@ss word", Path: "dest", Cwd: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.cwd != "/work" {
		t.Errorf("cwd: %q", c.cwd)
	}
	if !reflect.DeepEqual(c.args[:5], []string{"clone", "--depth", "1", "--branch", "main"}) {
		t.Fatalf("args prefix: %v", c.args[:5])
	}
	url := c.args[5]
	if !strings.HasPrefix(url, "https://u:p%40ss%20word@github.com/") {
		t.Fatalf("auth url: %q", url)
	}
	if c.args[6] != "dest" {
		t.Errorf("path arg: %q", c.args[6])
	}
}

func TestGitAddEmptyNoopAndSeparator(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	ctx := context.Background()
	if err := g.Add(ctx, []string{}, ""); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("empty add should be a no-op, got %d calls", len(*calls))
	}
	if err := g.Add(ctx, []string{"a.ts", "-weird.ts"}, "/repo"); err != nil {
		t.Fatal(err)
	}
	want := []string{"add", "--", "a.ts", "-weird.ts"}
	if !reflect.DeepEqual((*calls)[0].args, want) {
		t.Fatalf("add args: %v", (*calls)[0].args)
	}
}

func TestGitNonZeroExitSurfacesError(t *testing.T) {
	run := func(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
		return &CommandResult{ExitCode: 128, Stderr: "fatal: not a git repository"}, nil
	}
	g := newGit(run)
	_, err := g.Status(context.Background(), "/nope")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("expected error mentioning 'not a git repository', got %v", err)
	}
	if !strings.Contains(err.Error(), "exit 128") {
		t.Errorf("error should include exit code: %v", err)
	}
}

func TestGitPushInjectsInsteadOfAuthThenPushes(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		// Only the URL-resolve leg is scripted; the push itself exits clean.
		if len(args) >= 2 && args[0] == "remote" && args[1] == "get-url" {
			return &CommandResult{Stdout: "https://github.com/acme/repo.git\n"}
		}
		return nil
	})
	err := g.Push(context.Background(), GitRemoteOptions{
		Cwd: "/repo", Username: "u", Password: "p@ss", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1) resolve the remote's plain URL.
	if !reflect.DeepEqual((*calls)[0].args, []string{"remote", "get-url", "origin"}) {
		t.Fatalf("url-resolve args: %v", (*calls)[0].args)
	}
	if (*calls)[0].cwd != "/repo" {
		t.Errorf("cwd: %q", (*calls)[0].cwd)
	}
	// 2) push under a one-off -c url.<authed>.insteadOf=<plain> (creds never persisted).
	push := (*calls)[1].args
	if push[0] != "-c" {
		t.Fatalf("expected -c first, got %v", push)
	}
	want := "url.https://u:p%40ss@github.com/acme/repo.git.insteadOf=https://github.com/acme/repo.git"
	if push[1] != want {
		t.Errorf("insteadOf:\n got %q\nwant %q", push[1], want)
	}
	if !reflect.DeepEqual(push[2:], []string{"push", "origin", "main"}) {
		t.Errorf("push tail: %v", push[2:])
	}
}

func TestGitPushWithoutCredsSkipsUrlRewrite(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	if err := g.Push(context.Background(), GitRemoteOptions{Cwd: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("no `remote get-url` leg expected without creds, got %d calls", len(*calls))
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"push", "origin"}) {
		t.Errorf("push args: %v", (*calls)[0].args)
	}
}

func TestGitPullSharesAuthPathWithCustomRemote(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if len(args) >= 1 && args[0] == "remote" {
			return &CommandResult{Stdout: "https://github.com/acme/repo.git\n"}
		}
		return nil
	})
	err := g.Pull(context.Background(), GitRemoteOptions{Remote: "upstream", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"remote", "get-url", "upstream"}) {
		t.Fatalf("url-resolve args: %v", (*calls)[0].args)
	}
	pull := (*calls)[1].args
	if pull[0] != "-c" {
		t.Fatalf("expected -c first, got %v", pull)
	}
	if !reflect.DeepEqual(pull[2:], []string{"pull", "upstream"}) {
		t.Errorf("pull tail: %v", pull[2:])
	}
}

func TestGitCheckoutSwitchesRefAndCreateAddsDashB(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	if err := g.Checkout(context.Background(), "dev", "/repo", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"checkout", "dev"}) {
		t.Errorf("checkout args: %v", (*calls)[0].args)
	}
	if err := g.Checkout(context.Background(), "feature", "/repo", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[1].args, []string{"checkout", "-b", "feature"}) {
		t.Errorf("checkout -b args: %v", (*calls)[1].args)
	}
}

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
	env  map[string]string
}

// stubGit builds a Git whose runner records calls and maps `git <args>` to a
// CommandResult via responder (mirrors the TS git suite's stubGit). A nil
// responder result defaults to a clean exit.
func stubGit(responder func(args []string) *CommandResult) (*Git, *[]gitCall) {
	calls := &[]gitCall{}
	run := func(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
		*calls = append(*calls, gitCall{cmd: cmd, args: opts.Args, cwd: opts.Cwd, env: opts.Env})
		r := responder(opts.Args)
		if r == nil {
			r = &CommandResult{}
		}
		return r, nil
	}
	return newGit(run), calls
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

func TestGitCloneSendsCredentialsByEnvAndInlineHelperNeverInURLOrArgv(t *testing.T) {
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
	if !reflect.DeepEqual(c.args[:3], []string{"-c", "credential.helper=", "-c"}) || !strings.HasPrefix(c.args[3], "credential.https://github.com.helper=!f()") {
		t.Fatalf("helper options: %v", c.args[:4])
	}
	want := []string{"clone", "--depth", "1", "--branch", "main", "--", "https://github.com/acme/repo.git", "dest"}
	if !reflect.DeepEqual(c.args[4:], want) {
		t.Fatalf("args: %v", c.args[4:])
	}
	if c.env["SOLARI_GIT_USERNAME"] != "u" || c.env["SOLARI_GIT_PASSWORD"] != "p@ss word" || c.env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("env: %v", c.env)
	}
	for _, a := range c.args {
		if strings.Contains(a, "p@ss") || strings.Contains(a, "p%40ss") || strings.Contains(a, "u:") {
			t.Errorf("credential in argv: %q", a)
		}
	}
}

func TestGitClonePutsDashDashBeforeUserURLAndSendsNoHelperWithoutCreds(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	if err := g.Clone(context.Background(), "--upload-pack=touch /tmp/pwned", GitCloneOptions{Path: "dest"}); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if !reflect.DeepEqual(c.args, []string{"clone", "--", "--upload-pack=touch /tmp/pwned", "dest"}) || c.env != nil {
		t.Errorf("args %v env %v", c.args, c.env)
	}
}

func TestGitCredentialsWithLineBreaksAreRefusedAndPasswordIsMaskedInErrors(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	err := g.Clone(context.Background(), "https://h/r.git", GitCloneOptions{Username: "u", Password: "a\nurl=https://evil"})
	if err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Errorf("want a line-break refusal, got %v", err)
	}
	g2, _ := stubGit(func(args []string) *CommandResult {
		return &CommandResult{ExitCode: 128, Stderr: "fatal: unable to access: hunter2 rejected"}
	})
	err = g2.Clone(context.Background(), "https://h/r.git", GitCloneOptions{Username: "u", Password: "hunter2"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "git clone failed") {
		t.Errorf("want a masked clone error, got %v", err)
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

func TestGitPushResolvesRemoteURLBindsHelperToItsOriginAndKeepsSecretOutOfArgv(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if len(args) > 0 && args[0] == "remote" {
			return &CommandResult{Stdout: "https://git.example.com:8443/acme/repo.git\n"}
		}
		return nil
	})
	err := g.Push(context.Background(), GitRemoteOptions{Cwd: "/repo", Username: "u", Password: "p@ss", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"remote", "get-url", "--", "origin"}) || (*calls)[0].env != nil {
		t.Fatalf("url-resolve leg: %+v", (*calls)[0])
	}
	push := (*calls)[1].args
	if !reflect.DeepEqual(push[:2], []string{"-c", "credential.helper="}) || !strings.HasPrefix(push[3], "credential.https://git.example.com:8443.helper=!f()") || !reflect.DeepEqual(push[4:], []string{"push", "--", "origin", "main"}) {
		t.Errorf("push args: %v", push)
	}
	if (*calls)[1].env["SOLARI_GIT_PASSWORD"] != "p@ss" || strings.Contains(strings.Join(push, " "), "p@ss") {
		t.Errorf("password handling wrong: env %v args %v", (*calls)[1].env, push)
	}
}

func TestGitCredentialsAreNotUsedForAnSSHRemote(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if len(args) > 0 && args[0] == "remote" {
			return &CommandResult{Stdout: "git@github.com:acme/repo.git\n"}
		}
		return nil
	})
	if err := g.Push(context.Background(), GitRemoteOptions{Cwd: "/repo", Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[1].args, []string{"push", "--", "origin"}) || (*calls)[1].env != nil {
		t.Errorf("push: %+v", (*calls)[1])
	}
}

func TestGitPushWithoutCredsIsAPlainPush(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	if err := g.Push(context.Background(), GitRemoteOptions{Cwd: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].args, []string{"push", "--", "origin"}) || (*calls)[0].env != nil {
		t.Errorf("calls: %+v", *calls)
	}
}

func TestGitPullSharesAuthPathWithCustomRemote(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if len(args) > 0 && args[0] == "remote" {
			return &CommandResult{Stdout: "https://github.com/acme/repo.git\n"}
		}
		return nil
	})
	if err := g.Pull(context.Background(), GitRemoteOptions{Remote: "upstream", Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"remote", "get-url", "--", "upstream"}) {
		t.Fatalf("url-resolve args: %v", (*calls)[0].args)
	}
	pull := (*calls)[1].args
	if !reflect.DeepEqual(pull[4:], []string{"pull", "--", "upstream"}) || (*calls)[1].env["SOLARI_GIT_USERNAME"] != "u" {
		t.Errorf("pull: %v env %v", pull, (*calls)[1].env)
	}
}

func TestGitRefusesOptionLikeRemotesBranchesAndRefs(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	ctx := context.Background()
	if err := g.Push(ctx, GitRemoteOptions{Remote: "--receive-pack=x"}); err == nil {
		t.Error("push remote")
	}
	if err := g.Pull(ctx, GitRemoteOptions{Branch: "--rebase"}); err == nil {
		t.Error("pull branch")
	}
	if err := g.Checkout(ctx, "--orphan=x", "", false); err == nil {
		t.Error("checkout ref")
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should reach the guest, got %d calls", len(*calls))
	}
}

func TestGitCheckoutSwitchesRefAndCreateAddsDashB(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return &CommandResult{} })
	if err := g.Checkout(context.Background(), "dev", "/repo", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[0].args, []string{"checkout", "dev", "--"}) {
		t.Errorf("checkout args: %v", (*calls)[0].args)
	}
	if err := g.Checkout(context.Background(), "feature", "/repo", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*calls)[1].args, []string{"checkout", "-b", "feature", "--"}) {
		t.Errorf("checkout -b args: %v", (*calls)[1].args)
	}
}

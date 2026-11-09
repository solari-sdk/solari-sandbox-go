package solari

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The retest found git status/branches/log parsing lossy in every SDK: quoted paths, "old -> new" renames, an
// untracked directory as "dir/", a conflict listed as staged and modified, a detached HEAD as a pseudo branch, a
// subject-only log that threw on an empty repository. These tests pin the verbatim parsing; the same rows run in the
// vendored conformance suite (tests/conformance, spec/git.json).

func statusStub(stdout string) (*Git, *[]gitCall) {
	return stubGit(func(args []string) *CommandResult {
		if args[0] == "status" {
			return &CommandResult{Stdout: stdout}
		}
		return nil
	})
}

func TestGitStatusRunsPorcelainZWithAllUntrackedFiles(t *testing.T) {
	g, calls := statusStub("## main\x00")
	if _, err := g.Status(context.Background(), "/repo"); err != nil {
		t.Fatal(err)
	}
	want := []string{"status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all"}
	if got := (*calls)[0].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args: %v", got)
	}
}

func TestGitStatusVerbatimPathsRenameConflictUntracked(t *testing.T) {
	g, _ := statusStub("## main...origin/main [ahead 1, behind 2]\x00 M a b.txt\x00R  new name\x00old name\x00UU conflict\x00A  é ü.txt\x00?? nd/inner\x00?? nd/deep file\x00")
	st, err := g.Status(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" || st.Upstream != "origin/main" || st.Ahead != 1 || st.Behind != 2 || st.Detached {
		t.Errorf("branch fields: %+v", st)
	}
	if !reflect.DeepEqual(st.Staged, []string{"new name", "é ü.txt"}) {
		t.Errorf("staged: %q", st.Staged)
	}
	if !reflect.DeepEqual(st.Modified, []string{"a b.txt"}) {
		t.Errorf("modified: %q", st.Modified)
	}
	if !reflect.DeepEqual(st.Conflicted, []string{"conflict"}) {
		t.Errorf("conflicted: %q", st.Conflicted)
	}
	if !reflect.DeepEqual(st.Untracked, []string{"nd/inner", "nd/deep file"}) {
		t.Errorf("untracked: %q", st.Untracked)
	}
	wantEntries := []GitStatusEntry{
		{Path: "a b.txt", Index: " ", Worktree: "M"},
		{Path: "new name", OrigPath: "old name", Index: "R", Worktree: " "},
		{Path: "conflict", Index: "U", Worktree: "U"},
		{Path: "é ü.txt", Index: "A", Worktree: " "},
		{Path: "nd/inner", Index: "?", Worktree: "?"},
		{Path: "nd/deep file", Index: "?", Worktree: "?"},
	}
	if !reflect.DeepEqual(st.Entries, wantEntries) {
		t.Errorf("entries: %+v", st.Entries)
	}
	if st.Clean {
		t.Error("clean should be false")
	}
}

func TestGitStatusBranchForms(t *testing.T) {
	cases := []struct {
		name, out string
		check     func(*GitStatus) bool
	}{
		{"detached", "## HEAD (no branch)\x00", func(s *GitStatus) bool { return s.Detached && s.Branch == "" && s.Clean && len(s.Entries) == 0 }},
		{"fresh", "## No commits yet on trunk\x00?? a.txt\x00", func(s *GitStatus) bool {
			return s.Branch == "trunk" && reflect.DeepEqual(s.Untracked, []string{"a.txt"}) && !s.Clean
		}},
		{"gone", "## main...origin/main [gone]\x00", func(s *GitStatus) bool {
			return s.Branch == "main" && s.Upstream == "origin/main" && s.Ahead == 0 && s.Behind == 0 && s.Clean
		}},
		{"ahead only", "## dev...up/dev [ahead 3]\x00", func(s *GitStatus) bool { return s.Ahead == 3 && s.Behind == 0 && s.Upstream == "up/dev" }},
	}
	for _, c := range cases {
		g, _ := statusStub(c.out)
		st, err := g.Status(context.Background(), "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.check(st) {
			t.Errorf("%s: %+v", c.name, st)
		}
	}
}

func TestGitStatusIgnoredAndCopyRecords(t *testing.T) {
	g, _ := statusStub("## main\x00!! build/out\x00C  copy\x00orig\x00 D gone\x00")
	st, err := g.Status(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.Staged, []string{"copy"}) || !reflect.DeepEqual(st.Modified, []string{"gone"}) {
		t.Errorf("staged/modified: %q %q", st.Staged, st.Modified)
	}
	if len(st.Entries) != 3 || st.Entries[1].OrigPath != "orig" || st.Entries[0].Index != "!" {
		t.Errorf("entries: %+v", st.Entries)
	}
}

func TestGitStatusEveryConflictCodeIsConflictedOnly(t *testing.T) {
	for _, xy := range []string{"DD", "AU", "UD", "UA", "DU", "AA", "UU"} {
		g, _ := statusStub("## main\x00" + xy + " f\x00")
		st, err := g.Status(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(st.Conflicted, []string{"f"}) || len(st.Staged) != 0 || len(st.Modified) != 0 {
			t.Errorf("%s: %+v", xy, st)
		}
	}
}

func TestGitBranchesUseForEachRefRealBranchesOnly(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if args[0] == "for-each-ref" {
			return &CommandResult{Stdout: " \x1ffeat/x\x1fabc1234\x1f\n \x1fmain\x1fdef5678\x1forigin/main\n"}
		}
		return nil
	})
	got, err := g.Branches(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []GitBranch{
		{Name: "feat/x", Commit: "abc1234"},
		{Name: "main", Commit: "def5678", Upstream: "origin/main"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("branches: %+v", got)
	}
	wantArgs := []string{"for-each-ref", "--format=%(HEAD)%1f%(refname:short)%1f%(objectname:short)%1f%(upstream:short)", "refs/heads"}
	if a := (*calls)[0].args; !reflect.DeepEqual(a, wantArgs) {
		t.Fatalf("args: %v", a)
	}
}

func TestGitBranchesMarksCurrent(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult { return &CommandResult{Stdout: "*\x1fmain\x1fdef5678\x1f\n"} })
	got, err := g.Branches(context.Background(), "/repo")
	if err != nil || len(got) != 1 || !got[0].Current || got[0].Upstream != "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGitLogKeepsSubjectAndBody(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult {
		if args[0] == "log" {
			return &CommandResult{Stdout: "h1\x1fAda\x1fada@x\x1f2026-01-01T00:00:00+00:00\x1ffirst\x1fbody 1\nline 2\n\x1e\n" +
				"h2\x1fLin\x1flin@x\x1f2026-01-02T00:00:00+00:00\x1fsecond\x1f\n\x1e\n"}
		}
		return nil
	})
	got, err := g.Log(context.Background(), GitLogOptions{Cwd: "/repo", MaxCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []GitCommit{
		{Hash: "h1", Author: "Ada", Email: "ada@x", Date: "2026-01-01T00:00:00+00:00", Message: "first", Body: "body 1\nline 2"},
		{Hash: "h2", Author: "Lin", Email: "lin@x", Date: "2026-01-02T00:00:00+00:00", Message: "second", Body: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log: %+v", got)
	}
	wantArgs := []string{"log", "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e", "--max-count=2"}
	if a := (*calls)[0].args; !reflect.DeepEqual(a, wantArgs) {
		t.Fatalf("args: %v", a)
	}
}

func TestGitLogEmptyRepositoryIsEmptyNotAnError(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		return &CommandResult{ExitCode: 128, Stderr: "fatal: your current branch 'main' does not have any commits yet\n"}
	})
	got, err := g.Log(context.Background(), GitLogOptions{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func TestGitLogOtherFailuresStillError(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		return &CommandResult{ExitCode: 128, Stderr: "fatal: not a git repository\n"}
	})
	if _, err := g.Log(context.Background(), GitLogOptions{}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("err: %v", err)
	}
}

// Real git: the parsing has to survive what git actually prints for hostile names.
func TestGitParsingAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	home := t.TempDir()
	var argvs [][]string
	g := newGit(realRunner(home, &argvs))
	run := func(args ...string) {
		t.Helper()
		sh(t, append([]string{"git", "-C", dir, "-c", "user.name=T", "-c", "user.email=t@x", "-c", "init.defaultBranch=main"}, args...)...)
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	run("init", "-q")
	if got, err := g.Log(ctx, GitLogOptions{Cwd: dir}); err != nil || len(got) != 0 {
		t.Fatalf("empty repo log: %v %v", got, err)
	}
	write("old name.txt", "one\ntwo\nthree\nfour\n")
	write("quo\"te.txt", "q")
	run("add", "-A")
	run("commit", "-q", "-m", "subject line\n\nbody line 1\nbody line 2")
	run("mv", "old name.txt", "new name é.txt")
	write("dir with space/deep file.txt", "x")
	write("tab\there.txt", "t")

	st, err := g.Status(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" || st.Detached {
		t.Errorf("branch: %+v", st)
	}
	if !reflect.DeepEqual(st.Staged, []string{"new name é.txt"}) {
		t.Errorf("staged: %q", st.Staged)
	}
	if !reflect.DeepEqual(st.Untracked, []string{"dir with space/deep file.txt", "tab\there.txt"}) {
		t.Errorf("untracked: %q", st.Untracked)
	}
	var rename *GitStatusEntry
	for i := range st.Entries {
		if st.Entries[i].OrigPath != "" {
			rename = &st.Entries[i]
		}
	}
	if rename == nil || rename.Path != "new name é.txt" || rename.OrigPath != "old name.txt" {
		t.Errorf("rename entry: %+v", rename)
	}

	log, err := g.Log(ctx, GitLogOptions{Cwd: dir, MaxCount: 5})
	if err != nil || len(log) != 1 || log[0].Message != "subject line" || log[0].Body != "body line 1\nbody line 2" {
		t.Fatalf("log: %+v %v", log, err)
	}

	run("commit", "-q", "-am", "second")
	run("checkout", "-q", "--detach")
	st, err = g.Status(ctx, dir)
	if err != nil || !st.Detached || st.Branch != "" {
		t.Errorf("detached: %+v %v", st, err)
	}
	bs, err := g.Branches(ctx, dir)
	if err != nil || len(bs) != 1 || bs[0].Name != "main" || bs[0].Current {
		t.Errorf("branches while detached: %+v %v", bs, err)
	}
}

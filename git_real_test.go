package solari

// Real git against a real smart-HTTP server (git http-backend behind basic auth): the credentials reach git through
// the environment and the inline credential helper, and nothing about them is left in .git/config or argv.

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	realUser = "alice"
	realPass = "p@ss w0rd/ü"
)

func gitBackend(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, p := range []string{"/usr/lib/git-core/git-http-backend", "/usr/libexec/git-core/git-http-backend"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("git-http-backend not installed")
	return ""
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// realRunner executes the real git with the env the SDK asked for, on a clean HOME.
func realRunner(home string, argvs *[][]string) commandRunner {
	return func(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
		*argvs = append(*argvs, opts.Args)
		c := exec.CommandContext(ctx, cmd, opts.Args...)
		c.Dir = opts.Cwd
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}
		for k, v := range opts.Env {
			c.Env = append(c.Env, k+"="+v)
		}
		var so, se bytes.Buffer
		c.Stdout, c.Stderr = &so, &se
		err := c.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			return nil, err
		}
		return &CommandResult{ExitCode: code, Stdout: so.String(), Stderr: se.String()}, nil
	}
}

func TestGitPrivateCloneAndPushCredentialsViaEnvAndHelperOnly(t *testing.T) {
	backend := gitBackend(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "srv")
	bare := filepath.Join(root, "repo.git")
	sh(t, "git", "init", "-q", "--bare", "-b", "main", bare)
	sh(t, "git", "-C", bare, "config", "http.receivepack", "true")
	seed := filepath.Join(dir, "seed")
	sh(t, "git", "init", "-q", "-b", "main", seed)
	sh(t, "git", "-C", seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, "git", "-C", seed, "push", "-q", bare, "main")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(realUser+":"+realPass))
	var seen []string
	cgiH := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + realUser}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cgiH.ServeHTTP(w, r)
	}))
	defer srv.Close()

	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	var argvs [][]string
	g := newGit(realRunner(home, &argvs))
	ctx := context.Background()
	plain := srv.URL + "/repo.git"

	if err := g.Clone(ctx, plain, GitCloneOptions{Path: filepath.Join(dir, "nocreds")}); err == nil {
		t.Fatal("clone without credentials should fail")
	}
	err := g.Clone(ctx, plain, GitCloneOptions{Path: filepath.Join(dir, "bad"), Username: realUser, Password: "wrong-" + realPass})
	if err == nil || strings.Contains(err.Error(), "wrong-"+realPass) {
		t.Fatalf("wrong password: want an error that does not echo it, got %v", err)
	}

	dest := filepath.Join(dir, "work")
	if err := g.Clone(ctx, plain, GitCloneOptions{Path: dest, Username: realUser, Password: realPass}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range seen {
		found = found || a == want
	}
	if !found {
		t.Error("the server never received the credentials")
	}
	cfgBytes, _ := os.ReadFile(filepath.Join(dest, ".git", "config"))
	cfg := string(cfgBytes)
	if !strings.Contains(cfg, "url = "+plain) {
		t.Errorf("config should keep the plain URL:\n%s", cfg)
	}
	for _, needle := range []string{realPass, "p%40ss", realUser + ":", "credential"} {
		if strings.Contains(cfg, needle) {
			t.Errorf(".git/config contains %q:\n%s", needle, cfg)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".git-credentials")); err == nil {
		t.Error("a credential store was written")
	}

	sh(t, "git", "-C", dest, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "second")
	if err := g.Push(ctx, GitRemoteOptions{Cwd: dest, Username: realUser, Password: realPass, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sh(t, "git", "-C", bare, "log", "--format=%s", "main"), "second") {
		t.Error("push did not reach the server")
	}
	cfgBytes, _ = os.ReadFile(filepath.Join(dest, ".git", "config"))
	if strings.Contains(string(cfgBytes), realPass) {
		t.Error("push left the password in .git/config")
	}
	for _, a := range argvs {
		j := strings.Join(a, " ")
		if strings.Contains(j, realPass) || strings.Contains(j, "p%40ss") {
			t.Errorf("credential in argv: %q", j)
		}
	}
}

func TestGitHostileUploadPackURLIsJustABadRepository(t *testing.T) {
	gitBackend(t)
	dir := t.TempDir()
	var argvs [][]string
	g := newGit(realRunner(dir, &argvs))
	mark := filepath.Join(dir, "pwned")
	if err := g.Clone(context.Background(), "--upload-pack=touch "+mark, GitCloneOptions{Path: filepath.Join(dir, "x")}); err == nil {
		t.Fatal("expected a clone failure")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the --upload-pack option was executed")
	}
}

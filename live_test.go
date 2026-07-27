// LIVE integration suite — Go, against staging.
//
//	cd sdk/go && SOLARI_LIVE=1 SOLARI_API_KEY=… go test -run TestLive -v
//	SOLARI_BASE_URL=…   # override target (default: staging)
//
// Gated on SOLARI_LIVE so it never runs by accident, but it always COMPILES
// (no build tag) so `go vet ./...` and `go build` keep it honest.
//
// Scope: the Go SDK is CONTROL-PLANE ONLY. There is no screenshot/mouse/
// keyboard surface to test — a desktop here is a VM whose `kind` is "desktop"
// and which carries a StreamURL. This suite covers what exists: create/connect,
// kind, StreamURL, commands, files, code.run, git, and lifecycle.
//
// Creates exactly ONE VM in setup and kills it in a deferred teardown that runs
// even on panic/failure — a leaked staging VM is real money and real pool
// pressure. Subtests are sequential (no t.Parallel) so they share the handle.
//
// Each SDK call is logged as a transcript:   -> call        <- result
package solari

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const liveDefaultBaseURL = "https://api-sta.getsolari.com"

// liveTarget returns the (apiKey, baseURL) for the live run, or skips.
func liveTarget(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("SOLARI_LIVE") == "" {
		t.Skip("set SOLARI_LIVE=1 (and SOLARI_API_KEY) to run live tests")
	}
	key := os.Getenv("SOLARI_API_KEY")
	if key == "" {
		t.Fatal("SOLARI_LIVE=1 but SOLARI_API_KEY is empty")
	}
	base := os.Getenv("SOLARI_BASE_URL")
	if base == "" {
		base = liveDefaultBaseURL
	}
	return key, base
}

// fmtLive renders a call result compactly for the transcript.
func fmtLive(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "ok"
	case []byte:
		n := len(x)
		head := x
		if n > 8 {
			head = x[:8]
		}
		parts := make([]string, 0, len(head))
		for _, b := range head {
			parts = append(parts, fmt.Sprint(b))
		}
		tail := ""
		if n > 8 {
			tail = ",…"
		}
		return fmt.Sprintf("<%d bytes> [%s%s]", n, strings.Join(parts, ","), tail)
	case string:
		if len(x) > 120 {
			x = x[:120] + "…"
		}
		return fmt.Sprintf("%q", x)
	}
	s := fmt.Sprintf("%+v", v)
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

// step runs one SDK call, printing `-> call` then `<- result`.
func step[T any](t *testing.T, call string, fn func() (T, error)) T {
	t.Helper()
	t.Logf("-> %s", call)
	r, err := fn()
	if err != nil {
		t.Fatalf("<- ERROR %s: %v", call, err)
	}
	t.Logf("<- %s", fmtLive(r))
	return r
}

// stepVoid is step for calls that only return an error.
func stepVoid(t *testing.T, call string, fn func() error) {
	t.Helper()
	t.Logf("-> %s", call)
	if err := fn(); err != nil {
		t.Fatalf("<- ERROR %s: %v", call, err)
	}
	t.Logf("<- ok")
}

// transientCreate reports whether a create failure is a transient capacity /
// concurrency condition worth retrying.
//
// Matched on the TYPED error, not on message text: the gateway's 503 body is
// "No sandbox host available", which contains neither "concurrent" nor
// "capacity" — a substring matcher (as the TS suite uses) silently fails to
// retry the single most common staging failure, warm-pool exhaustion.
//
//	429 ConcurrencyLimitError — the org's slots are held (by other agents/tests)
//	503 NoCapacityError       — no host free yet; the pool is refilling
func transientCreate(err error) bool {
	var noCap *NoCapacityError
	var conc *ConcurrencyLimitError
	return errors.As(err, &noCap) || errors.As(err, &conc)
}

// createWithRetry creates ONE VM, retrying 25× / 6s on transient capacity or
// concurrency errors (mirrors the TS suite's retry budget). Any other error is a
// real bug and fails immediately.
func createWithRetry(ctx context.Context, t *testing.T, c *Client, desktop bool) *Sandbox {
	t.Helper()
	var last error
	for i := 0; i < 25; i++ {
		// No Template for a desktop: the gateway defaults kind:"desktop" to the
		// desktop golden — exercising that default is the point.
		opts := CreateOptions{Metadata: map[string]string{"suite": "live-go"}}
		if desktop {
			opts.Kind = KindDesktop
			t.Logf("-> client.Create({Kind: KindDesktop})")
		} else {
			opts.Template = "base"
			t.Logf(`-> client.Create({Template: "base"})`)
		}
		sb, err := c.Create(ctx, opts)
		if err == nil {
			t.Logf("<- id=%s kind=%s streamUrl=%q", sb.ID, sb.Kind, sb.StreamURL)
			return sb
		}
		last = err
		if !transientCreate(err) {
			t.Fatalf("create failed (not a capacity/concurrency error): %v", err)
		}
		t.Logf("<- transient (%v); retry %d/25 in 6s", err, i+1)
		time.Sleep(6 * time.Second)
	}
	t.Fatalf("create: exhausted retries: %v", last)
	return nil
}

// TestLiveDesktop drives the whole Go control-plane surface against ONE live
// desktop VM.
func TestLiveDesktop(t *testing.T) {
	key, base := liveTarget(t)
	ctx := context.Background()

	c, err := NewClient(ClientOptions{APIKey: key, BaseURL: base})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sb := createWithRetry(ctx, t, c, true)

	// Teardown FIRST (defer runs LIFO and on panic): never leak a staging VM.
	defer func() {
		t.Logf("-> client.Kill(%s)  [teardown]", sb.ID)
		if err := c.Kill(context.Background(), sb.ID); err != nil {
			t.Errorf("<- TEARDOWN FAILED — VM %s may be leaked: %v", sb.ID, err)
			return
		}
		t.Logf("<- ok (VM destroyed)")
	}()

	// --- create contract ----------------------------------------------------
	t.Run("create returns a desktop with a stream URL", func(t *testing.T) {
		if sb.ID == "" {
			t.Fatal("empty sandbox id")
		}
		if sb.Kind != KindDesktop {
			t.Fatalf("kind = %q, want %q", sb.Kind, KindDesktop)
		}
		// streamUrl is returned by CREATE for desktop kind only (router.ts:1356).
		if sb.StreamURL == "" {
			t.Fatal("desktop create returned no StreamURL")
		}
		if !strings.Contains(sb.StreamURL, "/stream/") {
			t.Fatalf("StreamURL = %q, want a /stream/ URL", sb.StreamURL)
		}
		if !strings.HasPrefix(sb.StreamURL, "ws://") && !strings.HasPrefix(sb.StreamURL, "wss://") {
			t.Fatalf("StreamURL = %q, want a ws(s) scheme", sb.StreamURL)
		}
		if sb.ControlURL == "" {
			t.Fatal("create returned no ControlURL")
		}
	})

	stepVoid(t, "sb.Connect()", func() error { return sb.Connect(ctx) })
	if !sb.Connected() {
		t.Fatal("Connected() = false after Connect()")
	}

	// --- commands -----------------------------------------------------------
	t.Run("commands.Run returns real guest output", func(t *testing.T) {
		r := step(t, `sb.Commands.Run("uname", {Args: ["-s"]})`, func() (*CommandResult, error) {
			return sb.Commands.Run(ctx, "uname", CommandOptions{Args: []string{"-s"}})
		})
		if r.ExitCode != 0 {
			t.Fatalf("exitCode = %d, want 0", r.ExitCode)
		}
		// Real evidence: this is a Linux guest, not just "didn't throw".
		if strings.TrimSpace(r.Stdout) != "Linux" {
			t.Fatalf("stdout = %q, want %q", strings.TrimSpace(r.Stdout), "Linux")
		}
	})
	t.Run("commands.Run surfaces a nonzero exit code", func(t *testing.T) {
		r := step(t, `sb.Commands.Run("sh", {Args: ["-c", "exit 7"]})`, func() (*CommandResult, error) {
			return sb.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c", "exit 7"}})
		})
		if r.ExitCode != 7 {
			t.Fatalf("exitCode = %d, want 7", r.ExitCode)
		}
	})
	t.Run("commands.Run captures stderr separately from stdout", func(t *testing.T) {
		r := step(t, `sb.Commands.Run("sh", {Args: ["-c", "echo out; echo oops 1>&2"]})`, func() (*CommandResult, error) {
			return sb.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c", "echo out; echo oops 1>&2"}})
		})
		if !strings.Contains(r.Stderr, "oops") {
			t.Fatalf("stderr = %q, want it to contain %q", r.Stderr, "oops")
		}
		// The streams must not be merged.
		if strings.Contains(r.Stdout, "oops") {
			t.Fatalf("stdout = %q leaked stderr", r.Stdout)
		}
		if !strings.Contains(r.Stdout, "out") {
			t.Fatalf("stdout = %q, want it to contain %q", r.Stdout, "out")
		}
	})
	t.Run("commands.Run captures stdout reliably (20x, no dropped frames)", func(t *testing.T) {
		t.Logf(`-> sb.Commands.Run("sh", ["-c","echo tokN"])  x20`)
		for i := 0; i < 20; i++ {
			r, err := sb.Commands.Run(ctx, "sh", CommandOptions{
				Args: []string{"-c", fmt.Sprintf("echo tok%d", i)},
			})
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
			if got, want := strings.TrimSpace(r.Stdout), fmt.Sprintf("tok%d", i); got != want {
				t.Fatalf("run %d: stdout = %q, want %q", i, got, want)
			}
		}
		t.Logf("<- 20/20 stdout captured, no dropped frames")
	})
	t.Run("commands.Start streams output and reports its exit code", func(t *testing.T) {
		h := step(t, `sb.Commands.Start("sh", {Args: ["-c", "echo streamed; exit 3"]})`, func() (*CommandHandle, error) {
			return sb.Commands.Start(ctx, "sh", CommandOptions{Args: []string{"-c", "echo streamed; exit 3"}})
		})
		var got strings.Builder
		h.OnData(func(stream, data string) {
			if stream == "stdout" {
				got.WriteString(data)
			}
		})
		code := step(t, "handle.Wait()", func() (int, error) { return h.Wait(ctx) })
		if code != 3 {
			t.Fatalf("exit = %d, want 3", code)
		}
		if !strings.Contains(got.String(), "streamed") {
			t.Fatalf("streamed stdout = %q, want it to contain %q", got.String(), "streamed")
		}
	})

	// --- files --------------------------------------------------------------
	t.Run("files Write/ReadText/List/Stat", func(t *testing.T) {
		stepVoid(t, `sb.Files.Write("/tmp/go-live.txt", "hello")`, func() error {
			return sb.Files.Write(ctx, "/tmp/go-live.txt", []byte("hello"), 0)
		})
		txt := step(t, `sb.Files.ReadText("/tmp/go-live.txt")`, func() (string, error) {
			return sb.Files.ReadText(ctx, "/tmp/go-live.txt")
		})
		if txt != "hello" {
			t.Fatalf("readText = %q, want %q", txt, "hello")
		}
		entries := step(t, `sb.Files.List("/tmp")`, func() ([]FsEntry, error) {
			return sb.Files.List(ctx, "/tmp")
		})
		var found *FsEntry
		for i := range entries {
			if entries[i].Name == "go-live.txt" {
				found = &entries[i]
			}
		}
		if found == nil {
			t.Fatal("go-live.txt not in /tmp listing")
		}
		// Wire-shape evidence: "dir" and a real size, not just presence.
		if found.Dir {
			t.Fatal("go-live.txt reported as a directory")
		}
		if found.Size != 5 {
			t.Fatalf("list size = %d, want 5", found.Size)
		}
		st := step(t, `sb.Files.Stat("/tmp/go-live.txt")`, func() (*FsStat, error) {
			return sb.Files.Stat(ctx, "/tmp/go-live.txt")
		})
		if st.Size != 5 {
			t.Fatalf("stat size = %d, want 5", st.Size)
		}
		if st.Dir {
			t.Fatal("stat says dir")
		}
		// modTimeMs is unix-millis and must be a plausible recent timestamp —
		// guards the "modTimeMs vs modTime" wire-field bug.
		if st.ModTimeMs < 1_600_000_000_000 {
			t.Fatalf("stat modTimeMs = %d, want unix-millis", st.ModTimeMs)
		}
	})
	t.Run("files Mkdir/Rename/Remove", func(t *testing.T) {
		stepVoid(t, `sb.Files.Mkdir("/tmp/go-sub")`, func() error { return sb.Files.Mkdir(ctx, "/tmp/go-sub") })
		stepVoid(t, `sb.Files.Rename("/tmp/go-live.txt", "/tmp/go-sub/b.txt")`, func() error {
			return sb.Files.Rename(ctx, "/tmp/go-live.txt", "/tmp/go-sub/b.txt")
		})
		txt := step(t, `sb.Files.ReadText("/tmp/go-sub/b.txt")`, func() (string, error) {
			return sb.Files.ReadText(ctx, "/tmp/go-sub/b.txt")
		})
		if txt != "hello" {
			t.Fatalf("content after rename = %q, want %q", txt, "hello")
		}
		stepVoid(t, `sb.Files.Remove("/tmp/go-sub", true)`, func() error {
			return sb.Files.Remove(ctx, "/tmp/go-sub", true)
		})
		entries := step(t, `sb.Files.List("/tmp")`, func() ([]FsEntry, error) { return sb.Files.List(ctx, "/tmp") })
		for _, e := range entries {
			if e.Name == "go-sub" {
				t.Fatal("go-sub still present after recursive Remove")
			}
		}
	})
	t.Run("files binary round-trip preserves every byte", func(t *testing.T) {
		want := []byte{0, 1, 2, 250, 255}
		stepVoid(t, `sb.Files.Write("/tmp/go-bin", <5 bytes>)`, func() error {
			return sb.Files.Write(ctx, "/tmp/go-bin", want, 0)
		})
		got := step(t, `sb.Files.Read("/tmp/go-bin")`, func() ([]byte, error) {
			return sb.Files.Read(ctx, "/tmp/go-bin")
		})
		if len(got) != len(want) {
			t.Fatalf("read %d bytes, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("byte %d = %d, want %d", i, got[i], want[i])
			}
		}
	})

	// --- code.run -----------------------------------------------------------
	t.Run("code.Run evaluates python and returns the result text", func(t *testing.T) {
		r := step(t, `sb.Code.Run("print(2**10)")`, func() (*RunCodeResult, error) {
			return sb.Code.Run(ctx, "print(2**10)", RunCodeOptions{})
		})
		var text string
		for _, item := range r.Results {
			if item.Text != "" {
				text += item.Text
			}
		}
		if !strings.Contains(text, "1024") {
			t.Fatalf("code.run text = %q, want it to contain %q", text, "1024")
		}
	})
	t.Run("code.CreateContext is stateful across runs", func(t *testing.T) {
		ctxID := step(t, `sb.Code.CreateContext("python")`, func() (string, error) {
			return sb.Code.CreateContext(ctx, "python")
		})
		step(t, `sb.Code.Run("x = 41", {ContextID})`, func() (*RunCodeResult, error) {
			return sb.Code.Run(ctx, "x = 41", RunCodeOptions{ContextID: ctxID})
		})
		r := step(t, `sb.Code.Run("print(x + 1)", {ContextID})`, func() (*RunCodeResult, error) {
			return sb.Code.Run(ctx, "print(x + 1)", RunCodeOptions{ContextID: ctxID})
		})
		var text string
		for _, item := range r.Results {
			text += item.Text
		}
		// 42 only appears if `x` survived from the previous run in this context.
		if !strings.Contains(text, "42") {
			t.Fatalf("stateful context text = %q, want it to contain %q", text, "42")
		}
	})

	// --- git ----------------------------------------------------------------
	t.Run("git init/status/add/commit/log/branches", func(t *testing.T) {
		const repo = "/tmp/go-repo"
		// Build a real repo in the guest, then drive it through the git namespace.
		step(t, `sb.Commands.Run("sh", ["-c", "rm -rf /tmp/go-repo && mkdir -p …&& git init"])`, func() (*CommandResult, error) {
			return sb.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c",
				"rm -rf " + repo + " && mkdir -p " + repo + " && cd " + repo +
					" && git init -q && git config user.email live@solari.test && git config user.name live"}})
		})

		stepVoid(t, `sb.Files.Write("/tmp/go-repo/f.txt", "v1")`, func() error {
			return sb.Files.Write(ctx, repo+"/f.txt", []byte("v1"), 0)
		})
		st := step(t, `sb.Git.Status("/tmp/go-repo")`, func() (*GitStatus, error) {
			return sb.Git.Status(ctx, repo)
		})
		// A fresh repo with one new file: untracked, not clean.
		if st.Clean {
			t.Fatal("status.Clean = true, want false (f.txt is untracked)")
		}
		var sawUntracked bool
		for _, u := range st.Untracked {
			if u == "f.txt" {
				sawUntracked = true
			}
		}
		if !sawUntracked {
			t.Fatalf("status.Untracked = %v, want it to contain f.txt", st.Untracked)
		}

		stepVoid(t, `sb.Git.Add(["f.txt"], "/tmp/go-repo")`, func() error {
			return sb.Git.Add(ctx, []string{"f.txt"}, repo)
		})
		hash := step(t, `sb.Git.Commit("live commit")`, func() (string, error) {
			return sb.Git.Commit(ctx, "live commit", GitCommitOptions{
				Cwd: repo, Author: "live", Email: "live@solari.test",
			})
		})
		// Real evidence: a git hash is hex and >= 7 chars.
		if len(hash) < 7 {
			t.Fatalf("commit hash = %q, want a real hash", hash)
		}
		for _, ch := range hash {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("commit hash = %q, want hex", hash)
			}
		}

		st2 := step(t, `sb.Git.Status("/tmp/go-repo")  (post-commit)`, func() (*GitStatus, error) {
			return sb.Git.Status(ctx, repo)
		})
		if !st2.Clean {
			t.Fatalf("status.Clean = false after commit, want true (staged=%v modified=%v untracked=%v)",
				st2.Staged, st2.Modified, st2.Untracked)
		}

		log := step(t, `sb.Git.Log({Cwd, MaxCount: 10})`, func() ([]GitCommit, error) {
			return sb.Git.Log(ctx, GitLogOptions{Cwd: repo, MaxCount: 10})
		})
		if len(log) != 1 {
			t.Fatalf("log has %d commits, want 1", len(log))
		}
		if log[0].Message != "live commit" {
			t.Fatalf("log[0].Message = %q, want %q", log[0].Message, "live commit")
		}
		if !strings.HasPrefix(hash, log[0].Hash) && !strings.HasPrefix(log[0].Hash, hash) {
			t.Fatalf("log[0].Hash = %q, want it to match the commit hash %q", log[0].Hash, hash)
		}

		branches := step(t, `sb.Git.Branches("/tmp/go-repo")`, func() ([]GitBranch, error) {
			return sb.Git.Branches(ctx, repo)
		})
		var current *GitBranch
		for i := range branches {
			if branches[i].Current {
				current = &branches[i]
			}
		}
		if current == nil {
			t.Fatalf("no current branch in %v", branches)
		}
		if current.Name != st2.Branch {
			t.Fatalf("current branch %q != status branch %q", current.Name, st2.Branch)
		}

		// checkout -b creates and switches.
		stepVoid(t, `sb.Git.Checkout("feature", cwd, true)`, func() error {
			return sb.Git.Checkout(ctx, "feature", repo, true)
		})
		st3 := step(t, `sb.Git.Status(cwd)  (post-checkout)`, func() (*GitStatus, error) {
			return sb.Git.Status(ctx, repo)
		})
		if st3.Branch != "feature" {
			t.Fatalf("branch after checkout = %q, want %q", st3.Branch, "feature")
		}
	})

	// --- query / re-attach --------------------------------------------------
	t.Run("Get returns our desktop", func(t *testing.T) {
		view := step(t, "client.Get(sb.ID)", func() (*SandboxView, error) { return c.Get(ctx, sb.ID) })
		if view.SandboxID != sb.ID {
			t.Fatalf("sandboxId = %q, want %q", view.SandboxID, sb.ID)
		}
		if view.Kind != KindDesktop {
			t.Fatalf("kind = %q, want desktop", view.Kind)
		}
		if view.State != "running" {
			t.Fatalf("state = %q, want running", view.State)
		}
		// By design (toSandboxView, router.ts:2501) the view carries NO
		// controlUrl/streamUrl — Connect derives them. Assert the design.
		if view.ControlURL != "" {
			t.Fatalf("SandboxView.ControlURL = %q, want empty (view serializer omits it)", view.ControlURL)
		}
	})
	t.Run("Connect re-attaches and derives the stream URL", func(t *testing.T) {
		re := step(t, "client.Connect(sb.ID)", func() (*Sandbox, error) { return c.Connect(ctx, sb.ID) })
		defer re.Close()
		if re.ID != sb.ID {
			t.Fatalf("id = %q, want %q", re.ID, sb.ID)
		}
		if re.Kind != KindDesktop {
			t.Fatalf("kind = %q, want desktop", re.Kind)
		}
		// GET carries no streamUrl, so Connect DERIVES it (desktops only).
		if re.StreamURL == "" {
			t.Fatal("Connect derived no StreamURL for a desktop")
		}
		if !strings.HasSuffix(re.StreamURL, "/stream/"+re.ID) {
			t.Fatalf("derived StreamURL = %q, want it to end with /stream/%s", re.StreamURL, re.ID)
		}
		if !strings.HasPrefix(re.StreamURL, "ws://") && !strings.HasPrefix(re.StreamURL, "wss://") {
			t.Fatalf("derived StreamURL = %q, want a ws(s) scheme", re.StreamURL)
		}
		// The re-attached handle is live, not just a struct.
		stepVoid(t, "reattached.Connect()", func() error { return re.Connect(ctx) })
		r := step(t, `reattached.Commands.Run("sh", ["-c","echo reattached"])`, func() (*CommandResult, error) {
			return re.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c", "echo reattached"}})
		})
		if !strings.Contains(r.Stdout, "reattached") {
			t.Fatalf("stdout = %q, want it to contain %q", r.Stdout, "reattached")
		}
	})
}

// TestLiveSandboxHasNoStreamURL proves the other half of the streamUrl design:
// a headless sandbox has no display, so the gateway omits streamUrl and Connect
// derives none. Uses its own (cheap, headless) VM and kills it in a defer.
func TestLiveSandboxHasNoStreamURL(t *testing.T) {
	key, base := liveTarget(t)
	ctx := context.Background()

	c, err := NewClient(ClientOptions{APIKey: key, BaseURL: base})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sb := createWithRetry(ctx, t, c, false)
	defer func() {
		t.Logf("-> client.Kill(%s)  [teardown]", sb.ID)
		if err := c.Kill(context.Background(), sb.ID); err != nil {
			t.Errorf("<- TEARDOWN FAILED — VM %s may be leaked: %v", sb.ID, err)
			return
		}
		t.Logf("<- ok (VM destroyed)")
	}()

	if sb.Kind != KindSandbox {
		t.Fatalf("kind = %q, want %q", sb.Kind, KindSandbox)
	}
	// The whole point: no display => no stream URL, on create...
	if sb.StreamURL != "" {
		t.Fatalf("headless sandbox create returned StreamURL %q, want none", sb.StreamURL)
	}
	// ...and none derived on connect either.
	re := step(t, "client.Connect(sb.ID)", func() (*Sandbox, error) { return c.Connect(ctx, sb.ID) })
	defer re.Close()
	if re.StreamURL != "" {
		t.Fatalf("Connect derived StreamURL %q for a headless sandbox, want none", re.StreamURL)
	}
	// It is a real, working sandbox all the same.
	stepVoid(t, "sb.Connect()", func() error { return sb.Connect(ctx) })
	r := step(t, `sb.Commands.Run("uname", ["-s"])`, func() (*CommandResult, error) {
		return sb.Commands.Run(ctx, "uname", CommandOptions{Args: []string{"-s"}})
	})
	if strings.TrimSpace(r.Stdout) != "Linux" {
		t.Fatalf("stdout = %q, want Linux", strings.TrimSpace(r.Stdout))
	}
}

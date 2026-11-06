// Package conformance runs the shared SDK conformance rows (spec/{wire,errors,retry}.json, vendored from the Solari SDK conformance source
// sdk/conformance, see SOURCE.txt) against THIS repository's public Go SDK through the scripted mock gateway.
//
//	go test ./tests/conformance/        (needs node 18+ for the mock gateway and the spec loader)
//
// A row listed in knownGaps.json must STILL FAIL (a gap cannot decay into a skip); an op this adapter lacks must be listed in
// unsupported.json (an unlisted gap fails).
package conformance_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	solari "github.com/solari-sdk/solari-sandbox-go"
)

type row struct {
	Key    string          `json:"key"`
	ID     string          `json:"id"`
	Op     string          `json:"op"`
	Input  json.RawMessage `json:"input"`
	Mock   json.RawMessage `json:"mock"`
	Expect struct {
		Result   map[string]any `json:"result"`
		Error    map[string]any `json:"error"`
		Requests *int           `json:"requests"`
		WsCalls  *int           `json:"wsCalls"`
	} `json:"expect"`
	WS json.RawMessage `json:"ws"`
}

type listed struct {
	Op  string `json:"op"`
	ID  string `json:"id"`
	Why string `json:"why"`
}

func readList(t *testing.T, path string) []listed {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var l []listed
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return l
}

func needNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for the conformance runner")
		}
		t.Skip("node not found: the conformance runner needs node 18+ (mock gateway + spec loader)")
	}
}

func loadRows(t *testing.T) []row {
	t.Helper()
	out, err := exec.Command("node", "dump-rows.mjs").Output()
	if err != nil {
		t.Fatalf("node dump-rows.mjs: %v", err)
	}
	var rows []row
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no conformance rows")
	}
	return rows
}

// mockProc is one mock gateway process (node mock-server.mjs --port 0).
type mockProc struct {
	cmd  *exec.Cmd
	URL  string
	http *http.Client
}

func startMock(t *testing.T) *mockProc {
	t.Helper()
	cmd := exec.Command("node", "mock-server.mjs", "--port", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("mock gateway printed no address: %v", err)
	}
	var addr struct{ URL string }
	if err := json.Unmarshal(line, &addr); err != nil || addr.URL == "" {
		_ = cmd.Process.Kill()
		t.Fatalf("mock gateway address %q: %v", line, err)
	}
	m := &mockProc{cmd: cmd, URL: addr.URL, http: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return m
}

func (m *mockProc) post(t *testing.T, path string, body []byte) {
	t.Helper()
	resp, err := m.http.Post(m.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, b)
	}
}

type mockLog struct {
	Requests []struct {
		Upgrade string `json:"upgrade"`
	} `json:"requests"`
	Mismatches []struct {
		Request string `json:"request"`
		Reason  string `json:"reason"`
	} `json:"mismatches"`
	Remaining int               `json:"remaining"`
	WsCalls   []json.RawMessage `json:"wsCalls"`
}

func (m *mockProc) log(t *testing.T) mockLog {
	t.Helper()
	resp, err := m.http.Get(m.URL + "/__log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var l mockLog
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

// ---- adapter: abstract op -> the Go SDK, errors -> { type, status, code, retryable } ----

type createInput struct {
	Template     string                    `json:"template"`
	CPU          int                       `json:"cpu"`
	MemMb        int                       `json:"memMb"`
	DiskGb       int                       `json:"diskGb"`
	Envs         map[string]string         `json:"envs"`
	Metadata     map[string]string         `json:"metadata"`
	TimeoutMs    int                       `json:"timeoutMs"`
	TtlSeconds   int                       `json:"ttlSeconds"`
	FromSnapshot string                    `json:"fromSnapshot"`
	Isolation    string                    `json:"isolation"`
	Lifecycle    *solari.SandboxLifecycle  `json:"lifecycle"`
	Resolution   string                    `json:"resolution"`
	Record       *bool                     `json:"record"`
	Volumes      []solari.VolumeAttachment `json:"volumes"`
	ID           string                    `json:"id"`
	// commands.run rows
	Via  string   `json:"via"`
	User string   `json:"user"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

func (c createInput) options(kind solari.SandboxKind) solari.CreateOptions {
	return solari.CreateOptions{
		Template: c.Template, Kind: kind, CPU: c.CPU, MemMb: c.MemMb, DiskGb: c.DiskGb, Envs: c.Envs, Metadata: c.Metadata,
		TimeoutMs: c.TimeoutMs, TtlSeconds: c.TtlSeconds, FromSnapshot: c.FromSnapshot, Isolation: solari.SandboxIsolation(c.Isolation),
		Lifecycle: c.Lifecycle, Resolution: c.Resolution, Record: c.Record, Volumes: c.Volumes,
	}
}

func sandboxView(s *solari.Sandbox) map[string]any {
	v := map[string]any{"sandboxId": s.ID}
	if s.Kind != "" {
		v["kind"] = string(s.Kind)
	}
	if s.StreamURL != "" {
		v["streamUrl"] = s.StreamURL
	}
	if s.RecordingURL != "" {
		v["recordingUrl"] = s.RecordingURL
	}
	return v
}

type adapterFn func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error)

// A handle op first creates a sandbox (the row's mock lists that exchange), then calls the method on the handle.
func viaHandle(f func(ctx context.Context, s *solari.Sandbox) error) adapterFn {
	return func(ctx context.Context, c *solari.Client, _ createInput) (map[string]any, error) {
		s, err := c.Create(ctx, solari.CreateOptions{Template: "base"})
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return map[string]any{}, f(ctx, s)
	}
}

var ops = map[string]adapterFn{
	"sandbox.create": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		s, err := c.Create(ctx, in.options(solari.KindSandbox))
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return sandboxView(s), nil
	},
	"sandbox.createDesktop": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		s, err := c.Create(ctx, in.options(solari.KindDesktop))
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return sandboxView(s), nil
	},
	"sandbox.get": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		v, err := c.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		r := map[string]any{"sandboxId": v.SandboxID}
		if v.Kind != "" {
			r["kind"] = string(v.Kind)
		}
		if v.State != "" {
			r["state"] = v.State
		}
		return r, nil
	},
	"sandbox.connect": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		s, err := c.Connect(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return sandboxView(s), nil
	},
	// via "ws": a per-command env makes the SDK use the live control connection; via "rest" nothing does (no open channel).
	"commands.run": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		s, err := c.Create(ctx, solari.CreateOptions{Template: "base"})
		if err != nil {
			return nil, err
		}
		defer s.Close()
		o := solari.CommandOptions{Args: in.Args, TimeoutMs: in.TimeoutMs, User: in.User}
		if in.Via == "ws" {
			o.Env = map[string]string{"X2": "1"}
		}
		// A cancellable ctx would (by design) take the live connection; the REST rows run with one that cannot be cancelled.
		r, err := s.Commands.Run(context.WithoutCancel(ctx), in.Cmd, o)
		if err != nil {
			return nil, err
		}
		return map[string]any{"exitCode": float64(r.ExitCode), "stdout": r.Stdout, "stderr": r.Stderr, "timedOut": r.TimedOut, "oomKilled": r.OOMKilled}, nil
	},
	// A call with a cancellable context: it must use the live connection (cancel sends cmd.kill there).
	"commands.runCancellable": func(ctx context.Context, c *solari.Client, in createInput) (map[string]any, error) {
		s, err := c.Create(ctx, solari.CreateOptions{Template: "base"})
		if err != nil {
			return nil, err
		}
		defer s.Close()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r, err := s.Commands.Run(cctx, in.Cmd, solari.CommandOptions{Args: in.Args, TimeoutMs: in.TimeoutMs})
		if err != nil {
			return nil, err
		}
		return map[string]any{"exitCode": float64(r.ExitCode), "stdout": r.Stdout, "stderr": r.Stderr, "timedOut": r.TimedOut, "oomKilled": r.OOMKilled}, nil
	},
	"sandbox.pause":  viaHandle(func(ctx context.Context, s *solari.Sandbox) error { return s.Pause(ctx) }),
	"sandbox.resume": viaHandle(func(ctx context.Context, s *solari.Sandbox) error { return s.Resume(ctx) }),
	"sandbox.start":  viaHandle(func(ctx context.Context, s *solari.Sandbox) error { return s.Start(ctx) }),
	"sandbox.kill":   viaHandle(func(ctx context.Context, s *solari.Sandbox) error { return s.Kill(ctx) }),
}

// normalizeError maps the SDK's error classes onto the spec's neutral types. Most specific first.
func normalizeError(err error) map[string]any {
	e := map[string]any{"type": fmt.Sprintf("other:%T", err), "message": err.Error()}
	var (
		auth *solari.AuthError
		plan *solari.PlanError
		stop *solari.SandboxNotStoppedError
		conc *solari.ConcurrencyLimitError
		capa *solari.NoCapacityError
		act  *solari.ActionError
		val  *solari.ValidationError
		gw   *solari.GatewayError
	)
	set := func(g *solari.GatewayError, typ string) {
		e["type"], e["status"], e["code"] = typ, float64(g.Status), g.Code
		if g.Body != nil {
			e["retryable"] = g.Body.Retryable
		}
	}
	switch {
	case g(err, &val):
		set(&val.GatewayError, "validation")
	case g(err, &act):
		e["type"], e["code"] = "action", act.Code
	case g(err, &stop):
		set(&stop.GatewayError, "not_stopped")
	case g(err, &auth):
		set(&auth.GatewayError, "auth")
	case g(err, &plan):
		set(&plan.GatewayError, "plan")
	case g(err, &conc):
		set(&conc.GatewayError, "concurrency_limit")
	case g(err, &capa):
		set(&capa.GatewayError, "no_capacity")
	case g(err, &gw):
		set(gw, "gateway")
	}
	return e
}

func g[T any](err error, target *T) bool { return errors.As(err, target) }

// ---- comparison: expected is a deep SUBSET of actual ----

func subsetMismatch(exp, act any, path string) string {
	switch e := exp.(type) {
	case map[string]any:
		a, ok := act.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: expected an object, got %v", path, act)
		}
		for k, ev := range e {
			av, present := a[k]
			if !present {
				return fmt.Sprintf("%s.%s: missing (expected %v)", path, k, ev)
			}
			if m := subsetMismatch(ev, av, path+"."+k); m != "" {
				return m
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(exp, act) {
			return fmt.Sprintf("%s: expected %v, got %v", path, exp, act)
		}
		return ""
	}
}

var placeholder = regexp.MustCompile(`\$MOCK_(WS|URL)`)

// check runs one row against its own mock; "" = pass, else why it fails.
func check(t *testing.T, r row) string {
	m := startMock(t)
	mock := string(r.Mock)
	if mock == "" || mock == "null" {
		mock = "[]"
	}
	mock = placeholder.ReplaceAllStringFunc(mock, func(s string) string {
		if s == "$MOCK_WS" {
			return strings.Replace(m.URL, "http", "ws", 1)
		}
		return m.URL
	})
	ws := string(r.WS)
	if ws == "" || ws == "null" {
		ws = "[]"
	}
	ws = placeholder.ReplaceAllStringFunc(ws, func(s string) string {
		if s == "$MOCK_WS" {
			return strings.Replace(m.URL, "http", "ws", 1)
		}
		return m.URL
	})
	// strict: a row that counts control calls must see none it did not script (how "no control call was made" is proven).
	m.post(t, "/__arm", []byte(fmt.Sprintf(`{"mock":%s,"ws":%s,"strict":%v}`, mock, ws, r.Expect.WsCalls != nil)))

	var in createInput
	if len(r.Input) > 0 {
		_ = json.Unmarshal(r.Input, &in)
	}
	zero := 0 // no backoff wait: the rows check what is retried and with which key, not the sleep
	client, err := solari.NewClient(solari.ClientOptions{APIKey: "conformance-test-key", BaseURL: m.URL, RetryDelayMs: &zero})
	if err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, opErr := ops[r.Op](ctx, client, in)

	l := m.log(t)
	requests := 0
	for _, q := range l.Requests {
		if q.Upgrade == "" {
			requests++
		}
	}
	if len(l.Mismatches) > 0 {
		var parts []string
		for _, mm := range l.Mismatches {
			parts = append(parts, mm.Request+": "+mm.Reason)
		}
		return "request did not fit the script: " + strings.Join(parts, "; ")
	}
	switch {
	case r.Expect.Error != nil:
		if opErr == nil {
			return fmt.Sprintf("expected error %v, got result %v", r.Expect.Error, res)
		}
		norm := normalizeError(opErr)
		if mm := subsetMismatch(any(r.Expect.Error), any(norm), "error"); mm != "" {
			return fmt.Sprintf("%s (message: %v)", mm, norm["message"])
		}
	default:
		if opErr != nil {
			return fmt.Sprintf("expected success, got error %v", normalizeError(opErr))
		}
		if r.Expect.Result != nil {
			if mm := subsetMismatch(any(r.Expect.Result), any(res), "result"); mm != "" {
				return mm
			}
		}
	}
	if r.Expect.Requests != nil && requests != *r.Expect.Requests {
		return fmt.Sprintf("expected %d request(s), made %d", *r.Expect.Requests, requests)
	}
	if r.Expect.WsCalls != nil && len(l.WsCalls) != *r.Expect.WsCalls {
		return fmt.Sprintf("expected %d control call(s), made %d", *r.Expect.WsCalls, len(l.WsCalls))
	}
	if l.Remaining != 0 {
		return fmt.Sprintf("%d scripted exchange(s) never happened", l.Remaining)
	}
	return ""
}

func TestConformanceRows(t *testing.T) {
	needNode(t)
	rows := loadRows(t)
	unsupported := map[string]string{}
	for _, u := range readList(t, "unsupported.json") {
		unsupported[u.Op] = u.Why
	}
	gaps := map[string]listed{}
	for _, k := range readList(t, "knownGaps.json") {
		gaps[k.ID] = k
	}
	keys := map[string]bool{}
	usedOps := map[string]bool{}
	for _, r := range rows {
		keys[r.Key] = true
		usedOps[r.Op] = true
	}
	for id := range gaps {
		if !keys[id] {
			t.Errorf("knownGaps entry %s matches no row", id)
		}
	}
	for op := range unsupported {
		if !usedOps[op] {
			t.Errorf("unsupported entry %s matches no row", op)
		}
	}
	for _, r := range rows {
		r := r
		t.Run(r.Key, func(t *testing.T) {
			t.Parallel()
			if _, ok := ops[r.Op]; !ok {
				if _, listed := unsupported[r.Op]; !listed {
					t.Fatalf("%s is not implemented by the Go adapter and not listed in unsupported.json", r.Op)
				}
				return
			}
			failure := check(t, r)
			if gap, ok := gaps[r.Key]; ok {
				if failure == "" {
					t.Fatalf("%s is listed in knownGaps.json (%s) but now PASSES: delete the entry", r.Key, gap.Why)
				}
				return
			}
			if failure != "" {
				t.Fatal(failure)
			}
		})
	}
}

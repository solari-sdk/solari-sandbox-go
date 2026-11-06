package solari

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// POST /sandboxes must carry the Authorization + Idempotency-Key headers, a
// JSON body with the expected fields, and Content-Type application/json.
func TestCreateRequestShape(t *testing.T) {
	var gotAuth, gotIdem, gotCT, gotAccept, gotMethod, gotPath string
	var body map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"sandbox","controlUrl":"wss://gw/control/sbx_1","expiresAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := c.Create(context.Background(), CreateOptions{Template: "base", CPU: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if gotMethod != "POST" || gotPath != "/sandboxes" {
		t.Errorf("method/path: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth header: %q", gotAuth)
	}
	if gotIdem == "" {
		t.Error("missing Idempotency-Key header")
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: %q", gotCT)
	}
	if gotAccept != "application/json" {
		t.Errorf("accept: %q", gotAccept)
	}
	if body["template"] != "base" || body["kind"] != "sandbox" {
		t.Errorf("body: %+v", body)
	}
	if v, ok := body["cpu"].(float64); !ok || v != 2 {
		t.Errorf("body cpu: %v", body["cpu"])
	}
	if sb.ID != "sbx_1" || sb.ControlURL != "wss://gw/control/sbx_1" {
		t.Errorf("sandbox: id=%q controlUrl=%q", sb.ID, sb.ControlURL)
	}
}

// A create hitting a 429 maps to *ConcurrencyLimitError and is NOT retried.
// Create with KindDesktop must send kind:"desktop" and surface the streamUrl
// the gateway returns for desktop-kind VMs onto the handle.
func TestCreateDesktopSurfacesStreamURL(t *testing.T) {
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		// Mirrors the gateway: streamUrl is present for kind:"desktop" only.
		_, _ = w.Write([]byte(`{"sandboxId":"sbx_d","kind":"desktop","controlUrl":"wss://gw/control/sbx_d","expiresAt":"2026-01-01T00:00:00Z","streamUrl":"wss://gw/stream/sbx_d"}`))
	}))
	defer srv.Close()

	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := c.Create(context.Background(), CreateOptions{Kind: KindDesktop, Template: "default"})
	if err != nil {
		t.Fatalf("create desktop: %v", err)
	}

	// The gateway keys desktop-ness off this field.
	if body["kind"] != "desktop" {
		t.Errorf("kind on wire = %v, want desktop", body["kind"])
	}
	if sb.StreamURL != "wss://gw/stream/sbx_d" {
		t.Errorf("StreamURL = %q, want wss://gw/stream/sbx_d", sb.StreamURL)
	}
	if sb.Kind != KindDesktop {
		t.Errorf("Kind = %q, want desktop", sb.Kind)
	}
}

// A headless sandbox has no display, so the gateway omits streamUrl and the
// handle must not invent one.
func TestCreateSandboxHasNoStreamURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"sandbox","controlUrl":"wss://gw/control/sbx_1","expiresAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Create(context.Background(), CreateOptions{Template: "base"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.StreamURL != "" {
		t.Errorf("StreamURL = %q, want empty for a headless sandbox", sb.StreamURL)
	}
}

// GET /sandboxes/:id carries no streamUrl (the gateway view serializer omits
// it), so Connect derives it for desktops from the same origin as controlUrl.
func TestConnectDerivesStreamURLForDesktop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Exactly what toSandboxView emits: no controlUrl, no streamUrl.
		_, _ = w.Write([]byte(`{"sandboxId":"sbx_d","kind":"desktop","state":"running","expiresAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Connect(context.Background(), "sbx_d")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	wsOrigin := c.http.wsOrigin()
	if want := wsOrigin + "/stream/sbx_d"; sb.StreamURL != want {
		t.Errorf("StreamURL = %q, want %q", sb.StreamURL, want)
	}
	if want := wsOrigin + "/control/sbx_d"; sb.ControlURL != want {
		t.Errorf("ControlURL = %q, want %q", sb.ControlURL, want)
	}
}

// Connecting to a headless sandbox must not derive a stream URL.
func TestConnectSandboxHasNoStreamURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"sandbox","state":"running","expiresAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Connect(context.Background(), "sbx_1")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if sb.StreamURL != "" {
		t.Errorf("StreamURL = %q, want empty for a headless sandbox", sb.StreamURL)
	}
}

func TestCreate429ConcurrencyLimitNotRetried(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":"ConcurrencyLimitExceeded","message":"at cap"}`))
	}))
	defer srv.Close()

	zero := 0
	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL, RetryDelayMs: &zero})
	_, err := c.Create(context.Background(), CreateOptions{Template: "base"})
	var cl *ConcurrencyLimitError
	if err == nil {
		t.Fatal("expected error")
	}
	// errors.As via the concrete type.
	if e, ok := asConcurrency(err); !ok {
		t.Fatalf("expected *ConcurrencyLimitError, got %T", err)
	} else {
		cl = e
	}
	if cl.Status != 429 {
		t.Errorf("status: %d", cl.Status)
	}
	if attempts != 1 {
		t.Errorf("429 must not be retried, got %d attempts", attempts)
	}
}

// The one-shot exec fast path: Commands.Run on an unconnected sandbox POSTs to
// /sandboxes/:id/exec instead of opening the control WS.
func TestExecFastPath(t *testing.T) {
	var execHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/sandboxes":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"sandbox","controlUrl":"wss://gw/control/sbx_1","expiresAt":""}`))
		case r.Method == "POST" && r.URL.Path == "/sandboxes/sbx_1/exec":
			execHit = true
			var req map[string]interface{}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			if req["cmd"] != "echo" {
				t.Errorf("exec cmd: %v", req["cmd"])
			}
			_, _ = w.Write([]byte(`{"exitCode":0,"stdout":"ok\n","stderr":""}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Create(context.Background(), CreateOptions{Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Commands.Run(context.Background(), "echo", CommandOptions{Args: []string{"ok"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !execHit {
		t.Error("expected the /exec fast path to be used")
	}
	if res.Stdout != "ok\n" || res.ExitCode != 0 {
		t.Errorf("result: %+v", res)
	}
}

// A 404 from /exec that is a gateway answer (JSON: the sandbox is gone) must reach the caller. It used to be read as
// "route not served", fall back to the control WS, and surface as "call Connect() first".
func TestExecDeletedSandboxIsNotMistakenForAMissingRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"sandbox","controlUrl":"ws://127.0.0.1:1/control/sbx_1","expiresAt":""}`))
		case r.URL.Path == "/sandboxes/sbx_1/exec":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Not found","requestId":"req_1"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Create(context.Background(), CreateOptions{Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sb.Commands.Run(context.Background(), "true", CommandOptions{})
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != 404 {
		t.Fatalf("err = %v, want the gateway's 404", err)
	}
}

func TestIsRouteUnavailable(t *testing.T) {
	plain := &GatewayError{SolariError: SolariError{Message: "404"}, Status: 404}
	json404 := &GatewayError{SolariError: SolariError{Message: "gone"}, Status: 404, Body: &GatewayErrorBody{Error: "Not found"}}
	coded := &GatewayError{SolariError: SolariError{Message: "x"}, Status: 404, Code: "SandboxNotFound", Body: &GatewayErrorBody{Code: "SandboxNotFound"}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"plain-text 404 (no such route)", plain, true},
		{"JSON 404 (sandbox gone)", json404, false},
		{"coded 404", coded, false},
		{"405", &GatewayError{Status: 405}, true},
		{"501", &GatewayError{Status: 501}, true},
		{"500", &GatewayError{Status: 500}, false},
		{"not a gateway error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isRouteUnavailable(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestExecTimedOutAndRecordingURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandboxes":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"sandboxId":"sbx_1","kind":"desktop","controlUrl":"ws://127.0.0.1:1/control/sbx_1","expiresAt":"","recordingUrl":"https://rec.example/r/1"}`))
		case "/sandboxes/sbx_1/exec":
			_, _ = w.Write([]byte(`{"exitCode":-1,"stdout":"","stderr":"","timedOut":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	sb, err := c.Create(context.Background(), CreateOptions{Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if sb.RecordingURL != "https://rec.example/r/1" {
		t.Errorf("RecordingURL = %q", sb.RecordingURL)
	}
	res, err := sb.Commands.Run(context.Background(), "sleep", CommandOptions{Args: []string{"30"}, TimeoutMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("result = %+v, want TimedOut with exit -1", res)
	}
}

func asConcurrency(err error) (*ConcurrencyLimitError, bool) {
	var e *ConcurrencyLimitError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// startServer answers POST /sandboxes/:id/start with the given status/body and
// records the request line.
func startServer(t *testing.T, status int, body map[string]any, got *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Method + " " + r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOptions{APIKey: "slr_test_x", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientStartReturnsFreshControlURL(t *testing.T) {
	var got string
	c := startServer(t, 200, map[string]any{"state": "running", "controlUrl": "wss://gw.test/control/fresh"}, &got)
	url, err := c.Start(context.Background(), "pool-a:vm-7")
	if err != nil {
		t.Fatal(err)
	}
	if url != "wss://gw.test/control/fresh" {
		t.Errorf("control url = %q", url)
	}
	if got != "POST /sandboxes/pool-a%3Avm-7/start" {
		t.Errorf("wire = %q", got)
	}
}

func TestClientStartAlreadyRunningStillReturnsURL(t *testing.T) {
	var got string
	c := startServer(t, 200, map[string]any{"state": "running", "controlUrl": "wss://gw.test/control/cur", "alreadyRunning": true}, &got)
	url, err := c.Start(context.Background(), "sbx1")
	if err != nil || url != "wss://gw.test/control/cur" {
		t.Fatalf("url=%q err=%v", url, err)
	}
}

func TestClientStartDerivesURLWhenOmitted(t *testing.T) {
	var got string
	c := startServer(t, 200, map[string]any{"state": "running"}, &got)
	url, err := c.Start(context.Background(), "sbx1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "ws") || !strings.HasSuffix(url, "/control/sbx1") {
		t.Errorf("derived url = %q", url)
	}
}

func TestClientStartRefusalCodes(t *testing.T) {
	for _, code := range []string{"NotStopped", "StartInProgress", "StoppedDiskMissing", "StoppedDiskUnsafe", "GoldenMismatch", "DiskPlatformMismatch", "DiskShaMismatch", "DiskExceedsSpec", "DiskOwnerMismatch", "HostCapabilityMissing", "SandboxStopped"} {
		var got string
		c := startServer(t, 409, map[string]any{"code": code, "error": "refused", "state": "paused", "retryable": false}, &got)
		_, err := c.Start(context.Background(), "sbx1")
		var ge *GatewayError
		if !errors.As(err, &ge) {
			t.Fatalf("%s: err %v is not a *GatewayError", code, err)
		}
		if ge.Status != 409 || ge.Code != code {
			t.Errorf("%s: status=%d code=%q", code, ge.Status, ge.Code)
		}
	}
}

func TestClientStartNotStoppedIsTyped(t *testing.T) {
	for _, code := range []string{"NotStopped", "StartInProgress"} {
		var got string
		c := startServer(t, 409, map[string]any{"code": code, "error": "refused", "state": "paused", "retryable": false}, &got)
		_, err := c.Start(context.Background(), "sbx1")
		var ns *SandboxNotStoppedError
		if !errors.As(err, &ns) {
			t.Fatalf("%s: err %v is not a *SandboxNotStoppedError", code, err)
		}
		if ns.Status != 409 || ns.Code != code || ns.State != "paused" {
			t.Errorf("%s: status=%d code=%q state=%q", code, ns.Status, ns.Code, ns.State)
		}
		var ge *GatewayError
		if !errors.As(err, &ge) {
			t.Errorf("%s: a typed not-stopped error must still match *GatewayError", code)
		}
	}
	// Other 409s stay plain gateway errors: a disk refusal is not "not stopped".
	var got string
	c := startServer(t, 409, map[string]any{"code": "StoppedDiskMissing", "error": "x", "retryable": false}, &got)
	_, err := c.Start(context.Background(), "sbx1")
	var ns *SandboxNotStoppedError
	if errors.As(err, &ns) {
		t.Errorf("StoppedDiskMissing must not be a *SandboxNotStoppedError")
	}
}

func TestClientStartCapacityAndLimit(t *testing.T) {
	var got string
	c := startServer(t, 429, map[string]any{"error": "cap"}, &got)
	var cl *ConcurrencyLimitError
	if _, err := c.Start(context.Background(), "sbx1"); !errors.As(err, &cl) {
		t.Errorf("429: want *ConcurrencyLimitError, got %v", err)
	}
	c2 := startServer(t, 500, map[string]any{"code": "StartFailed", "error": "x", "retryable": false}, &got)
	var ge *GatewayError
	if _, err := c2.Start(context.Background(), "sbx1"); !errors.As(err, &ge) || ge.Code != "StartFailed" {
		t.Errorf("500: got %v", err)
	}
}

func TestSandboxStartNeedsClientHandle(t *testing.T) {
	sb := &Sandbox{}
	if err := sb.Start(context.Background()); err == nil {
		t.Error("want error for a handle without a client")
	}
}

func TestNewClientDefaultsBaseURL(t *testing.T) {
	if DefaultBaseURL != "https://api.getsolari.com" {
		t.Fatalf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	c, err := NewClient(ClientOptions{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewClient with no BaseURL: %v", err)
	}
	if c.http.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", c.http.baseURL, DefaultBaseURL)
	}
	c, err = NewClient(ClientOptions{APIKey: "k", BaseURL: "https://gw.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.baseURL != "https://gw.example.com" {
		t.Fatalf("explicit BaseURL not kept: %q", c.http.baseURL)
	}
}

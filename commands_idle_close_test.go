package solari

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A command that exits before its handle exists (a fast `true`: the cmd.exit frame is in the same websocket message as the
// cmd.start reply, so it is buffered as an orphan) must not leave its "channel closed" hook registered. Registering the frame
// handlers replays the buffered cmd.exit, and the exit handler unregisters the hook; the hook therefore has to be registered
// BEFORE the frame handlers, or it is registered after that unregister and leaks. A leaked hook makes the channel count the
// socket as in use, so its idle close never runs and the sandbox outlives its timeout (retest B-N1 / C4).
func TestCommandThatExitsBeforeItsHandleExistsDoesNotPinTheSocketOpen(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		reply := `{"id":"1","ok":true,"result":{"cmdId":"c1"}}`
		exit := `{"type":"cmd.exit","cmdId":"c1","exitCode":0}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(exit+"\n"+reply+"\n")) // the exit first: it is buffered as an orphan for certain
		time.Sleep(300 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	h, err := sb.Commands.Start(ctx, "true", CommandOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if code, err := h.Wait(wctx); err != nil || code != 0 {
		t.Fatalf("wait: code=%d err=%v", code, err)
	}
	sb.ch.mu.Lock()
	leaked := len(sb.ch.streamClosers)
	sb.ch.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d stream-close hook(s) still registered after the command exited: the channel would never idle-close", leaked)
	}
}

// --- idle close (retest C-17) ------------------------------------------------------------------------------------------
// An open control socket counts as activity at the gateway, so one left open keeps an idle sandbox from pausing or expiring
// on time. The channel closes the socket after idleClose with nothing using it and reopens it by itself on the next call.

// idleServer answers every control request with an empty ok reply (or a cmd.start reply for "cmd.start") and counts the
// connections it has accepted and how many have been closed by the client.
type idleServer struct {
	mu      sync.Mutex
	opened  int
	closed  int
	exitCh  chan struct{} // closed to make the server send cmd.exit for c1
	exitSet bool
	delay   time.Duration // how long a reply to fs.slow is held back
	dropCh  chan struct{} // closed to make the server drop connection number dropNth
	dropNth int
}

func newIdleServer(t *testing.T) (*idleServer, string, func()) {
	t.Helper()
	is := &idleServer{exitCh: make(chan struct{}), dropCh: make(chan struct{})}
	url, stop := startMockControlServer(t, func(conn *websocket.Conn) {
		is.mu.Lock()
		is.opened++
		n := is.opened
		is.mu.Unlock()
		go func() {
			select {
			case <-is.dropCh:
				if n == is.dropNth {
					conn.Close()
				}
			case <-time.After(10 * time.Second):
			}
		}()
		go func() {
			select {
			case <-is.exitCh:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":0}`+"\n"))
			case <-time.After(10 * time.Second):
			}
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				is.mu.Lock()
				is.closed++
				is.mu.Unlock()
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(msg, &req)
			res := `{}`
			if req.Method == "cmd.start" {
				res = `{"cmdId":"c1"}`
			}
			if req.Method == "fs.slow" {
				time.Sleep(is.delay)
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"`+req.ID+`","ok":true,"result":`+res+`}`+"\n"))
		}
	})
	return is, url, stop
}

func (is *idleServer) counts() (opened, closed int) {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.opened, is.closed
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIdleControlSocketIsClosedAndReopenedByTheNextCall(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(200)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err := sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": "/"}, callOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	waitFor(t, "the idle socket to be closed", 3*time.Second, func() bool { _, c := is.counts(); return c == 1 })
	if sb.Connected() {
		t.Fatalf("Connected() is true after the idle close")
	}
	// the next call reopens it by itself
	if _, err := sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": "/"}, callOptions{}); err != nil {
		t.Fatalf("call after the idle close: %v", err)
	}
	if o, _ := is.counts(); o != 2 {
		t.Fatalf("connections opened = %d, want 2 (reopened lazily)", o)
	}
	waitFor(t, "the reopened socket to idle-close again", 3*time.Second, func() bool { _, c := is.counts(); return c == 2 })
}

func TestRunningCommandKeepsTheSocketOpenAndItClosesAfterTheCommandEnds(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(200)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	h, err := sb.Commands.Start(ctx, "sleep", CommandOptions{Args: []string{"60"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // several idle windows
	if _, c := is.counts(); c != 0 || !sb.Connected() {
		t.Fatalf("the socket was closed (%d closed, connected=%v) while a command was running", c, sb.Connected())
	}
	close(is.exitCh) // the command ends
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := h.Wait(wctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waitFor(t, "the socket to close after the command ended", 3*time.Second, func() bool { _, c := is.counts(); return c == 1 })
}

func TestFastCommandDoesNotKeepTheSocketOpen(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(200)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	close(is.exitCh) // the exit frame is sent at once, so it can arrive before the handle exists
	time.Sleep(50 * time.Millisecond)
	h, err := sb.Commands.Start(ctx, "true", CommandOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := h.Wait(wctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waitFor(t, "the socket to idle-close after a fast command", 3*time.Second, func() bool { _, c := is.counts(); return c == 1 })
}

// ClientOptions.IdleCloseMs reaches the channel: 0 is the 3 s default, a negative value disables the idle close, a positive one is used.
func TestClientIdleCloseMsSemantics(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{{0, 3 * time.Second}, {-1, 0}, {250, 250 * time.Millisecond}} {
		c, err := NewClient(ClientOptions{APIKey: "slr_test_x", BaseURL: "http://127.0.0.1:1", IdleCloseMs: tc.ms})
		if err != nil {
			t.Fatal(err)
		}
		sb, err := c.newSandbox(CreateSandboxResponse{SandboxID: "sb", ControlURL: "ws://127.0.0.1:1/control/sb"})
		if err != nil {
			t.Fatal(err)
		}
		sb.ch.mu.Lock()
		got := sb.ch.idleClose
		sb.ch.mu.Unlock()
		if got != tc.want {
			t.Errorf("IdleCloseMs=%d: idleClose = %v, want %v", tc.ms, got, tc.want)
		}
	}
}

func TestNegativeIdleCloseKeepsTheSocketOpen(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(-1)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err := sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": "/"}, callOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, c := is.counts(); c != 0 || !sb.Connected() {
		t.Fatalf("the socket was closed (%d) although the idle close is disabled", c)
	}
}

// Activity resets the timer: calls spaced inside the window keep the socket open, and it closes only after the last one.
func TestActivityKeepsTheSocketOpenUntilTheLastCall(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(400)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	for i := 0; i < 6; i++ { // 6 x 200 ms = 1.2 s, three idle windows
		if _, err := sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": "/"}, callOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, c := is.counts(); c != 0 {
			t.Fatalf("the socket closed after %d call(s) although each came inside the window", i+1)
		}
		time.Sleep(200 * time.Millisecond)
	}
	waitFor(t, "the socket to close after the last call", 3*time.Second, func() bool { _, c := is.counts(); return c == 1 })
}

// A call in flight keeps the socket: its reply arrives after the idle window and the call still succeeds on the same socket.
func TestInFlightCallKeepsTheSocketPastTheIdleWindow(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	is.delay = 700 * time.Millisecond
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(200)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err := sb.ch.call(ctx, "fs.slow", map[string]interface{}{}, callOptions{}); err != nil {
		t.Fatalf("a slow call lost its socket to the idle close: %v", err)
	}
	if o, _ := is.counts(); o != 1 {
		t.Fatalf("connections opened = %d, want 1 (the slow call must not have been reopened)", o)
	}
}

// An idle close is not a drop: what the channel keeps for a later handler (a buffered frame) survives it, and a socket reopened by
// hand and then dropped by the peer is a real drop (the next call reports it, it does not silently reopen).
func TestIdleCloseIsNotADrop(t *testing.T) {
	is, url, stop := newIdleServer(t)
	defer stop()
	sb := newTestSandbox(url)
	sb.ch.setIdleClose(200)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	sb.ch.feed([]byte(`{"type":"cmd.exit","cmdId":"c9","exitCode":0}` + "\n")) // buffered: no handler yet
	waitFor(t, "the idle close", 3*time.Second, func() bool { _, c := is.counts(); return c == 1 })
	sb.ch.mu.Lock()
	kept := len(sb.ch.orphanFrames)
	sb.ch.mu.Unlock()
	if kept != 1 {
		t.Fatalf("the idle close dropped the buffered frame (orphan frames = %d): it was treated as a drop", kept)
	}
	// reopen by hand, then the peer drops it for real
	sb.ch.setIdleClose(-1)
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	is.dropNth = 2
	close(is.dropCh)
	waitFor(t, "the drop to be noticed", 3*time.Second, func() bool { return !sb.Connected() })
	if _, err := sb.ch.call(ctx, "fs.stat", map[string]interface{}{"path": "/"}, callOptions{}); err == nil {
		t.Fatalf("a call after a real drop silently reopened the socket (idleClosed was not cleared by the reconnect)")
	}
}

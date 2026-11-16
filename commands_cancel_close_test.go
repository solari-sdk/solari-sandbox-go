package solari

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Retest 2026-11-16, R4: cancelling a Commands.Run and calling Sandbox.Close() straight away left the guest process running. The
// cancelled Run sends SIGTERM from its own goroutine so the caller returns at once, and Close() tore the socket down before that
// goroutine had written the cmd.kill (raw 4/4 survived on staging; with 500 ms between cancel and Close they were all killed).
// Close() now waits (bounded) for the kills already on their way.

// killRecorder answers cmd.start with a cmdId and records every method it receives; a cmd.kill gets an ok reply.
type killRecorder struct {
	mu      sync.Mutex
	methods []string
	slow    time.Duration // how long cmd.kill is held before it is answered (a slow link)
}

func (k *killRecorder) saw(method string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, m := range k.methods {
		if m == method {
			return true
		}
	}
	return false
}

func newKillRecorder(t *testing.T) (*killRecorder, string, func()) {
	t.Helper()
	k := &killRecorder{}
	url, stop := startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var req struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal([]byte(line), &req) != nil || req.Method == "" {
					continue
				}
				k.mu.Lock()
				k.methods = append(k.methods, req.Method)
				slow := k.slow
				k.mu.Unlock()
				result := `{}`
				if req.Method == "cmd.start" {
					result = `{"cmdId":"c1"}`
				}
				if req.Method == "cmd.kill" && slow > 0 {
					time.Sleep(slow)
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"`+req.ID+`","ok":true,"result":`+result+"}\n"))
			}
		}
	})
	return k, url, stop
}

// runThenCancelAndClose starts a streaming Run, cancels it once the guest has the command, and closes the sandbox immediately.
func runThenCancelAndClose(t *testing.T, k *killRecorder, wsURL string) {
	t.Helper()
	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sb.Commands.Run(runCtx, "sleep", CommandOptions{Args: []string{"60"}, OnStdout: func(string) {}})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !k.saw("cmd.start") {
		if time.Now().After(deadline) {
			t.Fatal("the server never saw cmd.start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the Run has its handle and is waiting for the exit
	cancel()
	sb.Close() // immediately
	<-done
}

func TestCancelThenImmediateCloseStillDeliversTheKill(t *testing.T) {
	k, url, stop := newKillRecorder(t)
	defer stop()
	runThenCancelAndClose(t, k, url)
	time.Sleep(300 * time.Millisecond)
	if !k.saw("cmd.kill") {
		t.Fatalf("a cancelled Run followed by Close() never delivered cmd.kill (server saw %v): the command keeps running in the guest", k.methods)
	}
}

func TestCloseWaitsForTheKillButOnlyBrieflyOnAStalledLink(t *testing.T) {
	k, url, stop := newKillRecorder(t)
	defer stop()
	k.slow = 10 * time.Second // the server never answers the kill in time
	start := time.Now()
	runThenCancelAndClose(t, k, url)
	if d := time.Since(start); d > closeFlushTimeout+2*time.Second {
		t.Fatalf("Close() waited %v for a kill the server would not answer; it must give up after %v", d, closeFlushTimeout)
	}
	if !k.saw("cmd.kill") {
		t.Fatalf("the kill was not even written before Close gave up (server saw %v)", k.methods)
	}
}

func TestCloseWithNothingPendingDoesNotWait(t *testing.T) {
	_, url, stop := newKillRecorder(t)
	defer stop()
	sb := newTestSandbox(url)
	if err := sb.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	start := time.Now()
	sb.Close()
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Close() with no cancelled command took %v", d)
	}
}

func TestCloseDoesNotWaitForARunThatIsStillGoing(t *testing.T) {
	k, url, stop := newKillRecorder(t)
	defer stop()
	sb := newTestSandbox(url)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = sb.Commands.Run(runCtx, "sleep", CommandOptions{Args: []string{"60"}, OnStdout: func(string) {}})
	}()
	for deadline := time.Now().Add(3 * time.Second); !k.saw("cmd.start"); {
		if time.Now().After(deadline) {
			t.Fatal("the server never saw cmd.start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	sb.Close() // the context is NOT cancelled: nothing is owed, so Close must not wait
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("Close() waited %v for a run that was never cancelled", d)
	}
}

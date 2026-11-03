package solari

import (
	"context"
	"encoding/base64"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// scriptedCommand serves one cmd.start (reply c1), then runs after(conn) so a test can stream frames and answer later calls.
func scriptedCommand(after func(conn *websocket.Conn)) func(conn *websocket.Conn) {
	return func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","ok":true,"result":{"cmdId":"c1"}}`+"\n"))
		after(conn)
	}
}

// Wait used to hand out the exit result once: the result sat in a one-slot channel and the first Wait took it, so a second
// Wait (or a Wait racing another goroutine's) blocked until its context ended.
func TestWaitCanBeCalledMoreThanOnceAndConcurrently(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, scriptedCommand(func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":5}`+"\n"))
		time.Sleep(500 * time.Millisecond)
	}))
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	h, err := sb.Commands.Start(ctx, "true", CommandOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			code, err := h.Wait(wctx)
			if err != nil || code != 5 {
				t.Errorf("Wait = (%d, %v), want (5, nil)", code, err)
			}
		}()
	}
	wg.Wait()
	wctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if code, err := h.Wait(wctx); err != nil || code != 5 {
		t.Errorf("Wait after exit = (%d, %v), want (5, nil)", code, err)
	}
}

// User callbacks ran on the socket's read loop, so a callback that made a call of its own (Stdin, Kill, a file read) waited
// for a reply that only that same loop could deliver, and stalled until the call timed out.
func TestCallbackMayMakeACall(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, scriptedCommand(func(conn *websocket.Conn) {
		b := base64.StdEncoding.EncodeToString([]byte("prompt>"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"`+b+`"}`+"\n"))
		// The callback answers with cmd.stdin; reply to it, then exit.
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"2","ok":true,"result":{"ok":true}}`+"\n"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":0}`+"\n"))
		time.Sleep(500 * time.Millisecond)
	}))
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	var h *CommandHandle
	started := make(chan struct{})
	callErr := make(chan error, 1)
	var once sync.Once
	var err error
	h, err = sb.Commands.Start(ctx, "cat", CommandOptions{OnStdout: func(string) {
		once.Do(func() {
			<-started // the handle is returned before the callback uses it
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			callErr <- h.Stdin(cctx, []byte("yes\n"))
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	close(started)
	select {
	case err := <-callErr:
		if err != nil {
			t.Fatalf("a call made from inside a callback failed: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the callback's call never completed")
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if code, err := h.Wait(wctx); err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v)", code, err)
	}
}

// Output and the exit stay in order for the callback: every chunk is delivered before Wait returns.
func TestCallbacksSeeAllOutputBeforeWaitReturns(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, scriptedCommand(func(conn *websocket.Conn) {
		for _, s := range []string{"a", "b", "c"} {
			b := base64.StdEncoding.EncodeToString([]byte(s))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"`+b+`"}`+"\n"))
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":0}`+"\n"))
		time.Sleep(500 * time.Millisecond)
	}))
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	var mu sync.Mutex
	got := ""
	h, err := sb.Commands.Start(ctx, "x", CommandOptions{OnStdout: func(s string) {
		time.Sleep(20 * time.Millisecond) // a slow consumer
		mu.Lock()
		got += s
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := h.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "abc" {
		t.Fatalf("callbacks saw %q by the time Wait returned, want abc", got)
	}
}

// Concurrent Connect calls each dialed: the last socket won and the others leaked, reader goroutine and all.
func TestConcurrentConnectDialsOnce(t *testing.T) {
	var dials int32
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		atomic.AddInt32(&dials, 1)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	defer sb.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sb.Connect(context.Background()); err != nil {
				t.Errorf("connect: %v", err)
			}
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&dials); n != 1 {
		t.Fatalf("%d sockets were opened for 8 concurrent Connect calls, want 1", n)
	}
}

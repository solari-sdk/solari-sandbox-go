package solari

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- newline-JSON frame splitting -------------------------------------------

// feed drives the framing directly (no socket): two frames packed in one read,
// and one frame split across two reads, must all dispatch correctly and in order.
func TestFeedSplitsFramesPackedAndSplit(t *testing.T) {
	c := newChannel("ws://x", nil, 0)

	var mu sync.Mutex
	var got []string
	c.onFrame("cmd.data", "c1", func(f asyncFrame) {
		mu.Lock()
		b, _ := base64.StdEncoding.DecodeString(f.Base64)
		got = append(got, string(b))
		mu.Unlock()
	})

	b1 := base64.StdEncoding.EncodeToString([]byte("alpha"))
	b2 := base64.StdEncoding.EncodeToString([]byte("beta"))
	f1 := `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + b1 + `"}`
	f2 := `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + b2 + `"}`

	// Two frames in one read (f1 + partial f2)...
	half := len(f2) / 2
	c.feed([]byte(f1 + "\n" + f2[:half]))
	// ...and the rest of f2 in a second read.
	c.feed([]byte(f2[half:] + "\n"))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("expected [alpha beta], got %v", got)
	}
}

// A non-JSON line must be ignored, not crash the dispatcher.
func TestFeedIgnoresNonJSON(t *testing.T) {
	c := newChannel("ws://x", nil, 0)
	got := 0
	c.onFrame("cmd.exit", "c1", func(f asyncFrame) { got++ })
	c.feed([]byte("this is not json\n"))
	c.feed([]byte("   \n"))
	c.feed([]byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":0}` + "\n"))
	if got != 1 {
		t.Fatalf("expected 1 dispatch, got %d", got)
	}
}

// --- orphan async frame buffering -------------------------------------------

// cmd.data / cmd.exit that arrive BEFORE the handler is registered must be
// buffered and flushed, in order, the instant onFrame registers.
func TestOrphanFramesBufferedThenFlushed(t *testing.T) {
	c := newChannel("ws://x", nil, 0)

	b := base64.StdEncoding.EncodeToString([]byte("early"))
	c.feed([]byte(`{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + b + `"}` + "\n"))
	c.feed([]byte(`{"type":"cmd.exit","cmdId":"c1","exitCode":3}` + "\n"))

	var data string
	var exit int
	order := []string{}
	c.onFrame("cmd.data", "c1", func(f asyncFrame) {
		bb, _ := base64.StdEncoding.DecodeString(f.Base64)
		data = string(bb)
		order = append(order, "data")
	})
	c.onFrame("cmd.exit", "c1", func(f asyncFrame) {
		if f.ExitCode != nil {
			exit = *f.ExitCode
		}
		order = append(order, "exit")
	})

	if data != "early" || exit != 3 {
		t.Fatalf("orphan flush wrong: data=%q exit=%d", data, exit)
	}
	if len(order) != 2 || order[0] != "data" || order[1] != "exit" {
		t.Fatalf("orphan flush order wrong: %v", order)
	}
}

// --- mock control-WS server -------------------------------------------------

var testUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// startMockControlServer upgrades /control/* connections and hands the raw conn
// to handle. Returns the ws:// base URL and a closer.
func startMockControlServer(t *testing.T, handle func(conn *websocket.Conn)) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		handle(conn)
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, srv.Close
}

// readOneFrame reads one newline-delimited JSON frame from the socket. gorilla
// preserves message boundaries here (the client writes one framed message), so
// one ReadMessage yields the whole line.
func readOneFrame(conn *websocket.Conn) (string, error) {
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(msg)), nil
}

// --- Commands.Run happy path over the mock WS -------------------------------

func TestCommandsRunOverMockWS(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		// Read the cmd.start request; reply with a cmdId, then stream output +
		// exit. We deliberately pack two frames into one write and split a third
		// across two writes to exercise the client's byte-buffering.
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		out1 := base64.StdEncoding.EncodeToString([]byte("hello "))
		out2 := base64.StdEncoding.EncodeToString([]byte("world"))
		reply := `{"id":"1","ok":true,"result":{"cmdId":"c1"}}`
		data1 := `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + out1 + `"}`
		data2 := `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + out2 + `"}`
		errf := `{"type":"cmd.data","cmdId":"c1","stream":"stderr","base64":"` +
			base64.StdEncoding.EncodeToString([]byte("warn")) + `"}`
		exit := `{"type":"cmd.exit","cmdId":"c1","exitCode":7}`

		// Message A: reply + data1 packed, plus the first half of data2.
		half := len(data2) / 2
		msgA := reply + "\n" + data1 + "\n" + data2[:half]
		// Message B: rest of data2 + stderr + exit.
		msgB := data2[half:] + "\n" + errf + "\n" + exit + "\n"
		_ = conn.WriteMessage(websocket.TextMessage, []byte(msgA))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(msgB))
		time.Sleep(200 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	res, err := sb.Commands.Run(ctx, "echo", CommandOptions{Args: []string{"hello", "world"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code: want 7 got %d", res.ExitCode)
	}
	if res.Stdout != "hello world" {
		t.Errorf("stdout: want %q got %q", "hello world", res.Stdout)
	}
	if res.Stderr != "warn" {
		t.Errorf("stderr: want %q got %q", "warn", res.Stderr)
	}
}

// The guest can emit cmd.data / cmd.exit BEFORE the cmd.start reply (PROTOCOL.md section 2). Those orphan frames
// are flushed when the handler registers; with OnStdout/OnStderr set they must still reach the CommandResult.
func TestCommandsRunKeepsEarlyOutputWithCallbacks(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
		early := `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + b64("early-out\n") + `"}`
		earlyErr := `{"type":"cmd.data","cmdId":"c1","stream":"stderr","base64":"` + b64("early-err\n") + `"}`
		exit := `{"type":"cmd.exit","cmdId":"c1","exitCode":3}`
		reply := `{"id":"1","ok":true,"result":{"cmdId":"c1"}}`
		// One message: output and exit first, the reply last.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(early+"\n"+earlyErr+"\n"+exit+"\n"+reply+"\n"))
		time.Sleep(200 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	var gotOut, gotErr string
	res, err := sb.Commands.Run(ctx, "x", CommandOptions{
		OnStdout: func(s string) { gotOut += s },
		OnStderr: func(s string) { gotErr += s },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 || res.Stdout != "early-out\n" || res.Stderr != "early-err\n" {
		t.Errorf("result lost early output: %+v", res)
	}
	if gotOut != "early-out\n" || gotErr != "early-err\n" {
		t.Errorf("callbacks: out=%q err=%q", gotOut, gotErr)
	}
}

// TimeoutMs is a command deadline on the WebSocket path too: the command is killed and the result is exit -1
// with the output so far (it used to be honoured only by the REST /exec fast path).
func TestCommandsRunTimeoutKillsCommand(t *testing.T) {
	killed := make(chan string, 1)
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		out := base64.StdEncoding.EncodeToString([]byte("partial\n"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(
			`{"id":"1","ok":true,"result":{"cmdId":"c1"}}`+"\n"+
				`{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"`+out+`"}`+"\n"))
		// The next frame must be cmd.kill; the command never exits on its own.
		if frame, err := readOneFrame(conn); err == nil {
			killed <- frame
			var req struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal([]byte(frame), &req)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"`+req.ID+`","ok":true,"result":{"ok":true}}`+"\n"))
		}
		time.Sleep(200 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	start := time.Now()
	res, err := sb.Commands.Run(ctx, "sleep", CommandOptions{Args: []string{"3600"}, TimeoutMs: 80})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("run did not honour TimeoutMs: took %v", time.Since(start))
	}
	if res.ExitCode != -1 || res.Stdout != "partial\n" {
		t.Errorf("want exit -1 with partial output, got %+v", res)
	}
	select {
	case frame := <-killed:
		if !strings.Contains(frame, `"cmd.kill"`) || !strings.Contains(frame, `"signal":9`) {
			t.Errorf("expected cmd.kill with signal 9, got %s", frame)
		}
	case <-time.After(2 * time.Second):
		t.Error("cmd.kill was never sent")
	}
}

// A channel close while a command is awaiting cmd.exit must reject the wait
// (it's frame-based, not a pending RPC) instead of hanging forever.
func TestChannelCloseRejectsInflightCommand(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		// Reply with a cmdId, then close WITHOUT sending cmd.exit.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","ok":true,"result":{"cmdId":"c1"}}`+"\n"))
		time.Sleep(50 * time.Millisecond)
		conn.Close()
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	_, err := sb.Commands.Run(ctx, "sleep", CommandOptions{Args: []string{"100"}})
	if err == nil {
		t.Fatal("expected error from dropped channel, got nil")
	}
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConnectionError, got %T: %v", err, err)
	}
}

// The registration race: onStreamClose called AFTER the channel is already
// torn down must fire its callback synchronously (with a *ConnectionError)
// rather than storing it, or the command's Wait() would block forever.
func TestOnStreamCloseAfterCloseFiresImmediately(t *testing.T) {
	c := newChannel("ws://x", nil, 0)
	c.close()

	var got error
	fired := false
	c.onStreamClose("cmd_late", func(err error) {
		got = err
		fired = true
	})
	if !fired {
		t.Fatal("late onStreamClose on a closed channel did not fire")
	}
	var ce *ConnectionError
	if !errors.As(got, &ce) {
		t.Fatalf("expected *ConnectionError, got %T: %v", got, got)
	}
}

// An RPC ok:false reply must surface as an *ActionError carrying the method.
func TestActionErrorOnFailedReply(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte(`{"id":"1","ok":false,"error":{"code":"EBAD","message":"nope"}}`+"\n"))
		time.Sleep(50 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	_, err := sb.Files.Read(ctx, "/x")
	var ae *ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *ActionError, got %T: %v", err, err)
	}
	if ae.Method != "fs.read" || ae.Code != "EBAD" || ae.Message != "nope" {
		t.Fatalf("ActionError fields wrong: %+v", ae)
	}
}

// newTestSandbox builds a Sandbox whose control channel points at a mock WS,
// with no exec hook (so Commands.Run always takes the WS path).
func newTestSandbox(wsURL string) *Sandbox {
	sb := &Sandbox{
		ID:         "sb",
		ControlURL: wsURL,
		ch:         newChannel(wsURL, http.Header{}, 0),
	}
	sb.Commands = &Commands{sb: sb}
	sb.Files = &Files{sb: sb}
	sb.Code = &Code{sb: sb}
	sb.Git = newGit(sb.runCommand)
	return sb
}

// Frames that arrive before a handler exists (output packed with the cmd.start
// reply) are replayed in order, and a LIVE frame that arrives during the replay
// must not overtake them. Regression for the "worldhello " flake.
func TestOnFrameReplayKeepsOrderAgainstLiveFrames(t *testing.T) {
	c := newChannel("ws://unused", nil, time.Second)
	frame := func(s string) string {
		return `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` +
			base64.StdEncoding.EncodeToString([]byte(s)) + `"}` + "\n"
	}
	c.feed([]byte(frame("a")))

	var mu sync.Mutex
	var got []string
	var calls atomic.Int32
	firstSeen := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.onFrame("cmd.data", "c1", func(f asyncFrame) {
			if calls.Add(1) == 1 {
				close(firstSeen)
				<-release // hold the replay open (before recording) while a live frame arrives
			}
			b, _ := base64.StdEncoding.DecodeString(f.Base64)
			mu.Lock()
			got = append(got, string(b))
			mu.Unlock()
		})
	}()
	<-firstSeen
	c.feed([]byte(frame("b"))) // live frame, handler registration still in progress
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "") != "ab" {
		t.Fatalf("frames out of order or lost: got %q, want %q", strings.Join(got, ""), "ab")
	}
}

// echoServer replies {ok:true,result:{}} to every request on every connection.
func echoServer(t *testing.T) (string, func()) {
	t.Helper()
	return startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			frame, err := readOneFrame(conn)
			if err != nil {
				return
			}
			var req struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal([]byte(frame), &req)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"`+req.ID+`","ok":true,"result":{}}`+"\n"))
		}
	})
}

// close() racing an in-flight call must not nil-dereference c.conn (it used to panic, and trips -race).
func TestChannelCloseRacingInflightCallDoesNotPanic(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		for { // read requests, never reply
			if _, err := readOneFrame(conn); err != nil {
				return
			}
		}
	})
	defer closeSrv()

	for i := 0; i < 40; i++ {
		ch := newChannel(wsURL, nil, 2*time.Second)
		if err := ch.connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		done := make(chan error, 8)
		for j := 0; j < 8; j++ {
			go func() {
				_, err := ch.call(context.Background(), "x", nil, callOptions{})
				done <- err
			}()
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		ch.close()
		for j := 0; j < 8; j++ {
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("call succeeded on a closed channel")
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("call did not return after close()")
			}
		}
	}
}

// The reader of an OLD socket finishing late must not mark the NEW connection disconnected.
func TestStaleReaderCloseDoesNotKillNewConnection(t *testing.T) {
	wsURL, closeSrv := echoServer(t)
	defer closeSrv()
	ctx := context.Background()
	ch := newChannel(wsURL, nil, 2*time.Second)
	if err := ch.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ch.mu.Lock()
	old := ch.conn
	ch.mu.Unlock()
	ch.close()
	if err := ch.connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	ch.onClose(old, errors.New("late close of the old socket"))
	if !ch.isConnected() {
		t.Fatal("a stale reader's close marked the new connection disconnected")
	}
	if _, err := ch.call(ctx, "ping", nil, callOptions{}); err != nil {
		t.Fatalf("call on the new connection failed: %v", err)
	}
	ch.close()
}

// A partial line from a dead connection must not be glued to the next connection's first frame.
func TestPartialLineDoesNotSurviveReconnect(t *testing.T) {
	first := true
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if first {
			first = false
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","ok":tr`)) // no newline, then the socket dies
			conn.Close()
			return
		}
		frame, err := readOneFrame(conn)
		if err != nil {
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(frame), &req)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"`+req.ID+`","ok":true,"result":{"v":1}}`+"\n"))
	})
	defer closeSrv()
	ctx := context.Background()
	ch := newChannel(wsURL, nil, 2*time.Second)
	if err := ch.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for ch.isConnected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := ch.reconnect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	raw, err := ch.call(ctx, "ping", nil, callOptions{})
	if err != nil || !strings.Contains(string(raw), `"v":1`) {
		t.Fatalf("first frame on the new connection was corrupted: raw=%s err=%v", raw, err)
	}
	ch.close()
}

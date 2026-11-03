package solari

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// noReply makes the mock server say nothing for a request (a call that never finishes).
var noReply = &struct{}{}

type rpcReq struct {
	ID     string                 `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}

// rpcServer answers every control-channel call: handle returns (result, errMessage). All requests are recorded in order.
type rpcServer struct {
	mu   sync.Mutex
	reqs []rpcReq
}

func (r *rpcServer) record(q rpcReq) {
	r.mu.Lock()
	r.reqs = append(r.reqs, q)
	r.mu.Unlock()
}

func (r *rpcServer) all() []rpcReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rpcReq(nil), r.reqs...)
}

func (r *rpcServer) methods() []string {
	var m []string
	for _, q := range r.all() {
		m = append(m, q.Method)
	}
	return m
}

func serveRPC(t *testing.T, handle func(q rpcReq, conn *websocket.Conn) (interface{}, string)) (*rpcServer, *Sandbox, func()) {
	t.Helper()
	rec := &rpcServer{}
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var q rpcReq
			if json.Unmarshal(bytes.TrimSpace(msg), &q) != nil {
				continue
			}
			rec.record(q)
			result, errMsg := handle(q, conn)
			if result == noReply {
				continue
			}
			var reply map[string]interface{}
			if errMsg != "" {
				reply = map[string]interface{}{"id": q.ID, "ok": false, "error": errMsg}
			} else {
				reply = map[string]interface{}{"id": q.ID, "ok": true, "result": result}
			}
			b, _ := json.Marshal(reply)
			_ = conn.WriteMessage(websocket.TextMessage, append(b, '\n'))
		}
	})
	sb := newTestSandbox(wsURL)
	if err := sb.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rec, sb, func() { sb.Close(); closeSrv() }
}

func TestUploadSendsFourMiBChunksTruncatingOnlyTheFirst(t *testing.T) {
	var got []byte
	var mu sync.Mutex
	rec, sb, done := serveRPC(t, func(q rpcReq, _ *websocket.Conn) (interface{}, string) {
		b, _ := base64.StdEncoding.DecodeString(q.Params["base64"].(string))
		mu.Lock()
		got = append(got, b...)
		mu.Unlock()
		return map[string]interface{}{"ok": true, "written": len(b)}, ""
	})
	defer done()
	data := bytes.Repeat([]byte("x"), 9<<20) // 4 + 4 + 1 MiB
	if err := sb.Files.Upload(context.Background(), "/f", data); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 3 {
		t.Fatalf("%d calls, want 3", len(reqs))
	}
	for i, want := range []struct {
		offset   float64
		truncate bool
	}{{0, true}, {4 << 20, false}, {8 << 20, false}} {
		q := reqs[i]
		if q.Method != "fs.upload" || q.Params["offset"].(float64) != want.offset || q.Params["truncate"].(bool) != want.truncate {
			t.Errorf("call %d = %s %v, want fs.upload offset=%v truncate=%v", i, q.Method, q.Params["offset"], want.offset, want.truncate)
		}
	}
	if !bytes.Equal(got, data) {
		t.Error("the chunks do not add up to the data")
	}
}

func TestUploadOfAnEmptyFileIsOneZeroLengthCall(t *testing.T) {
	rec, sb, done := serveRPC(t, func(rpcReq, *websocket.Conn) (interface{}, string) { return map[string]interface{}{"ok": true}, "" })
	defer done()
	if err := sb.Files.Upload(context.Background(), "/empty", nil); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].Params["base64"] != "" || reqs[0].Params["offset"].(float64) != 0 || reqs[0].Params["truncate"] != true {
		t.Fatalf("calls = %+v", reqs)
	}
}

func TestReadLoopsOverChunksUntilEOF(t *testing.T) {
	content := bytes.Repeat([]byte("abc"), 3<<20) // 9 MiB
	rec, sb, done := serveRPC(t, func(q rpcReq, _ *websocket.Conn) (interface{}, string) {
		off := int(q.Params["offset"].(float64))
		length := int(q.Params["length"].(float64))
		end := off + length
		if end > len(content) {
			end = len(content)
		}
		return map[string]interface{}{
			"base64": base64.StdEncoding.EncodeToString(content[off:end]), "eof": end == len(content), "size": len(content), "nextOffset": end,
		}, ""
	})
	defer done()
	got, err := sb.Files.Read(context.Background(), "/big")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
	if m := rec.methods(); len(m) != 3 || m[0] != "fs.download" {
		t.Fatalf("methods = %v, want 3 fs.download calls", m)
	}
	for _, q := range rec.all() {
		if q.Params["length"].(float64) != 4<<20 {
			t.Errorf("chunk length = %v, want 4 MiB", q.Params["length"])
		}
	}
}

func TestReadFallsBackToFsReadOnAnOlderGuest(t *testing.T) {
	rec, sb, done := serveRPC(t, func(q rpcReq, _ *websocket.Conn) (interface{}, string) {
		if q.Method == "fs.download" {
			return nil, "unknown method: fs.download"
		}
		return map[string]interface{}{"base64": base64.StdEncoding.EncodeToString([]byte("hi"))}, ""
	})
	defer done()
	got, err := sb.Files.Read(context.Background(), "/f")
	if err != nil || string(got) != "hi" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if m := rec.methods(); len(m) != 2 || m[1] != "fs.read" {
		t.Fatalf("methods = %v", m)
	}
}

func TestWriteRefusesMoreThan32MiBBeforeSending(t *testing.T) {
	rec, sb, done := serveRPC(t, func(rpcReq, *websocket.Conn) (interface{}, string) { return map[string]interface{}{"ok": true}, "" })
	defer done()
	err := sb.Files.Write(context.Background(), "/f", make([]byte, 32<<20+1), 0)
	if err == nil || !strings.Contains(err.Error(), "file too large for a single call (33554433 bytes, max 33554432): use files.upload()/files.download() or uploadUrl()") {
		t.Fatalf("err = %v", err)
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d frames were sent, want none", n)
	}
	if err := sb.Files.Write(context.Background(), "/f", make([]byte, 32<<20), 0); err != nil {
		t.Fatalf("exactly 32 MiB is allowed: %v", err)
	}
}

// Cancelling Commands.Run left the guest process running. It now gets SIGTERM and a closed stdin, and SIGKILL 2 s later.
func TestCancelledRunKillsTheGuestProcess(t *testing.T) {
	rec, sb, done := serveRPC(t, func(q rpcReq, _ *websocket.Conn) (interface{}, string) {
		if q.Method == "cmd.start" {
			return map[string]interface{}{"cmdId": "c1"}, ""
		}
		return map[string]interface{}{"ok": true}, "" // the command never exits
	})
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, err := sb.Commands.Run(ctx, "sleep", CommandOptions{Args: []string{"60"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	time.Sleep(300 * time.Millisecond)
	reqs := rec.all()
	if len(reqs) < 3 || reqs[1].Method != "cmd.kill" || reqs[1].Params["signal"].(float64) != 15 ||
		reqs[2].Method != "cmd.stdin" || reqs[2].Params["eof"] != true || reqs[2].Params["base64"] != "" {
		t.Fatalf("after cancel the guest saw %v, want cmd.kill 15 then cmd.stdin eof", rec.methods())
	}
	if len(reqs) != 3 {
		t.Fatalf("SIGKILL came before the 2 s grace: %v", rec.methods())
	}
	time.Sleep(cancelKillGrace + 300*time.Millisecond)
	reqs = rec.all()
	if last := reqs[len(reqs)-1]; last.Method != "cmd.kill" || last.Params["signal"].(float64) != 9 {
		t.Fatalf("no SIGKILL after the grace: %v", rec.methods())
	}
}

func TestCancelledCodeRunInterruptsTheKernel(t *testing.T) {
	rec, sb, done := serveRPC(t, func(q rpcReq, _ *websocket.Conn) (interface{}, string) {
		if q.Method == "code.run" {
			return noReply, "" // never answers
		}
		if q.Method == "code.interrupt" {
			return nil, "unknown method: code.interrupt" // today's guest: must be ignored
		}
		return map[string]interface{}{"ok": true}, ""
	})
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := sb.Code.Run(ctx, "while True: pass", RunCodeOptions{ContextID: "k1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	m := rec.methods()
	if len(m) < 2 || m[1] != "code.interrupt" {
		t.Fatalf("methods = %v, want code.interrupt after the cancelled code.run", m)
	}
	if rec.all()[1].Params["contextId"] != "k1" {
		t.Errorf("interrupt params = %v", rec.all()[1].Params)
	}
	// Today's guests do not know code.interrupt: that must not tear the channel down or surface anywhere.
	if !sb.Connected() {
		t.Fatal("the channel was torn down by the unknown-method reply")
	}
	if err := sb.Files.Mkdir(context.Background(), "/still-works"); err != nil {
		t.Fatalf("the channel is unusable after a cancelled code.run: %v", err)
	}
}

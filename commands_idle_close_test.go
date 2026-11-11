package solari

import (
	"context"
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
		_ = conn.WriteMessage(websocket.TextMessage, []byte(reply+"\n"+exit+"\n"))
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

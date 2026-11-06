package solari

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// X2: Commands.Run behaves the same over REST (/exec) and the control WebSocket (cmd.start).

// runServer answers every cmd.start with startReply (a full JSON line), or, when it is empty, streams `chunks` for stdout
// and exits 0.
func runServer(t *testing.T, startReply string, chunks ...string) (string, func()) {
	return startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			if _, err := readOneFrame(conn); err != nil {
				return
			}
			if startReply != "" {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(startReply+"\n"))
				continue
			}
			msg := `{"id":"1","ok":true,"result":{"cmdId":"c1"}}` + "\n"
			for _, c := range chunks {
				msg += `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + base64.StdEncoding.EncodeToString([]byte(c)) + `"}` + "\n"
			}
			msg += `{"type":"cmd.exit","cmdId":"c1","exitCode":0}` + "\n"
			_ = conn.WriteMessage(websocket.TextMessage, []byte(msg))
		}
	})
}

// A run that needs the WebSocket (Env here) connects the channel itself, as REST needs no Connect().
func TestRunNeedingTheSocketConnectsItself(t *testing.T) {
	wsURL, closeSrv := runServer(t, "", "hi\n")
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	defer sb.Close()
	r, err := sb.Commands.Run(context.Background(), "echo", CommandOptions{Args: []string{"hi"}, Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatalf("run without a prior Connect: %v", err)
	}
	if r.Stdout != "hi\n" || r.ExitCode != 0 {
		t.Fatalf("result %+v", r)
	}
}

// A command that cannot start is one error class on both paths: *ActionError{Method:"cmd.start", Code:"ExecFailed"}.
func TestRunCannotStartIsTheSameErrorOnBothPaths(t *testing.T) {
	for _, msg := range []string{"fork/exec /nonexistent-bin: no such file or directory", "chdir /no/such/dir: no such file or directory"} {
		// REST: the gateway answers 400 ExecFailed.
		sb := newTestSandbox("ws://127.0.0.1:1")
		sb.execHook = func(context.Context, string, execOneShotRequest) (*CommandResult, error) {
			return nil, &GatewayError{SolariError: SolariError{Message: msg}, Status: 400, Code: "ExecFailed"}
		}
		_, restErr := sb.Commands.Run(context.Background(), "x", CommandOptions{})
		// WS: the guest answers cmd.start with a bare string error.
		wsURL, closeSrv := runServer(t, `{"id":"1","ok":false,"error":"`+msg+`"}`)
		sb2 := newTestSandbox(wsURL)
		_, wsErr := sb2.Commands.Run(context.Background(), "x", CommandOptions{Env: map[string]string{"X": "1"}})
		closeSrv()
		sb2.Close()
		for name, err := range map[string]error{"rest": restErr, "ws": wsErr} {
			var ae *ActionError
			if !errors.As(err, &ae) {
				t.Fatalf("%s (%s): want *ActionError, got %T %v", name, msg, err, err)
			}
			if ae.Method != "cmd.start" || ae.Code != "ExecFailed" || ae.Message != msg {
				t.Errorf("%s (%s): got method=%q code=%q message=%q", name, msg, ae.Method, ae.Code, ae.Message)
			}
		}
	}
}

// Text is decoded over the whole stream: a character split across chunks survives, and each invalid byte becomes U+FFFD
// (what the REST route returns), never raw bytes.
func TestRunTextIsDecodedOverTheWholeStream(t *testing.T) {
	han := "漢" // e6 bc a2
	wsURL, closeSrv := runServer(t, "", "\xff\xfeab", han[:1], han[1:]+"\n")
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	defer sb.Close()
	r, err := sb.Commands.Run(context.Background(), "x", CommandOptions{Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "��ab漢\n"; r.Stdout != want {
		t.Fatalf("stdout %q, want %q", r.Stdout, want)
	}
	if !strings.Contains(r.Stdout, "漢") {
		t.Fatal("multibyte character split across chunks was lost")
	}
}

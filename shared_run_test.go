package solari

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// A shared sandbox refuses cmd.start but runs commands over REST. commands.run on a CONNECTED shared sandbox used to surface the
// refusal (TS and Python fall back to REST /exec). The refusal is also remembered, so later runs go straight to REST.
func sharedRefusingServer(t *testing.T, starts *atomic.Int32) (string, func()) {
	return startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			if _, err := readOneFrame(conn); err != nil {
				return
			}
			starts.Add(1)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","ok":false,"error":{"code":"NotAvailableOnSharedIsolation","message":"cmd.start is not available on a shared context: it operates on the whole VM, not this context"}}`+"\n"))
		}
	})
}

func TestRunOnAConnectedSharedSandboxFallsBackToREST(t *testing.T) {
	var starts atomic.Int32
	wsURL, closeSrv := sharedRefusingServer(t, &starts)
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	var rest atomic.Int32
	sb.execHook = func(_ context.Context, _ string, req execOneShotRequest) (*CommandResult, error) {
		rest.Add(1)
		return &CommandResult{ExitCode: 0, Stdout: "hi from " + req.Cmd}, nil
	}
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	for i := 0; i < 3; i++ {
		r, err := sb.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c", "echo hi"}})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if r.ExitCode != 0 || !strings.Contains(r.Stdout, "hi from sh") {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
	if rest.Load() != 3 {
		t.Fatalf("REST exec used %d times, want 3", rest.Load())
	}
	if starts.Load() != 1 {
		t.Fatalf("cmd.start sent %d times, want 1 (the refusal is remembered)", starts.Load())
	}
}

func TestSharedRefusalOfAStreamingRunIsStillReported(t *testing.T) {
	var starts atomic.Int32
	wsURL, closeSrv := sharedRefusingServer(t, &starts)
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	sb.execHook = func(context.Context, string, execOneShotRequest) (*CommandResult, error) {
		t.Error("a run with an output callback cannot be served by REST")
		return nil, nil
	}
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	_, err := sb.Commands.Run(ctx, "sh", CommandOptions{Args: []string{"-c", "echo hi"}, OnStdout: func(string) {}})
	if err == nil || !isSharedRefusal(err) {
		t.Fatalf("want the shared-isolation refusal, got %v", err)
	}
}

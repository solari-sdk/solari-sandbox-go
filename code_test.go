package solari

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// code.run returns rich results; the client flattens every results[i].chart
// present into the top-level Charts slice.
func TestCodeRunChartFlattening(t *testing.T) {
	result := `{"id":"1","ok":true,"result":{"results":[` +
		`{"type":"stdout","text":"hi"},` +
		`{"type":"result","chart":{"type":"line","title":"A","x":{"label":"t"},"elements":[1,2,3]}},` +
		`{"type":"result","png":"deadbeef"},` +
		`{"type":"result","chart":{"type":"bar","title":"B"}}` +
		`]}}`

	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		if _, err := readOneFrame(conn); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(result+"\n"))
		time.Sleep(100 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	var streamed string
	r, err := sb.Code.Run(ctx, "print('hi')", RunCodeOptions{
		OnStdout: func(s string) { streamed += s },
	})
	if err != nil {
		t.Fatalf("code.run: %v", err)
	}
	if len(r.Results) != 4 {
		t.Fatalf("results len: %d", len(r.Results))
	}
	if len(r.Charts) != 2 {
		t.Fatalf("charts len: want 2 got %d", len(r.Charts))
	}
	if r.Charts[0].Type != ChartLine || r.Charts[0].Title != "A" {
		t.Errorf("chart[0]: %+v", r.Charts[0])
	}
	if r.Charts[0].X == nil || r.Charts[0].X.Label != "t" {
		t.Errorf("chart[0].X: %+v", r.Charts[0].X)
	}
	if len(r.Charts[0].Elements) != 3 {
		t.Errorf("chart[0].Elements: %v", r.Charts[0].Elements)
	}
	if r.Charts[1].Type != ChartBar || r.Charts[1].Title != "B" {
		t.Errorf("chart[1]: %+v", r.Charts[1])
	}
	if streamed != "hi" {
		t.Errorf("streamed stdout: %q", streamed)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// With a callback, code.run asks the guest to stream (stream:true) and the callback fires while the cell runs, not after it:
// the mock sends one frame, then WAITS until the callback has seen it before it sends the rest and the result. A character
// split across two frames arrives whole, and the result's complete text is not delivered a second time.
func TestCodeRunStreamsOutputWhileRunning(t *testing.T) {
	gotFirst := make(chan struct{})
	var sawStreamTrue, liveBeforeRest bool
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		req, err := readOneFrame(conn)
		if err != nil {
			return
		}
		sawStreamTrue = strings.Contains(req, `"stream":true`)
		euro := []byte("€") // 3 bytes, split across two frames
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","stream":"stdout","data":"`+b64("tick1 ")+`"}`+"\n"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","stream":"stdout","data":"`+base64.StdEncoding.EncodeToString(euro[:2])+`"}`+"\n"))
		select { // the rest only after the client's callback saw the first output
		case <-gotFirst:
			liveBeforeRest = true
		case <-time.After(5 * time.Second):
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","stream":"stdout","data":"`+base64.StdEncoding.EncodeToString(euro[2:])+`"}`+"\n"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","stream":"stderr","data":"`+b64("warn\n")+`"}`+"\n"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","ok":true,"result":{"results":[{"type":"stdout","text":"tick1 €"},{"type":"stderr","text":"warn\n"},{"type":"stdout","text":"tail"}]}}`+"\n"))
		time.Sleep(100 * time.Millisecond)
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	var mu sync.Mutex
	var out, errOut strings.Builder
	first := true
	_, err := sb.Code.Run(ctx, "print()", RunCodeOptions{
		OnStdout: func(s string) {
			mu.Lock()
			out.WriteString(s)
			if first {
				first = false
				close(gotFirst)
			}
			mu.Unlock()
		},
		OnStderr: func(s string) { mu.Lock(); errOut.WriteString(s); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("code.run: %v", err)
	}
	if !sawStreamTrue {
		t.Errorf("code.run was not sent with stream:true")
	}
	if !liveBeforeRest {
		t.Errorf("the callback did not fire while the cell was still running")
	}
	if got := out.String(); got != "tick1 €tail" {
		t.Errorf("stdout callbacks = %q, want %q (streamed text once, then only the unflushed tail)", got, "tick1 €tail")
	}
	if got := errOut.String(); got != "warn\n" {
		t.Errorf("stderr callbacks = %q, want %q", got, "warn\n")
	}
}

// helper: a mock that records the code.run request, sends the given frames, then the result
func codeRunMock(t *testing.T, frames []string, result string) (string, func(), *string) {
	var req string
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		r, err := readOneFrame(conn)
		if err != nil {
			return
		}
		req = r
		for _, f := range frames {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(f+"\n"))
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(result+"\n"))
		time.Sleep(100 * time.Millisecond)
	})
	return wsURL, closeSrv, &req
}

func runCodeWith(t *testing.T, wsURL string, o RunCodeOptions) {
	t.Helper()
	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()
	if _, err := sb.Code.Run(ctx, "print()", o); err != nil {
		t.Fatalf("code.run: %v", err)
	}
}

// Only OnStdout is set and the guest streams a stderr frame: it is ignored, not a nil-callback call.
func TestCodeRunStderrFrameWithOnlyOnStdout(t *testing.T) {
	wsURL, closeSrv, _ := codeRunMock(t, []string{
		`{"id":"1","stream":"stderr","data":"` + b64("warn\n") + `"}`,
		`{"id":"1","stream":"stdout","data":"` + b64("out") + `"}`,
	}, `{"id":"1","ok":true,"result":{"results":[{"type":"stdout","text":"out"},{"type":"stderr","text":"warn\n"}]}}`)
	defer closeSrv()
	var out string
	runCodeWith(t, wsURL, RunCodeOptions{OnStdout: func(s string) { out += s }})
	if out != "out" {
		t.Errorf("stdout = %q, want %q", out, "out")
	}
}

// Without callbacks the request does not ask the guest to stream.
func TestCodeRunWithoutCallbacksDoesNotRequestStreaming(t *testing.T) {
	wsURL, closeSrv, req := codeRunMock(t, nil, `{"id":"1","ok":true,"result":{"results":[{"type":"stdout","text":"x"}]}}`)
	defer closeSrv()
	runCodeWith(t, wsURL, RunCodeOptions{})
	if strings.Contains(*req, `"stream"`) {
		t.Errorf("code.run without callbacks carried a stream flag: %s", *req)
	}
}

// A guest that predates streaming sends no frames: the callbacks get the whole result text, exactly once.
func TestCodeRunOldGuestDeliversTheResultTextOnce(t *testing.T) {
	wsURL, closeSrv, _ := codeRunMock(t, nil, `{"id":"1","ok":true,"result":{"results":[{"type":"stdout","text":"a"},{"type":"stdout","text":"b"},{"type":"stderr","text":"e"}]}}`)
	defer closeSrv()
	var out, errOut []string
	runCodeWith(t, wsURL, RunCodeOptions{OnStdout: func(s string) { out = append(out, s) }, OnStderr: func(s string) { errOut = append(errOut, s) }})
	if strings.Join(out, "|") != "ab" || strings.Join(errOut, "|") != "e" {
		t.Errorf("callbacks got stdout=%q stderr=%q, want one delivery each: %q / %q", out, errOut, "ab", "e")
	}
}

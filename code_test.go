package solari

import (
	"context"
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

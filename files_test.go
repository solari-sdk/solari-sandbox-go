package solari

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gorilla/websocket"
)

// Files.List / Files.Stat must parse the guest wire shape exactly: the
// directory flag is "dir" (not "isDir") and the stat timestamp is "modTimeMs".
// This is a regression guard — an earlier draft used "isDir", which silently
// decoded every entry as a file.
func TestFilesListAndStatParseWireShape(t *testing.T) {
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			raw, err := readOneFrame(conn)
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(raw), &req) != nil {
				continue
			}
			var result string
			switch req.Method {
			case "fs.list":
				result = `{"entries":[` +
					`{"name":"a.txt","dir":false,"size":12},` +
					`{"name":"sub","dir":true,"size":4096}]}`
			case "fs.stat":
				result = `{"name":"a.txt","dir":false,"size":12,"mode":420,"modTimeMs":1720000000000}`
			default:
				result = `{}`
			}
			_ = conn.WriteMessage(websocket.TextMessage,
				[]byte(`{"id":"`+req.ID+`","ok":true,"result":`+result+`}`+"\n"))
		}
	})
	defer closeSrv()

	sb := newTestSandbox(wsURL)
	ctx := context.Background()
	if err := sb.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sb.Close()

	entries, err := sb.Files.List(ctx, "/work")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: want 2 got %d", len(entries))
	}
	if entries[0].Name != "a.txt" || entries[0].Dir || entries[0].Size != 12 {
		t.Errorf("entry0: want {a.txt,false,12} got %+v", entries[0])
	}
	if entries[1].Name != "sub" || !entries[1].Dir || entries[1].Size != 4096 {
		t.Errorf("entry1: want {sub,true,4096} got %+v", entries[1])
	}

	st, err := sb.Files.Stat(ctx, "/work/a.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Name != "a.txt" || st.Dir || st.Size != 12 || st.Mode != 420 || st.ModTimeMs != 1720000000000 {
		t.Errorf("stat: unexpected %+v", *st)
	}
}

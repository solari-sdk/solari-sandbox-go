package solari

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Opt-in streamed exec: the one-shot exec sends X-Solari-Stream: 1 and unwraps the gateway's streamed answer, which is HTTP 200 with a run of
// keep-alive newlines first and the real outcome in "httpStatus".
//
// MUTATION CHECKS: drop the header in http.go -> TestExecSendsStreamHeader fails; stop deleting/handling "httpStatus" -> the result and error
// tests fail; ignore httpStatus >= 400 -> TestExecStreamedErrorIsMapped fails.

func execServer(t *testing.T, body string, seen *[]string) *Client {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Header.Get("X-Solari-Stream"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.http.retryDelayMs = 0
	return c
}

func TestExecSendsStreamHeader(t *testing.T) {
	var seen []string
	c := execServer(t, `{"exitCode":0,"stdout":"","stderr":""}`, &seen)
	if _, err := c.exec(context.Background(), "sbx_1", execOneShotRequest{Cmd: "true"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "1" {
		t.Fatalf("X-Solari-Stream = %q, want one request carrying 1", seen)
	}
	// Other calls do not opt in.
	var out map[string]any
	if err := c.http.request(context.Background(), http.MethodGet, "/x", nil, httpRequestOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if seen[1] != "" {
		t.Fatalf("a plain request sent X-Solari-Stream = %q", seen[1])
	}
}

func TestExecStreamedSuccessIsTheNormalResult(t *testing.T) {
	var seen []string
	c := execServer(t, "\n\n\n"+`{"exitCode":3,"stdout":"hi","stderr":"","httpStatus":200}`, &seen)
	res, err := c.exec(context.Background(), "sbx_1", execOneShotRequest{Cmd: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecStreamedErrorIsMapped(t *testing.T) {
	var seen []string
	c := execServer(t, "\n\n"+`{"code":"ExecInterrupted","error":"parked","retryable":false,"httpStatus":409}`, &seen)
	_, err := c.exec(context.Background(), "sbx_1", execOneShotRequest{Cmd: "x"})
	if err == nil {
		t.Fatal("want an error for httpStatus 409")
	}
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != 409 || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("err = %#v (%v), want a 409 gateway error mentioning parked", err, err)
	}
}

func TestExecGatewayThatIgnoresTheHeaderIsReadAsBefore(t *testing.T) {
	var seen []string
	c := execServer(t, `{"exitCode":0,"stdout":"ok","stderr":""}`, &seen)
	res, err := c.exec(context.Background(), "sbx_1", execOneShotRequest{Cmd: "x"})
	if err != nil || res.Stdout != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

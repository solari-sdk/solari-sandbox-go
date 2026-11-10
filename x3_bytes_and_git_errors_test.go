package solari

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

// X3 (retest 2026-11-10): Go could not return binary stdout, and git failures were not *SolariError.

// Over the control socket the result carries the exact bytes beside the lossy text.
func TestRunReturnsExactBinaryBytesOverTheSocket(t *testing.T) {
	bin := "\xff\x00\xfe\x89PNG"
	wsURL, closeSrv := runServer(t, "", bin[:2], bin[2:])
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	defer sb.Close()
	r, err := sb.Commands.Run(context.Background(), "cat", CommandOptions{Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.StdoutBytes, []byte(bin)) {
		t.Fatalf("StdoutBytes %x, want %x", r.StdoutBytes, []byte(bin))
	}
	if r.Stdout == bin {
		t.Fatal("Stdout must stay the UTF-8 text (invalid bytes replaced), not the raw bytes")
	}
	if len(r.StderrBytes) != 0 {
		t.Fatalf("StderrBytes %x, want none", r.StderrBytes)
	}
}

// Over REST /exec the gateway sends the exact bytes as base64 beside the text; without them the bytes of the text.
func TestExecReturnsBase64BytesFromTheGateway(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []byte
	}{
		{"gateway sends stdoutBase64", `{"exitCode":0,"stdout":"��","stderr":"","stdoutBase64":"/wD+"}`, []byte{0xff, 0x00, 0xfe}},
		{"older gateway: the bytes of the text", `{"exitCode":0,"stdout":"héllo","stderr":""}`, []byte("héllo")},
		{"a bad base64 falls back to the text", `{"exitCode":0,"stdout":"ok","stderr":"","stdoutBase64":"!!"}`, []byte("ok")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, _ := NewClient(ClientOptions{APIKey: "slr_test_x", BaseURL: srv.URL})
			r, err := c.exec(context.Background(), "sbx1", execOneShotRequest{Cmd: "cat"})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(r.StdoutBytes, tc.want) {
				t.Fatalf("StdoutBytes %x, want %x", r.StdoutBytes, tc.want)
			}
		})
	}
}

// A failed git command is a *GitError, which is a *SolariError; it carries the subcommand and exit code, and a credential
// stays masked.
func TestGitFailureIsASolariError(t *testing.T) {
	g, _ := stubGit(func(args []string) *CommandResult {
		return &CommandResult{ExitCode: 128, Stderr: "fatal: not a git repository"}
	})
	err := g.Add(context.Background(), []string{"a.txt"}, "/nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *SolariError
	if !errors.As(err, &se) {
		t.Fatalf("git failure %T is not a *SolariError", err)
	}
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("git failure %T is not a *GitError", err)
	}
	if ge.ExitCode != 128 || ge.Subcommand != "add" {
		t.Fatalf("GitError %+v, want subcommand add exit 128", ge)
	}
}

// Input the SDK refuses before running git is a *ValidationError (a *SolariError), like a bad Commands.Run option.
func TestGitInputRefusalsAreValidationErrors(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return nil })
	err := g.Clone(context.Background(), "https://example.com/r.git", GitCloneOptions{Username: "u\nx", Password: "p"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("credential refusal %T is not a *ValidationError", err)
	}
	var se *SolariError
	if !errors.As(err, &se) {
		t.Fatalf("credential refusal %T is not a *SolariError", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused call must not run git: %v", *calls)
	}
}

// The same pairs for stderr: exact bytes from stderrBase64, else the bytes of the text, and a bad base64 falls back.
func TestExecReturnsStderrBytesToo(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []byte
	}{
		{"gateway sends stderrBase64", `{"exitCode":1,"stdout":"","stderr":"\ufffd","stderrBase64":"/w=="}`, []byte{0xff}},
		{"older gateway: the bytes of the text", `{"exitCode":1,"stdout":"","stderr":"oops é"}`, []byte("oops é")},
		{"a bad base64 falls back to the text", `{"exitCode":1,"stdout":"","stderr":"bad","stderrBase64":"!!"}`, []byte("bad")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, _ := NewClient(ClientOptions{APIKey: "slr_test_x", BaseURL: srv.URL})
			r, err := c.exec(context.Background(), "sbx1", execOneShotRequest{Cmd: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(r.StderrBytes, tc.want) {
				t.Fatalf("StderrBytes %x, want %x", r.StderrBytes, tc.want)
			}
		})
	}
}

// Every refusal the SDK makes before running git is a *ValidationError: an option-like ref, remote or branch too.
func TestGitOptionLikeInputIsAValidationError(t *testing.T) {
	g, calls := stubGit(func(args []string) *CommandResult { return nil })
	ctx := context.Background()
	cases := map[string]error{
		"checkout ref": g.Checkout(ctx, "--upload-pack=evil", "/r", false),
		"push remote":  g.Push(ctx, GitRemoteOptions{Cwd: "/r", Remote: "-evil"}),
		"pull branch":  g.Pull(ctx, GitRemoteOptions{Cwd: "/r", Branch: "--force"}),
	}
	for name, err := range cases {
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: %T is not a *ValidationError", name, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused call must not run git: %v", *calls)
	}
}

// Socket stderr: the exact bytes too, with stdout and stderr kept apart.
func TestRunReturnsExactStderrBytesOverTheSocket(t *testing.T) {
	errBytes := "\xfe\x00\xffwarn"
	wsURL, closeSrv := startMockControlServer(t, func(conn *websocket.Conn) {
		for {
			if _, err := readOneFrame(conn); err != nil {
				return
			}
			msg := `{"id":"1","ok":true,"result":{"cmdId":"c1"}}` + "\n"
			msg += `{"type":"cmd.data","cmdId":"c1","stream":"stdout","base64":"` + base64.StdEncoding.EncodeToString([]byte("out")) + `"}` + "\n"
			msg += `{"type":"cmd.data","cmdId":"c1","stream":"stderr","base64":"` + base64.StdEncoding.EncodeToString([]byte(errBytes)) + `"}` + "\n"
			msg += `{"type":"cmd.exit","cmdId":"c1","exitCode":2}` + "\n"
			_ = conn.WriteMessage(websocket.TextMessage, []byte(msg))
		}
	})
	defer closeSrv()
	sb := newTestSandbox(wsURL)
	defer sb.Close()
	r, err := sb.Commands.Run(context.Background(), "x", CommandOptions{Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.StderrBytes, []byte(errBytes)) || !bytes.Equal(r.StdoutBytes, []byte("out")) {
		t.Fatalf("stdout %x stderr %x, want %x / %x", r.StdoutBytes, r.StderrBytes, []byte("out"), []byte(errBytes))
	}
}

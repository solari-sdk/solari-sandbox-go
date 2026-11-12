package solari

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A host that cuts a pause short leaves the sandbox running and the gateway answers a retryable 503 PauseRetryable. POST /pause
// carried no idempotency key, so the transport never retried it and the caller saw the 503.
func TestPauseRetriesPauseRetryableWithOneKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"Sandbox pause did not finish; it is still running","code":"PauseRetryable","retryable":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"paused"}`))
	}))
	defer srv.Close()

	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.http.retryDelayMs = 0
	if err := c.Pause(context.Background(), "sbx_1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("attempts = %d, want 3", len(keys))
	}
	if keys[0] == "" || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Fatalf("want one non-empty Idempotency-Key on every attempt, got %q", keys)
	}
}

// More PauseRetryable answers than the transport's ordinary retry budget (5 retries): a pause takes 35-110 s on a loaded host, so
// it is retried for a 120 s window (as in TypeScript and Python), not ~5 s. Eight 503s then the 200. Without the window the 6th
// 503 reached the caller.
func TestPauseRetriesBeyondTheOrdinaryRetryBudget(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n <= 8 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"Sandbox pause did not finish; it is still running","code":"PauseRetryable","retryable":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"paused"}`))
	}))
	defer srv.Close()

	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.http.retryDelayMs = 0
	if err := c.Pause(context.Background(), "sbx_1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 9 {
		t.Fatalf("attempts = %d, want 9", len(keys))
	}
	for _, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("want one non-empty Idempotency-Key on every attempt, got %q", keys)
		}
	}
}

// A retry window does not make a non-retryable answer retried: one 409 is one request.
func TestPauseWindowDoesNotRetryANonRetryableAnswer(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"Sandbox is stopped","code":"SandboxStopped"}`))
	}))
	defer srv.Close()
	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.http.retryDelayMs = 0
	if err := c.Pause(context.Background(), "sbx_1"); err == nil {
		t.Fatal("want the 409")
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

// The pause window is 120 s, as in TypeScript and Python (PAUSE_RETRY_WINDOW_MS).
func TestPauseRetryWindowIsTwoMinutes(t *testing.T) {
	if pauseRetryWindow != 120*time.Second {
		t.Fatalf("pauseRetryWindow = %v, want 2m0s", pauseRetryWindow)
	}
}

// The window bounds the retries: a pause that keeps answering PauseRetryable is retried while the window is open and then the last
// 503 reaches the caller. Shortened to 300 ms with a 20 ms delay: more attempts than the ordinary budget (6), and it ends near
// the window, not never.
func TestPauseRetryWindowEnds(t *testing.T) {
	saved := pauseRetryWindow
	pauseRetryWindow = 300 * time.Millisecond
	defer func() { pauseRetryWindow = saved }()

	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"Sandbox pause did not finish; it is still running","code":"PauseRetryable","retryable":true}`))
	}))
	defer srv.Close()
	c, err := NewClient(ClientOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.http.retryDelayMs = 20
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a window that never ended would retry until this
	defer cancel()
	start := time.Now()
	err = c.Pause(ctx, "sbx_1")
	took := time.Since(start)
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != 503 || ge.Code != "PauseRetryable" {
		t.Fatalf("want the last 503 PauseRetryable after the window, got %v", err)
	}
	if took < 300*time.Millisecond || took > 3*time.Second {
		t.Fatalf("Pause returned after %v, want just after the 300ms window", took)
	}
	mu.Lock()
	defer mu.Unlock()
	if n <= 6 {
		t.Fatalf("requests = %d, want more than the ordinary budget of 6 inside the window", n)
	}
}

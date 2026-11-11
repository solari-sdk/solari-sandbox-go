package solari

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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

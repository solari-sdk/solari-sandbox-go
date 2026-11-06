package conformance_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	solari "github.com/solari-sdk/solari-sandbox-go"
)

// TestLiveSmoke is the one live row: create, get, kill against a real gateway, then confirm it is gone. Staging only, own id
// only, cleaned up even when an assertion fails. Gated on SOLARI_LIVE=1 (never set in CI), like the repo's other live tests.
//
//	SOLARI_LIVE=1 SOLARI_API_KEY=slr_live_... SOLARI_BASE_URL=https://api-sta.getsolari.com go test ./tests/conformance/ -run TestLiveSmoke -v
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("SOLARI_LIVE") == "" {
		t.Skip("set SOLARI_LIVE=1 (plus SOLARI_API_KEY, SOLARI_BASE_URL on staging) to run the live smoke row")
	}
	base, key := os.Getenv("SOLARI_BASE_URL"), os.Getenv("SOLARI_API_KEY")
	if key == "" || !strings.Contains(base, "-sta") {
		t.Fatalf("the live smoke row runs on staging only: SOLARI_BASE_URL=%q must be an api-sta gateway and SOLARI_API_KEY must be set", base)
	}
	c, err := solari.NewClient(solari.ClientOptions{APIKey: key, BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	s, err := c.Create(ctx, solari.CreateOptions{Template: "base", Metadata: map[string]string{"conformance": "go-live-smoke"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := s.ID
	s.Close()
	killed := false
	defer func() {
		if !killed {
			if err := c.Kill(context.Background(), id); err != nil {
				t.Errorf("cleanup kill of %s failed: %v", id, err)
			}
		}
	}()

	v, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.SandboxID != id || v.State != "running" {
		t.Fatalf("get: want %s running, got %s %q", id, v.SandboxID, v.State)
	}
	if err := c.Kill(ctx, id); err != nil {
		t.Fatalf("kill: %v", err)
	}
	killed = true
	if v, err := c.Get(ctx, id); err == nil && v.State == "running" {
		t.Fatalf("sandbox %s still running after kill", id)
	}
}

package node

import (
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/config"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/throttle"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("network:\n  listen_port: 0\n  upnp: false\n  dht: false\nlocal:\n  lsd: false\n  tailscale: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Node.DataDir = filepath.Join(t.TempDir(), "data")
	c.Node.StateDir = t.TempDir()
	return c
}

func testPolicy(t *testing.T, cidrs []string) *throttle.Policy {
	t.Helper()
	m, err := throttle.NewLocalMatcher(cidrs)
	if err != nil {
		t.Fatal(err)
	}
	return &throttle.Policy{Buckets: throttle.NewBuckets(), Local: m}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// makeFile writes size random bytes to dir/name and returns (path, sha256).
func makeFile(t *testing.T, dir, name string, size int) (string, string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	b := make([]byte, size)
	rand.Read(b)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := hashcache.HashFile(p)
	return p, sum
}

func waitDone[T any](t *testing.T, ch <-chan T, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out after %v waiting for %s", d, what)
	}
}

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/catalog"
)

func TestCheckWant(t *testing.T) {
	c := &catalog.Catalog{Models: []catalog.Model{{ID: "a"}, {ID: "b"}}}
	if err := checkWant([]string{"a", "*"}, c); err != nil {
		t.Fatal(err)
	}
	err := checkWant([]string{"a", "zz"}, c)
	if err == nil || !strings.Contains(err.Error(), "zz") || !strings.Contains(err.Error(), "models.want") {
		t.Fatalf("%v", err)
	}
}

// failListener is a net.Listener whose Accept always fails with a
// non-temporary error, so http.Server.Serve returns that same error
// immediately instead of retrying.
type failListener struct{ err error }

func (l *failListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *failListener) Close() error              { return nil }
func (l *failListener) Addr() net.Addr            { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:0" }

func TestServeAPIFailureCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &http.Server{}

	errCh := serveAPI(srv, &failListener{err: errors.New("boom")}, cancel, log)

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("serveAPI error = %v, want it to contain \"boom\"", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for serveAPI's error")
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop was not called: context was not canceled")
	}
}

func TestServeAPIClosedIsNotAnError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	stopped := false
	errCh := serveAPI(srv, ln, func() { stopped = true }, log)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveAPI error = %v, want nil after a graceful Shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for serveAPI to report shutdown")
	}
	if stopped {
		t.Fatal("stop should not be called on a graceful shutdown")
	}
}

// TestStartLoopsWaitsForExit: the daemon's background loops (catalog
// refresh, discovery) must be fully stopped before the node is closed —
// a refresh finishing after Close would otherwise call SetCatalog on a
// closed node.
func TestStartLoopsWaitsForExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var finished [2]bool
	wait := startLoops(ctx,
		func(ctx context.Context) { <-ctx.Done(); time.Sleep(50 * time.Millisecond); finished[0] = true },
		func(ctx context.Context) { <-ctx.Done(); finished[1] = true },
	)
	cancel()
	wait()
	if !finished[0] || !finished[1] {
		t.Fatalf("wait returned before every loop exited: %v", finished)
	}
}

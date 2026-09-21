package throttle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestLocalMatcher(t *testing.T) {
	m, err := NewLocalMatcher([]string{"10.0.0.0/8", "100.64.0.0/10", "fd7a:115c:a1e0::/48", "fe80::/10"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"10.1.2.3:42069": true, "100.64.0.22:7946": true, "[fd7a:115c:a1e0::1]:1": true,
		"8.8.8.8:53": false, "[::ffff:10.0.0.1]:5": true, "10.0.0.9": true, "garbage": false,
		"[fe80::1%eth0]:42069": true, "fe80::1%eth0": true, // link-local peers carry a zone
	}
	for in, want := range cases {
		if got := m.IsLocalString(in); got != want {
			t.Errorf("IsLocalString(%q) = %v", in, got)
		}
	}
	if !m.IsLocal(&net.TCPAddr{IP: net.ParseIP("10.9.9.9"), Port: 1}) || m.IsLocal(&net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 1}) ||
		!m.IsLocal(&net.TCPAddr{IP: net.ParseIP("fe80::2"), Port: 1, Zone: "eth0"}) {
		t.Fatal("IsLocal net.Addr")
	}
	var nilM *LocalMatcher
	if nilM.IsLocalString("10.0.0.1:1") {
		t.Fatal("nil matcher is never local")
	}
	if _, err := NewLocalMatcher([]string{"nope"}); err == nil {
		t.Fatal("bad cidr")
	}
}

func TestBucketsSet(t *testing.T) {
	b := NewBuckets()
	if b.Up.Limit() != rate.Inf {
		t.Fatal("new buckets are unlimited")
	}
	b.Set(1_000_000, 0)
	if b.Up.Limit() != 1_000_000 || b.Up.Burst() != 250_000 || b.Down.Limit() != rate.Inf {
		t.Fatalf("up %v/%d down %v", b.Up.Limit(), b.Up.Burst(), b.Down.Limit())
	}
	b.Set(1000, 0)
	if b.Up.Burst() != 64<<10 {
		t.Fatalf("burst floor: %d", b.Up.Burst())
	}
	b.Set(0, 0)
	if b.Up.Limit() != rate.Inf {
		t.Fatal("0 = unlimited")
	}
}

func TestWaitNSplitsAboveBurst(t *testing.T) {
	l := rate.NewLimiter(rate.Limit(1<<20), 64<<10)
	start := time.Now()
	if err := waitN(context.Background(), l, 64<<10+256<<10); err != nil { // burst + 256KiB
		t.Fatal(err)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("256KiB over burst at 1MiB/s took %v", d)
	}
}

// pair returns a client conn wrapped by p (dialed) and the raw server side.
func pair(t *testing.T, p *Policy) (net.Conn, net.Conn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	d := &Dialer{Network: "tcp", Policy: p}
	c, err := d.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-acc
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

func TestPublicConnIsThrottled(t *testing.T) {
	b := NewBuckets()
	b.Set(256<<10, 256<<10) // burst 64KiB
	lm, _ := NewLocalMatcher(nil)
	c, s := pair(t, &Policy{Buckets: b, Local: lm})
	if _, ok := c.(*conn); !ok {
		t.Fatal("public conn should be wrapped")
	}
	// Separate send/receive buffers: reusing one slice for both the
	// goroutine's Write and the main goroutine's concurrent ReadFull races
	// on the same memory (Write's chunking loop re-reads from its argument
	// across multiple underlying network writes while ReadFull is
	// simultaneously overwriting it).
	sendPayload := make([]byte, 512<<10)
	recvPayload := make([]byte, 512<<10)
	go func() { c.Write(sendPayload) }()
	start := time.Now()
	io.ReadFull(s, recvPayload)
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("512KiB write at 256KiB/s took %v", d)
	}
	go func() { s.Write(sendPayload) }()
	start = time.Now()
	io.ReadFull(c, recvPayload)
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("512KiB read at 256KiB/s took %v", d)
	}
}

func TestLocalConnIsNotWrapped(t *testing.T) {
	b := NewBuckets()
	b.Set(1024, 1024)
	lm, _ := NewLocalMatcher([]string{"127.0.0.0/8"})
	c, _ := pair(t, &Policy{Buckets: b, Local: lm})
	if _, ok := c.(*conn); ok {
		t.Fatal("local conn must not be wrapped")
	}
	c2, _ := pair(t, &Policy{Buckets: b, Local: lm, ThrottleLocal: true})
	if _, ok := c2.(*conn); !ok {
		t.Fatal("ThrottleLocal wraps local conns too")
	}
}

func TestCloseUnblocksWait(t *testing.T) {
	b := NewBuckets()
	b.Set(1, 1) // effectively stalled
	lm, _ := NewLocalMatcher(nil)
	c, _ := pair(t, &Policy{Buckets: b, Local: lm})
	errc := make(chan error, 1)
	go func() { _, err := c.Write(make([]byte, 1<<20)); errc <- err }()
	time.Sleep(100 * time.Millisecond)
	c.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Write error = %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a throttled Write")
	}
}

func TestWrapBodyThrottles(t *testing.T) {
	l := rate.NewLimiter(rate.Limit(256<<10), 64<<10)
	body := WrapBody(context.Background(), io.NopCloser(bytes.NewReader(make([]byte, 320<<10))), l)
	start := time.Now()
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		t.Fatal(err)
	}
	if n != 320<<10 {
		t.Fatalf("copied %d, want %d", n, 320<<10)
	}
	if d := time.Since(start); d < 800*time.Millisecond || d > 2*time.Second {
		t.Fatalf("320KiB at 256KiB/s took %v", d)
	}
}

func TestWrapBodyUnlimited(t *testing.T) {
	l := rate.NewLimiter(rate.Inf, 0)
	body := WrapBody(context.Background(), io.NopCloser(bytes.NewReader(make([]byte, 1<<20))), l)
	start := time.Now()
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("unlimited body read should not be throttled")
	}
}

// TestWrapBodyContextCancelUnblocksWait is the crux of the Task-16 fix
// round 1 regression: anacrolix's own webseed body rate limiter sleeps out
// its reservation delay with a plain time.Sleep, which no context can
// interrupt. WrapBody must instead return promptly with the context error
// once ctx is cancelled, even mid-wait.
func TestWrapBodyContextCancelUnblocksWait(t *testing.T) {
	l := rate.NewLimiter(1, 1) // one byte's worth of burst, refilling once a second
	ctx, cancel := context.WithCancel(context.Background())
	body := WrapBody(ctx, io.NopCloser(bytes.NewReader(make([]byte, 1<<20))), l)
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		var err error
		for i := 0; i < 10000 && err == nil; i++ {
			_, err = body.Read(buf)
		}
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not unblock a throttled Read")
	}
}

func TestTransportThrottlesAndRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 1<<20))
	}))
	t.Cleanup(srv.Close)

	b := NewBuckets()
	b.Set(0, 256<<10) // 256KiB/s download, burst 64KiB
	tr := &Transport{Buckets: b}
	client := &http.Client{Transport: tr}

	start := time.Now()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1<<20 {
		t.Fatalf("read %d bytes, want %d", n, 1<<20)
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Fatalf("1MiB at 256KiB/s took only %v, expected throttling", d)
	}

	// Now confirm a request context cancellation unblocks a throttled read
	// promptly, instead of sleeping out the reservation delay.
	b2 := NewBuckets()
	b2.Set(0, 1) // effectively stalled
	tr2 := &Transport{Buckets: b2}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp2, err := tr2.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp2.Body)
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not unblock a throttled response body read")
	}
}

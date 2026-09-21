// Package throttle rate-limits peer connections that are not local. Limits
// are applied per connection (not anacrolix's client-wide limiters) so LAN and
// tailnet peers can run at line rate.
package throttle

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"golang.org/x/time/rate"
)

type Buckets struct {
	Up, Down *rate.Limiter

	mu sync.Mutex
}

func NewBuckets() *Buckets {
	return &Buckets{Up: rate.NewLimiter(rate.Inf, 0), Down: rate.NewLimiter(rate.Inf, 0)}
}

// Set changes the rate limits. It is safe to call concurrently with itself
// (e.g. a schedule tick racing a live override) and with connections in use.
func (b *Buckets) Set(up, down int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set(b.Up, up)
	set(b.Down, down)
}

func set(l *rate.Limiter, bps int64) {
	if bps <= 0 {
		l.SetLimit(rate.Inf)
		return
	}
	burst := int(bps / 4)
	if burst < 64<<10 {
		burst = 64 << 10
	}
	if burst > 4<<20 {
		burst = 4 << 20
	}
	// Burst first so a concurrent waiter never sees a finite limit with burst 0.
	l.SetBurst(burst)
	l.SetLimit(rate.Limit(bps))
}

// waitN blocks until n bytes may pass, in burst-sized chunks. A limit or
// burst changed mid-wait is picked up on the next chunk.
func waitN(ctx context.Context, l *rate.Limiter, n int) error {
	for n > 0 {
		if l.Limit() == rate.Inf {
			return nil
		}
		k := min(n, l.Burst())
		if k <= 0 {
			k = n
		}
		if err := l.WaitN(ctx, k); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // burst shrank under us; retry with the new burst
		}
		n -= k
	}
	return nil
}

type LocalMatcher struct {
	prefixes []netip.Prefix
}

func NewLocalMatcher(cidrs []string) (*LocalMatcher, error) {
	m := &LocalMatcher{}
	for _, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("cidr %q: %w", s, err)
		}
		m.prefixes = append(m.prefixes, p.Masked())
	}
	return m, nil
}

func (m *LocalMatcher) isLocalAddr(a netip.Addr) bool {
	if m == nil {
		return false
	}
	// Prefix.Contains never matches a zoned address (link-local peers
	// arrive as fe80::1%eth0), so drop the zone first.
	a = a.Unmap().WithZone("")
	for _, p := range m.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (m *LocalMatcher) IsLocal(addr net.Addr) bool {
	switch v := addr.(type) {
	case *net.TCPAddr:
		return m.isLocalAddr(v.AddrPort().Addr())
	case *net.UDPAddr:
		return m.isLocalAddr(v.AddrPort().Addr())
	case nil:
		return false
	default:
		return m.IsLocalString(addr.String())
	}
}

func (m *LocalMatcher) IsLocalString(hostport string) bool {
	if ap, err := netip.ParseAddrPort(hostport); err == nil {
		return m.isLocalAddr(ap.Addr())
	}
	if a, err := netip.ParseAddr(hostport); err == nil {
		return m.isLocalAddr(a)
	}
	return false
}

type Policy struct {
	Buckets       *Buckets
	Local         *LocalMatcher
	ThrottleLocal bool
}

func (p *Policy) Throttled(addr net.Addr) bool {
	return p.ThrottleLocal || !p.Local.IsLocal(addr)
}

func (p *Policy) WrapConn(c net.Conn) net.Conn {
	if !p.Throttled(c.RemoteAddr()) {
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{Conn: c, b: p.Buckets, ctx: ctx, cancel: cancel}
}

type conn struct {
	net.Conn
	b      *Buckets
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if werr := waitN(c.ctx, c.b.Down, n); werr != nil && err == nil {
			err = c.closedErr("read")
		}
	}
	return n, err
}

func (c *conn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		k := len(p)
		if c.b.Up.Limit() != rate.Inf {
			if b := c.b.Up.Burst(); b > 0 && k > b {
				k = b
			}
		}
		if err := waitN(c.ctx, c.b.Up, k); err != nil {
			return written, c.closedErr("write")
		}
		n, err := c.Conn.Write(p[:k])
		written += n
		if err != nil {
			return written, err
		}
		p = p[k:]
	}
	return written, nil
}

func (c *conn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

// closedErr reports a waitN failure the way a raw net.Conn reports use after
// Close: an *net.OpError wrapping net.ErrClosed. waitN only ever fails here
// because Close cancelled c.ctx (it has no deadline of its own).
func (c *conn) closedErr(op string) error {
	return &net.OpError{Op: op, Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: net.ErrClosed}
}

// Listener wraps accepted peer connections. It satisfies torrent.Listener.
type Listener struct {
	net.Listener
	Policy *Policy
}

func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.Policy.WrapConn(c), nil
}

// Dialer wraps outgoing peer connections. It satisfies torrent.Dialer.
type Dialer struct {
	Network string
	Policy  *Policy
	D       net.Dialer
}

func (d *Dialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c, err := d.D.DialContext(ctx, d.Network, addr)
	if err != nil {
		return nil, err
	}
	return d.Policy.WrapConn(c), nil
}

func (d *Dialer) DialerNetwork() string { return d.Network }

// WrapBody wraps rc so each Read waits on l (via the same context-aware
// waitN used for peer conns) before returning. Unlike anacrolix's own
// webseed response-body rate limiter (an uncancellable time.Sleep on the
// reservation delay), the wait here is bounded by ctx: cancelling ctx (e.g.
// because the request, and so the torrent's webseed peer, was closed)
// unblocks a pending Read promptly instead of sleeping it out, so a
// torrent's Drop() can never be followed by a stray, still-throttled body
// read racing the client's bookkeeping.
func WrapBody(ctx context.Context, rc io.ReadCloser, l *rate.Limiter) io.ReadCloser {
	return &throttledBody{ReadCloser: rc, ctx: ctx, l: l}
}

type throttledBody struct {
	io.ReadCloser
	ctx context.Context
	l   *rate.Limiter
}

func (b *throttledBody) Read(p []byte) (int, error) {
	if b.l.Limit() == rate.Inf {
		return b.ReadCloser.Read(p)
	}
	if burst := b.l.Burst(); burst > 0 && len(p) > burst {
		p = p[:burst]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if werr := waitN(b.ctx, b.l, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// Transport wraps Base (or http.DefaultTransport if nil), throttling
// response bodies through Buckets.Down. It exists so webseed downloads can
// be rate-limited without anacrolix's own WebSeedResponseBodyRateLimiter,
// whose body reader sleeps out its reservation delay uncancellably: that
// sleep can still be in flight after a torrent's Drop() has already
// removed it from the client, racing the client's internal webseed-request
// bookkeeping. Wrapping the body here instead, keyed off the request's own
// context (which anacrolix cancels when the torrent/peer closes), makes a
// pending wait return immediately when that happens.
type Transport struct {
	Base    http.RoundTripper
	Buckets *Buckets
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = WrapBody(req.Context(), resp.Body, t.Buckets.Down)
	return resp, nil
}

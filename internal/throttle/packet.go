package throttle

import (
	"context"
	"net"
)

// WrapPacketConn throttles uTP packets to and from non-local addresses. It is
// installed through anacrolix ClientConfig.ListenPacket.
func WrapPacketConn(pc net.PacketConn, p *Policy) net.PacketConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &packetConn{PacketConn: pc, p: p, ctx: ctx, cancel: cancel}
}

type packetConn struct {
	net.PacketConn
	p      *Policy
	ctx    context.Context
	cancel context.CancelFunc
}

func isUTP(b []byte) bool {
	return len(b) >= 20 && b[0]&0x0f == 1 && b[0]>>4 <= 4
}

func (c *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if n > 0 && isUTP(b[:n]) && c.p.Throttled(addr) {
		// Delaying the read backs the kernel queue up; uTP's congestion
		// control then slows the sender.
		if werr := waitN(c.ctx, c.p.Buckets.Down, n); werr != nil && err == nil {
			err = c.closedErr("read", addr)
		}
	}
	return n, addr, err
}

func (c *packetConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if isUTP(b) && c.p.Throttled(addr) {
		if err := waitN(c.ctx, c.p.Buckets.Up, len(b)); err != nil {
			return 0, c.closedErr("write", addr)
		}
	}
	return c.PacketConn.WriteTo(b, addr)
}

func (c *packetConn) Close() error {
	c.cancel()
	return c.PacketConn.Close()
}

// closedErr reports a waitN failure the way a raw net.PacketConn reports use
// after Close: an *net.OpError wrapping net.ErrClosed. waitN only ever fails
// here because Close cancelled c.ctx (it has no deadline of its own).
func (c *packetConn) closedErr(op string, addr net.Addr) error {
	return &net.OpError{Op: op, Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: addr, Err: net.ErrClosed}
}

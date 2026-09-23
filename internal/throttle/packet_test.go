package throttle

import (
	"errors"
	"net"
	"testing"
	"time"
)

func utpPacket(size int) []byte {
	b := make([]byte, size)
	b[0] = 0x01 // ST_DATA, version 1
	return b
}

func TestIsUTP(t *testing.T) {
	if !isUTP(utpPacket(20)) || !isUTP(append([]byte{0x41}, make([]byte, 19)...)) {
		t.Fatal("uTP data/syn not detected")
	}
	if isUTP([]byte("d1:ad2:id20:aaaaaaaaaaaaaaaaaaaae1:q4:ping1:t2:aa1:y1:qe")) {
		t.Fatal("DHT packet misdetected as uTP")
	}
	if isUTP(utpPacket(19)) || isUTP([]byte{0x51, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("short packet or type 5 is not uTP")
	}
}

func udpPair(t *testing.T, p *Policy) (net.PacketConn, net.PacketConn) {
	a, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := net.ListenPacket("udp", "127.0.0.1:0")
	t.Cleanup(func() { a.Close(); b.Close() })
	return WrapPacketConn(a, p), b
}

func TestPacketConnThrottlesUTPWrites(t *testing.T) {
	bk := NewBuckets()
	bk.Set(256<<10, 0)
	lm, _ := NewLocalMatcher(nil)
	a, b := udpPair(t, &Policy{Buckets: bk, Local: lm})
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := b.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	for i := 0; i < 256; i++ { // 256 * 1400 = 350KiB, burst 64KiB -> ~1.1s
		a.WriteTo(utpPacket(1400), b.LocalAddr())
	}
	if d := time.Since(start); d < 800*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("uTP writes took %v", d)
	}
	start = time.Now()
	for i := 0; i < 256; i++ {
		a.WriteTo([]byte("d1:q4:pinge"), b.LocalAddr())
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("DHT packets must not be throttled")
	}
}

func TestPacketConnCloseUnblocksWait(t *testing.T) {
	bk := NewBuckets()
	bk.Set(1, 1) // effectively stalled
	lm, _ := NewLocalMatcher(nil)
	a, b := udpPair(t, &Policy{Buckets: bk, Local: lm})
	errc := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < 1000; i++ { // well past the 64KiB burst
			if _, err = a.WriteTo(utpPacket(1400), b.LocalAddr()); err != nil {
				break
			}
		}
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	a.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("WriteTo error = %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a throttled WriteTo")
	}
}

func TestPacketConnLocalBypass(t *testing.T) {
	bk := NewBuckets()
	bk.Set(1024, 1024)
	lm, _ := NewLocalMatcher([]string{"127.0.0.0/8"})
	a, b := udpPair(t, &Policy{Buckets: bk, Local: lm})
	start := time.Now()
	for i := 0; i < 100; i++ {
		a.WriteTo(utpPacket(1400), b.LocalAddr())
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("local uTP must not be throttled")
	}
}

// Inbound uTP from a non-local peer over the download limit is dropped
// rather than waited on, so the single reader keeps delivering everything
// else (here: DHT packets queued behind it) without delay.
func TestPacketConnPolicesUTPReads(t *testing.T) {
	bk := NewBuckets()
	bk.Set(0, 1) // download effectively stalled after the 64KiB burst
	lm, _ := NewLocalMatcher(nil)
	a, b := udpPair(t, &Policy{Buckets: bk, Local: lm})
	for i := 0; i < 100; i++ { // 140KB of uTP: well past the burst
		b.WriteTo(utpPacket(1400), a.LocalAddr())
	}
	b.WriteTo([]byte("d1:q4:pinge"), a.LocalAddr())
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	utp, start := 0, time.Now()
	for {
		n, _, err := a.ReadFrom(buf)
		if err != nil {
			t.Fatalf("after %d uTP packets: %v", utp, err)
		}
		if !isUTP(buf[:n]) {
			break
		}
		utp++
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("DHT packet delayed %v behind throttled uTP", d)
	}
	if utp == 0 || utp >= 100 {
		t.Fatalf("%d of 100 uTP packets passed; want the burst's worth, the rest dropped", utp)
	}
}

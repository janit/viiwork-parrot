package node

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

// transfer seeds a size-byte file from a swarm whose policy treats
// seederLocal as local and caps upload at upload bytes/s, and returns how
// long a second swarm takes to download it over loopback.
func transfer(t *testing.T, seederLocal []string, upload int64, size int) time.Duration {
	cfg := testConfig(t)
	src, _ := makeFile(t, t.TempDir(), "m.bin", size)
	r, err := mktorrent.Build(mktorrent.Options{
		Path: src, Name: "m.bin", WebSeed: "http://127.0.0.1:1/m.bin",
		Announce: [][]string{{"http://127.0.0.1:1/announce"}}, // never a real tracker
	})
	if err != nil {
		t.Fatal(err)
	}

	seedPol := testPolicy(t, seederLocal)
	seedPol.Buckets.Set(upload, 0)
	seeder, err := newSwarm(cfg, seedPol, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	st, _ := seeder.cl.AddTorrentOpt(torrent.AddTorrentOpts{
		InfoHash: r.MetaInfo.HashInfoBytes(), InfoBytes: r.MetaInfo.InfoBytes,
		Storage: fileStorage(seeder.pc, filepath.Dir(src), "m.bin"),
	})
	waitDone(t, st.Complete().On(), 30*time.Second, "seeder piece check")

	lcfg := testConfig(t)
	leech, err := newSwarm(lcfg, testPolicy(t, nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer leech.Close()
	lt, _ := leech.cl.AddTorrentOpt(torrent.AddTorrentOpts{
		InfoHash: r.MetaInfo.HashInfoBytes(), InfoBytes: r.MetaInfo.InfoBytes,
		Storage: fileStorage(leech.pc, t.TempDir(), "m.bin"),
	})
	start := time.Now()
	lt.AddPeers([]torrent.PeerInfo{{Addr: torrent.StringAddr(fmt.Sprintf("127.0.0.1:%d", seeder.Port())), Source: torrent.PeerSourceDirect, Trusted: true}})
	lt.DownloadAll()
	waitDone(t, lt.Complete().On(), 60*time.Second, "leecher download")
	return time.Since(start)
}

func TestLocalPeerBypassesUploadLimit(t *testing.T) {
	// 8MiB at 256KiB/s would take 32s if throttled.
	if d := transfer(t, []string{"127.0.0.0/8"}, 256<<10, 8<<20); d > 8*time.Second {
		t.Fatalf("local transfer took %v; should be unthrottled", d)
	}
}

func TestPublicPeerIsUploadLimited(t *testing.T) {
	// 3MiB at 512KiB/s with a 128KiB burst: about 5.75s.
	if d := transfer(t, nil, 512<<10, 3<<20); d < 4*time.Second || d > 15*time.Second {
		t.Fatalf("public transfer took %v; want about 5.75s", d)
	}
}

// TestNewSwarmFixedPortFailsFast: a configured (non-zero) listen_port that's
// already busy must fail immediately, with no retry — only an OS-assigned
// port (listen_port 0) gets retried, since only then is the collision a
// meaningless accident of which free port the kernel handed out.
func TestNewSwarmFixedPortFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := testConfig(t)
	cfg.Network.ListenPort = port
	start := time.Now()
	_, err = newSwarm(cfg, testPolicy(t, nil), quietLogger())
	if err == nil {
		t.Fatal("expected a bind failure on an already-used fixed port")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("fixed-port bind failure took %v; should fail fast, not retry", d)
	}
}

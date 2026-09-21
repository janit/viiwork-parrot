// Package node runs the viiwork-parrot torrent node: adopting, downloading,
// verifying and seeding catalog files under configured limits.
package node

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/janit/viiwork-parrot/internal/config"
	"github.com/janit/viiwork-parrot/internal/throttle"
)

type swarm struct {
	cl *torrent.Client
	ln net.Listener
	pc storage.PieceCompletion
}

func newSwarm(cfg *config.Config, pol *throttle.Policy, log *slog.Logger) (*swarm, error) {
	for _, d := range []string{cfg.Node.StateDir, cfg.Node.DataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	pc, err := storage.NewBoltPieceCompletion(cfg.Node.StateDir)
	if err != nil {
		return nil, fmt.Errorf("piece completion db: %w", err)
	}
	// With an OS-assigned port (listen_port 0), the anacrolix client and our
	// own manual TCP listener each ask the kernel for a free port
	// independently, so nothing guarantees the number the client got for its
	// UDP socket is also free for TCP. Retry the whole client creation a few
	// times on that specific collision. A fixed configured port keeps
	// failing fast: retrying wouldn't help someone who wanted an exact port.
	attempts := 1
	if cfg.Network.ListenPort == 0 {
		attempts = 5
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		sw, err := buildSwarm(cfg, pol, log, pc)
		if err == nil {
			return sw, nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
		log.Warn("tcp listen collided with the OS-picked port; retrying", "attempt", i+1, "err", err)
	}
	pc.Close()
	return nil, lastErr
}

// buildSwarm makes one attempt: create the anacrolix client, then bind our
// own throttled TCP listener on the port it picked. On failure it closes
// the client it created (if any) but leaves pc open, so the caller can
// retry reusing the same piece-completion DB.
func buildSwarm(cfg *config.Config, pol *throttle.Policy, log *slog.Logger, pc storage.PieceCompletion) (*swarm, error) {
	tc := torrent.NewDefaultClientConfig()
	tc.Slogger = log
	tc.DataDir = cfg.Node.DataDir
	// Every torrent gets explicit storage; this default only keeps anacrolix
	// from creating its own completion DB inside data_dir.
	tc.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: cfg.Node.DataDir, PieceCompletion: pc, UsePartFiles: g.Some(false),
	})
	tc.ListenPort = cfg.Network.ListenPort
	tc.DisableTCP = true // replaced by the throttled listener and dialer below
	tc.ListenPacket = func(network, addr string) (net.PacketConn, error) {
		p, err := net.ListenPacket(network, addr)
		if err != nil {
			return nil, err
		}
		return throttle.WrapPacketConn(p, pol), nil
	}
	tc.NoDHT = !cfg.Network.DHT
	tc.DisablePEX = !cfg.Network.PEX
	tc.NoDefaultPortForwarding = !cfg.Network.UPnP
	tc.EstablishedConnsPerTorrent = cfg.Schedule.Base.MaxConnsPerTorrent
	tc.TotalHalfOpenConns = cfg.Schedule.Base.MaxHalfOpen
	tc.Seed = true
	tc.HTTPUserAgent = "viiwork-parrot"
	// Web-seed (and metainfo-source) HTTP bodies are throttled here, not via
	// AddWebSeeds' own WebSeedResponseBodyRateLimiter option: anacrolix's
	// body reader sleeps out its reservation delay with a plain,
	// uncancellable time.Sleep. throttle.Transport waits on the request's
	// own context instead (cancelled synchronously by anacrolix on Drop()),
	// so a cancelled request's body read returns promptly rather than
	// sleeping for up to its reservation delay. This narrows, but does not
	// by itself eliminate, a separate, pre-existing race inside anacrolix
	// itself between Torrent.Drop (which removes the torrent from the
	// client synchronously) and in-flight webseed requests unregistering
	// themselves asynchronously — see the task-16 report's fix-round-1
	// section and internal/node/limits_test.go's
	// TestDropMidDownloadDoesNotPanic for the full analysis.
	tc.WebTransport = &throttle.Transport{
		Base:    httpTransportClone(),
		Buckets: pol.Buckets,
	}
	if cfg.Local.LSD {
		tc.LocalServiceDiscovery = &torrent.LocalServiceDiscoveryConfig{}
	}
	cl, err := torrent.NewClient(tc)
	if err != nil {
		return nil, err
	}
	port := cl.LocalPort()
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("tcp listen on %d: %w", port, err)
	}
	cl.AddListener(&throttle.Listener{Listener: ln, Policy: pol})
	cl.AddDialer(&throttle.Dialer{Network: "tcp", Policy: pol})
	return &swarm{cl: cl, ln: ln, pc: pc}, nil
}

// httpTransportClone gives the webseed client its own *http.Transport
// (rather than sharing http.DefaultTransport's connection pool/state with
// anything else in the process).
func httpTransportClone() *http.Transport {
	return http.DefaultTransport.(*http.Transport).Clone()
}

func (s *swarm) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *swarm) Close() {
	s.cl.Close()
	s.ln.Close()
	// anacrolix closes a DefaultStorage only when it created one itself
	// (cfg.DefaultStorage == nil, client.go); ours is supplied, so nothing
	// else closes the shared piece-completion DB: this is its one Close.
	s.pc.Close()
}

// fileStorage stores a single-file torrent at exactly dir/name, whatever the
// torrent's info.name. Never Close the result: that closes the shared
// piece-completion DB.
func fileStorage(pc storage.PieceCompletion, dir, name string) storage.ClientImpl {
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string { return base },
		FilePathMaker:   func(storage.FilePathMakerOpts) string { return name },
		PieceCompletion: pc,
		UsePartFiles:    g.Some(false),
	})
}

// dirStorage stores a folder model's multi-file torrent under root, each
// file at root/<its path in the torrent> (the HF path), whatever the
// torrent's info.name (the HF revision). Never Close the result: that
// closes the shared piece-completion DB.
func dirStorage(pc storage.PieceCompletion, root string) storage.ClientImpl {
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   root,
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string { return base },
		FilePathMaker: func(o storage.FilePathMakerOpts) string {
			return filepath.Join(o.File.BestPath()...)
		},
		PieceCompletion: pc,
		UsePartFiles:    g.Some(false),
	})
}

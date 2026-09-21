package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/janit/viiwork-parrot/internal/api"
	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/config"
	"github.com/janit/viiwork-parrot/internal/discovery"
	"github.com/janit/viiwork-parrot/internal/node"
	"github.com/janit/viiwork-parrot/internal/throttle"
)

func init() {
	register(command{"daemon", "run the seeding/downloading node", runDaemon})
}

func checkWant(want []string, c *catalog.Catalog) error {
	var unknown, have []string
	for _, w := range want {
		if _, ok := c.Model(w); !ok && w != "*" {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	for _, m := range c.Models {
		have = append(have, m.ID)
	}
	return fmt.Errorf("models.want: unknown model ids %v (catalog has %v)", unknown, have)
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/viiwork-parrot/viiwork-parrot.yaml", "config file")
	debug := fs.Bool("debug", false, "debug logging")
	fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Classic (non-mmap) storage IO: re-execs once if needed, so it comes
	// before anything else starts.
	if err := node.EnsureClassicFileIO(); err != nil {
		log.Warn("could not select classic file IO; a seeded file truncated by its owner can crash the daemon (SIGBUS)", "err", err)
	} else if v := os.Getenv(node.FileIOEnv); v != "classic" {
		log.Warn("mmap file IO selected; a seeded file truncated by its owner can crash the daemon (SIGBUS)", node.FileIOEnv, v)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	pub := cfg.Catalog.PubKey
	if pub == "" {
		pub = catalog.DefaultPubKey
	}
	if pub == "" {
		return errors.New("no catalog public key: set catalog.pubkey or build with `make build` (embeds keys/catalog.pub)")
	}
	fetcher := &catalog.Fetcher{
		URL: cfg.Catalog.URL, PubKey: pub,
		CacheDir: filepath.Join(cfg.Node.StateDir, "catalog"),
		HTTP:     &http.Client{Timeout: 5 * time.Minute},
	}
	matcher, err := throttle.NewLocalMatcher(cfg.Local.CIDRs)
	if err != nil {
		return err
	}
	pol := &throttle.Policy{Buckets: throttle.NewBuckets(), Local: matcher, ThrottleLocal: cfg.Local.Throttle}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cat, err := fetcher.Fetch(ctx)
	if cat == nil {
		return err
	}
	if err != nil {
		log.Warn("catalog", "err", err)
	}
	if err := checkWant(cfg.Models.Want, cat); err != nil {
		return err
	}

	n, err := node.New(node.Options{Config: cfg, Source: fetcher, Policy: pol, Logger: log})
	if err != nil {
		return err
	}
	defer n.Close()
	n.SetCatalog(cat)
	n.Start()

	srv := &http.Server{Addr: cfg.API.Listen, Handler: api.Handler(n), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	serveErr := serveAPI(srv, ln, stop, log)
	log.Info("viiwork-parrot up", "node", cfg.Node.Name, "port", n.Port(), "api", cfg.API.Listen, "models", len(cat.Models), "version", version)

	waitLoops := startLoops(ctx,
		func(ctx context.Context) { refreshLoop(ctx, cfg, fetcher, n, log) },
		func(ctx context.Context) { discoveryLoop(ctx, cfg, n, log) },
	)

	<-ctx.Done()
	log.Info("shutting down")
	// The loops call into n; they must be gone before the deferred n.Close.
	waitLoops()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	select {
	case err := <-serveErr:
		return err
	default:
		return nil
	}
}

// serveAPI runs srv.Serve(ln) in a goroutine and returns a channel carrying
// its outcome: nil on a normal shutdown (http.ErrServerClosed, triggered by
// srv.Shutdown), or the listener/serve error otherwise. On a real failure it
// logs at error level and calls stop, which cancels ctx exactly like a
// SIGINT/SIGTERM would — so a dead API listener doesn't leave the daemon
// silently running with /ensure and /status gone. runDaemon then proceeds
// through its normal shutdown path and returns this error, giving a non-zero
// exit (systemd Restart=on-failure then restarts it).
func serveAPI(srv *http.Server, ln net.Listener, stop context.CancelFunc, log *slog.Logger) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		log.Error("api server", "err", err)
		errCh <- err
		stop()
	}()
	return errCh
}

// startLoops runs each loop in its own goroutine and returns a function that
// blocks until all of them have returned (they return once ctx is done).
func startLoops(ctx context.Context, loops ...func(context.Context)) (wait func()) {
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l(ctx)
		}()
	}
	return wg.Wait
}

func refreshLoop(ctx context.Context, cfg *config.Config, f *catalog.Fetcher, n *node.Node, log *slog.Logger) {
	tk := time.NewTicker(cfg.Catalog.Refresh)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		cat, err := f.Fetch(ctx)
		if err != nil {
			log.Warn("catalog refresh", "err", err)
		}
		if cat == nil {
			continue
		}
		if err := checkWant(cfg.Models.Want, cat); err != nil {
			log.Warn("catalog refresh", "err", err)
		}
		n.SetCatalog(cat)
	}
}

func discoveryLoop(ctx context.Context, cfg *config.Config, n *node.Node, log *slog.Logger) {
	warned := false
	for {
		peers, errs := discovery.ResolveStatic(ctx, net.DefaultResolver, cfg.Local.Peers)
		for _, e := range errs {
			log.Warn("local peer", "err", e)
		}
		if cfg.Local.Tailscale {
			ts, err := discovery.TailscalePeers(ctx, cfg.Network.ListenPort)
			if err != nil && !warned {
				log.Info("tailscale discovery unavailable", "err", err)
				warned = true
			}
			peers = append(peers, ts...)
		}
		n.AddPeers(peers)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

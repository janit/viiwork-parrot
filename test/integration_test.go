//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/api"
	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/config"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
	"github.com/janit/viiwork-parrot/internal/node"
	"github.com/janit/viiwork-parrot/internal/throttle"
)

type world struct {
	t       *testing.T
	hfFiles map[string][]byte // path -> bytes served by fake HF
	hf      *httptest.Server
	catSrv  *httptest.Server
	catFS   map[string][]byte
	pub     string
	models  []catalog.Model
	sources map[string][]byte // model id -> original bytes

	reqMu  sync.Mutex
	hfReqs map[string]int // path -> number of requests the fake HF server received
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, hfFiles: map[string][]byte{}, catFS: map[string][]byte{}, sources: map[string][]byte{}, hfReqs: map[string]int{}}
	// HTTPS: the catalog only accepts https web-seeds (magnet ws).
	w.hf = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.reqMu.Lock()
		w.hfReqs[r.URL.Path]++
		w.reqMu.Unlock()
		b, ok := w.hfFiles[r.URL.Path]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		http.ServeContent(rw, r, "f", time.Time{}, bytes.NewReader(b))
	}))
	w.catSrv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, ok := w.catFS[r.URL.Path]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		rw.Write(b)
	}))
	t.Cleanup(w.hf.Close)
	t.Cleanup(w.catSrv.Close)
	// Nodes clone http.DefaultTransport for their web-seed client; make it
	// trust the fake HF's test certificate.
	dt := http.DefaultTransport.(*http.Transport)
	prevTLS := dt.TLSClientConfig
	dt.TLSClientConfig = w.hf.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { dt.TLSClientConfig = prevTLS })
	// The catalog requires every web-seed to be the model's real HF URL,
	// so all nodes reach the fake HF server as huggingface.co.
	w.dialHF(t)
	return w
}

// add registers a model. serve: "ok" (HF serves it), "none" (404), "tampered".
func (w *world) add(id string, size int, serve string) {
	data := make([]byte, size)
	rand.Read(data)
	src := filepath.Join(w.t.TempDir(), id+".gguf")
	os.WriteFile(src, data, 0o644)
	sum, _ := hashcache.HashFile(src)
	hfPath := "/o/" + id + "/resolve/" + strings.Repeat("a", 40) + "/" + id + ".gguf"
	r, err := mktorrent.Build(mktorrent.Options{
		Path: src, Name: id + ".gguf", WebSeed: "https://huggingface.co" + hfPath,
		Announce: [][]string{{"http://127.0.0.1:1/announce"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	switch serve {
	case "ok":
		w.hfFiles[hfPath] = data
	case "tampered":
		bad := append([]byte(nil), data...)
		for i := 0; i < len(bad); i += 4096 {
			bad[i] ^= 0xff
		}
		w.hfFiles[hfPath] = bad
	}
	var buf bytes.Buffer
	r.MetaInfo.Write(&buf)
	w.catFS["/torrents/"+r.InfoHash+".torrent"] = buf.Bytes()
	w.sources[id] = data
	w.models = append(w.models, catalog.Model{
		ID: id, HFRepo: "o/" + id, Revision: strings.Repeat("a", 40), License: "mit",
		Files: []catalog.File{{Name: id + ".gguf", Size: int64(size), SHA256: sum, InfoHash: r.InfoHash, Magnet: r.Magnet}},
	})
}

// addDir registers a folder model (layout: dir) whose files are served by
// fake HF at their real BEP-19 web-seed paths:
// https://huggingface.co/o/<id>/resolve/<revision>/<path>. The catalog
// requires exactly that web-seed, so a node only reaches the fake server
// through dialHF.
func (w *world) addDir(id string, files map[string]int) string {
	root := w.t.TempDir()
	rev := strings.Repeat("b", 40)
	repo := "o/" + id
	var names []string
	for rel, size := range files {
		data := make([]byte, size)
		rand.Read(data)
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
		w.hfFiles["/"+repo+"/resolve/"+rev+"/"+rel] = data
		names = append(names, rel)
	}
	r, err := mktorrent.BuildDir(mktorrent.DirOptions{
		Root: root, Name: rev, DisplayName: id, Files: names, WebSeed: catalog.DirWebSeed(repo),
		Announce: [][]string{{"http://127.0.0.1:1/announce"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	info, _ := r.MetaInfo.UnmarshalInfo()
	m := catalog.Model{ID: id, HFRepo: repo, Revision: rev, License: "mit", Layout: catalog.LayoutDir, InfoHash: r.InfoHash, Magnet: r.Magnet}
	for _, f := range info.Files {
		rel := strings.Join(f.Path, "/")
		sum, _ := hashcache.HashFile(filepath.Join(root, filepath.FromSlash(rel)))
		m.Files = append(m.Files, catalog.File{Name: rel, Size: f.Length, SHA256: sum})
	}
	var buf bytes.Buffer
	r.MetaInfo.Write(&buf)
	w.catFS["/torrents/"+r.InfoHash+".torrent"] = buf.Bytes()
	w.models = append(w.models, m)
	return root
}

// dialHF points huggingface.co:443 at the fake HF server for nodes created
// during test t (nodes clone http.DefaultTransport when they start); t's
// cleanup restores the transport. The fake server's test
// certificate is valid for example.com, so TLS verifies against that name.
func (w *world) dialHF(t *testing.T) {
	dt := http.DefaultTransport.(*http.Transport)
	prevDial, prevTLS := dt.DialContext, dt.TLSClientConfig
	addr := w.hf.Listener.Addr().String()
	d := &net.Dialer{Timeout: 5 * time.Second}
	dt.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		if a == "huggingface.co:443" {
			a = addr
		}
		return d.DialContext(ctx, network, a)
	}
	tc := prevTLS.Clone()
	tc.ServerName = "example.com"
	dt.TLSClientConfig = tc
	t.Cleanup(func() { dt.DialContext, dt.TLSClientConfig = prevDial, prevTLS })
}

// hfRequests reports how many requests the fake HF server has received for
// path so far.
func (w *world) hfRequests(path string) int {
	w.reqMu.Lock()
	defer w.reqMu.Unlock()
	return w.hfReqs[path]
}

func (w *world) publish() {
	pub, priv, _ := catalog.GenerateKey()
	data, err := catalog.Build(w.models, time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	sig, _ := catalog.Sign(priv, data)
	w.catFS["/catalog.json"], w.catFS["/catalog.json.sig"], w.pub = data, sig, pub
}

type testNode struct {
	n   *node.Node
	cfg *config.Config
	api *api.Client
}

// node builds a node for the given subtest and registers its cleanup
// (the node itself and its httptest API server) on that subtest's *testing.T,
// so it tears down when the subtest finishes rather than at the end of
// TestEndToEnd.
func (w *world) node(t *testing.T, want []string, localCIDRs []string, upload int64, preload []string) *testNode {
	t.Helper()
	cfg, err := config.Parse([]byte("network:\n  listen_port: 0\n  upnp: false\n  dht: false\nlocal:\n  lsd: false\n  tailscale: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Node.StateDir = t.TempDir()
	cfg.Models.Want = want
	cfg.Local.CIDRs = localCIDRs
	cfg.Schedule.Base.Upload = upload
	os.MkdirAll(cfg.Node.DataDir, 0o755)
	for _, id := range preload {
		os.WriteFile(filepath.Join(cfg.Node.DataDir, id+".gguf"), w.sources[id], 0o644)
	}
	m, _ := throttle.NewLocalMatcher(localCIDRs)
	pol := &throttle.Policy{Buckets: throttle.NewBuckets(), Local: m}
	f := &catalog.Fetcher{URL: w.catSrv.URL + "/catalog.json", PubKey: w.pub, CacheDir: filepath.Join(cfg.Node.StateDir, "catalog"), HTTP: w.catSrv.Client()}
	cat, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Options{Config: cfg, Source: f, Policy: pol, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	n.SetCatalog(cat)
	n.Start()
	srv := httptest.NewServer(api.Handler(n))
	t.Cleanup(srv.Close)
	return &testNode{n: n, cfg: cfg, api: api.NewClient(strings.TrimPrefix(srv.URL, "http://"))}
}

func waitSeeding(t *testing.T, tn *testNode, id string, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		resp, code, err := tn.api.Ensure(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if code == http.StatusOK {
			return time.Since(start)
		}
		if code != http.StatusAccepted {
			t.Fatalf("%s: ensure %d %+v", id, code, resp)
		}
		if time.Since(start) > d {
			t.Fatalf("%s: not seeding after %v: %+v", id, d, resp.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestEndToEnd(t *testing.T) {
	w := newWorld(t)
	w.add("ws", 4<<20, "ok")        // reachable only via web-seed
	w.add("p2p", 3<<20, "none")     // reachable only via peers
	w.add("bad", 2<<20, "tampered") // web-seed serves corrupted bytes
	dirFiles := map[string]int{
		"config.json": 311, "model-00001-of-00002.safetensors": 3<<20 + 9,
		"model-00002-of-00002.safetensors": 1<<20 + 1, "inference/sub dir/a b+c.py": 777,
	}
	dirSrc := w.addDir("folder", dirFiles) // a folder model (layout: dir)
	w.publish()

	t.Run("web-seed download via ensure", func(t *testing.T) {
		tn := w.node(t, nil, nil, 0, nil)
		waitSeeding(t, tn, "ws", 60*time.Second)
		got, _ := os.ReadFile(filepath.Join(tn.cfg.Node.DataDir, "ws.gguf"))
		if !bytes.Equal(got, w.sources["ws"]) {
			t.Fatal("web-seed download corrupted")
		}
	})

	t.Run("folder model web-seed download via ensure", func(t *testing.T) {
		tn := w.node(t, nil, nil, 0, nil)
		waitSeeding(t, tn, "folder", 60*time.Second)
		resp, code, err := tn.api.Ensure(context.Background(), "folder")
		want := filepath.Join(tn.cfg.Node.DataDir, "folder")
		if err != nil || code != http.StatusOK || resp.Path != want {
			t.Fatalf("ensure: %d %+v %v (want path %s)", code, resp, err, want)
		}
		for rel := range dirFiles {
			a, _ := os.ReadFile(filepath.Join(dirSrc, filepath.FromSlash(rel)))
			b, err := os.ReadFile(filepath.Join(want, filepath.FromSlash(rel)))
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("%s: %v", rel, err)
			}
		}
		if w.hfRequests("/o/folder/resolve/"+strings.Repeat("b", 40)+"/inference/sub dir/a b+c.py") == 0 {
			t.Fatal("expected the BEP-19 resolve/<revision>/<path> request")
		}
	})

	t.Run("public peer is capped", func(t *testing.T) {
		seed := w.node(t, []string{"p2p"}, nil, 512<<10, []string{"p2p"}) // nobody is local
		waitSeeding(t, seed, "p2p", 30*time.Second)
		leech := w.node(t, nil, nil, 0, nil)
		leech.n.AddPeers([]string{fmt.Sprintf("127.0.0.1:%d", seed.n.Port())})
		d := waitSeeding(t, leech, "p2p", 60*time.Second)
		if d < 4*time.Second || d > 20*time.Second {
			t.Fatalf("3MiB at 512KiB/s took %v", d)
		}
	})

	t.Run("local peer bypasses cap", func(t *testing.T) {
		seed := w.node(t, []string{"p2p"}, []string{"127.0.0.0/8"}, 128<<10, []string{"p2p"}) // 24s if throttled
		waitSeeding(t, seed, "p2p", 30*time.Second)
		leech := w.node(t, nil, nil, 0, nil)
		leech.n.AddPeers([]string{fmt.Sprintf("127.0.0.1:%d", seed.n.Port())})
		if d := waitSeeding(t, leech, "p2p", 60*time.Second); d > 6*time.Second {
			t.Fatalf("local transfer took %v", d)
		}
	})

	t.Run("live override", func(t *testing.T) {
		tn := w.node(t, nil, nil, 1<<20, nil)
		up := "2MiB"
		if err := tn.api.SetOverride(context.Background(), api.OverrideRequest{Upload: &up}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			ls, _ := tn.api.Limits(context.Background())
			if ls.Effective.Upload == 2<<20 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("override not effective within 2s: %+v", ls)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("tampered web-seed never lands", func(t *testing.T) {
		tn := w.node(t, nil, nil, 0, nil)
		resp, code, err := tn.api.Ensure(context.Background(), "bad")
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusAccepted {
			t.Fatalf("ensure bad: got %d, want %d: %+v", code, http.StatusAccepted, resp)
		}
		if resp.Error != "" {
			t.Fatalf("ensure bad: unexpected error %q", resp.Error)
		}

		// Prove a download actually ran before asserting it never lands:
		// wait for the job to report progress (StateDownloading, since a
		// piece-hash mismatch means it never reaches StateVerifying/Seeding)
		// AND for the fake HF server to have actually been asked for the
		// tampered bytes. Either signal alone is too weak: the state flips
		// to StateDownloading immediately on job start, before any bytes
		// are fetched, and a request could in principle be to some other
		// path.
		deadline := time.Now().Add(20 * time.Second)
		for {
			st, ok := tn.n.ModelStatus("bad")
			if !ok {
				t.Fatal("bad: model not found in status")
			}
			if st.State == node.StateSeeding {
				t.Fatal("tampered model reported seeding")
			}
			progressed := st.State == node.StateDownloading || st.State == node.StateVerifying ||
				(len(st.Files) > 0 && st.Files[0].Done > 0)
			if progressed && w.hfRequests("/o/bad/resolve/"+strings.Repeat("a", 40)+"/bad.gguf") > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("bad: no download progress within 20s (state=%s, hf requests=%d)", st.State, w.hfRequests("/o/bad/resolve/"+strings.Repeat("a", 40)+"/bad.gguf"))
			}
			time.Sleep(100 * time.Millisecond)
		}

		// Over the tamper-detection window, the corrupted piece must never
		// land in data_dir and the model must never be reported seeding.
		// Checked repeatedly through the window, not just at the end.
		end := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(tn.cfg.Node.DataDir, "bad.gguf")); err == nil {
				t.Fatal("tampered data reached data_dir")
			}
			st, _ := tn.n.ModelStatus("bad")
			if st.State == node.StateSeeding {
				t.Fatal("tampered model reported seeding")
			}
			if time.Now().After(end) {
				break
			}
			time.Sleep(time.Second)
		}
	})
}

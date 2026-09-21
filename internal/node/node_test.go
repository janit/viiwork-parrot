package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

// fixture fakes HF (web-seed) and the tracker's torrent store.
type fixture struct {
	t       *testing.T
	mu      sync.Mutex
	content map[string]string // url path -> local file serving it
	hfHits  atomic.Int64
	hf      *httptest.Server
	src     fakeSource
	cat     *catalog.Catalog
	gates   map[string]chan struct{} // url path -> closed when fake HF may serve it
}

type fakeSource map[string]*metainfo.MetaInfo

func (f fakeSource) Torrent(_ context.Context, ih string) (*metainfo.MetaInfo, error) {
	if mi, ok := f[ih]; ok {
		return mi, nil
	}
	return nil, fmt.Errorf("no torrent %s", ih)
}

func newFixture(t *testing.T) *fixture {
	fx := &fixture{t: t, content: map[string]string{}, src: fakeSource{}, cat: &catalog.Catalog{Version: 1}, gates: map[string]chan struct{}{}}
	fx.hf = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		p, ok := fx.content[r.URL.Path]
		gate := fx.gates[r.URL.Path]
		fx.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fx.hfHits.Add(1)
		http.ServeFile(w, r, p)
	}))
	t.Cleanup(fx.hf.Close)
	return fx
}

// addModel creates a one-file model of size bytes named disk and returns the
// source file path (served by fake HF) and its sha256.
func (fx *fixture) addModel(id, disk string, size int) (string, string) {
	src, sum := makeFile(fx.t, fx.t.TempDir(), disk, size)
	r, err := mktorrent.Build(mktorrent.Options{
		Path: src, Name: disk, WebSeed: fx.hf.URL + "/" + id + "/" + disk,
		Announce: [][]string{{"http://127.0.0.1:1/announce"}}, // never a real tracker
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.mu.Lock()
	fx.content["/"+id+"/"+disk] = src
	fx.mu.Unlock()
	fx.src[r.InfoHash] = r.MetaInfo
	fx.cat.Models = append(fx.cat.Models, catalog.Model{
		ID: id, HFRepo: "o/" + id, Revision: strings.Repeat("a", 40), License: "mit",
		Files: []catalog.File{{Name: disk, Size: int64(size), SHA256: sum, InfoHash: r.InfoHash, Magnet: r.Magnet}},
	})
	return src, sum
}

func (fx *fixture) node(mut func(*Node)) *Node {
	cfg := testConfig(fx.t)
	n, err := New(Options{Config: cfg, Source: fx.src, Policy: testPolicy(fx.t, cfg.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.t.Cleanup(n.Close)
	if mut != nil {
		mut(n)
	}
	return n
}

func waitState(t *testing.T, n *Node, id string, want State) ModelStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, _ := n.ModelStatus(id)
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("model %s: state %s (err %q), want %s", id, st.State, st.Error, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDownloadFromWebSeed(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 5<<20+17)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	final := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	if st.Path != final || st.Percent != 100 {
		t.Fatalf("%+v", st)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(final)
	if !bytes.Equal(a, b) {
		t.Fatal("downloaded bytes differ")
	}
	if left, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".incoming")); len(left) != 0 {
		t.Fatalf(".incoming not empty: %v", left)
	}
}

func TestAdoptFromDataDir(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	data, _ := os.ReadFile(src)
	os.WriteFile(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf"), data, 0o644)
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)
	if fx.hfHits.Load() != 0 {
		t.Fatal("adopted file must not be downloaded")
	}
}

func TestAdoptSymlinkUnderAnotherName(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	adoptDir := t.TempDir()
	os.MkdirAll(filepath.Join(adoptDir, "Tiny-GGUF"), 0o755)
	real := filepath.Join(adoptDir, "Tiny-GGUF", "Tiny-Q4.gguf")
	data, _ := os.ReadFile(src)
	os.WriteFile(real, data, 0o444) // read-only: we must never write to it
	link := filepath.Join(adoptDir, "short.gguf")
	os.Symlink(real, link)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.Adopt = []string{adoptDir} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	if !strings.HasPrefix(st.Path, adoptDir) {
		t.Fatalf("should seed in place from %s, got %s", adoptDir, st.Path)
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")); err == nil {
		t.Fatal("adoption must not copy into data_dir")
	}
	if fx.hfHits.Load() != 0 {
		t.Fatal("adopted file must not be downloaded")
	}
}

func TestExistingMismatchLeftUntouched(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	junk := bytes.Repeat([]byte{7}, 1<<20)
	p := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	os.WriteFile(p, junk, 0o644)
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateFailed)
	if !strings.Contains(st.Error, "left untouched") {
		t.Fatal(st.Error)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, junk) {
		t.Fatal("mismatching file was modified")
	}
}

func TestSeedOnlyExistingAbsent(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.SeedOnlyExisting = true })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateAbsent)
	if !strings.Contains(st.Error, "seed_only_existing") {
		t.Fatalf("absent model should say why: %q", st.Error)
	}
}

func TestQuarantineOnCatalogHashMismatch(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	fx.cat.Models[0].Files[0].SHA256 = strings.Repeat("0", 64)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateFailed)
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")); err == nil {
		t.Fatal("unverified file must not reach data_dir")
	}
	q, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".quarantine"))
	if len(q) != 1 {
		t.Fatalf("quarantine: %v", q)
	}
}

func TestEnsure(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(nil) // wants nothing
	if _, err := n.Ensure("tiny"); !errors.Is(err, ErrNoCatalog) {
		t.Fatal(err)
	}
	n.SetCatalog(fx.cat)
	if _, err := n.Ensure("nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatal(err)
	}
	if len(n.Status()) != 0 {
		t.Fatal("nothing wanted yet")
	}
	if _, err := n.Ensure("tiny"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, n, "tiny", StateSeeding)
	if st2, err := n.Ensure("tiny"); err != nil || st2.Path != st.Path {
		t.Fatalf("%+v %v", st2, err)
	}
}

func TestNoSpace(t *testing.T) {
	old := diskFree
	diskFree = func(string) (uint64, error) { return 0, nil }
	defer func() { diskFree = old }()
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(nil)
	n.SetCatalog(fx.cat)
	n.Ensure("tiny")
	waitState(t, n, "tiny", StatePaused)
	if _, err := n.Ensure("tiny"); !errors.Is(err, ErrNoSpace) {
		t.Fatal(err)
	}
}

func TestPruneRemovesOnlyDownloads(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("dl", "dl.gguf", 1<<20)
	adoptSrc, _ := fx.addModel("ad", "ad.gguf", 1<<20)
	adoptDir := t.TempDir()
	data, _ := os.ReadFile(adoptSrc)
	adopted := filepath.Join(adoptDir, "mine.gguf")
	os.WriteFile(adopted, data, 0o644)
	n := fx.node(func(n *Node) {
		n.cfg.Models.Want = []string{"*"}
		n.cfg.Models.Prune = true
		n.cfg.Models.Adopt = []string{adoptDir}
	})
	n.SetCatalog(fx.cat)
	waitState(t, n, "dl", StateSeeding)
	waitState(t, n, "ad", StateSeeding)
	n.SetCatalog(&catalog.Catalog{Version: 1})
	dl := filepath.Join(n.cfg.Node.DataDir, "dl.gguf")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(dl); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downloaded file not pruned")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(adopted); err != nil {
		t.Fatal("adopted file must never be pruned")
	}
}

func TestAdoptFromViiworkConfig(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	vwDir := t.TempDir() // not in models.adopt
	data, _ := os.ReadFile(src)
	model := filepath.Join(vwDir, "Tiny.gguf")
	os.WriteFile(model, data, 0o644)
	vwCfg := filepath.Join(t.TempDir(), "viiwork.yaml")
	os.WriteFile(vwCfg, []byte("models:\n  - name: tiny\n    path: "+model+"\n"), 0o644)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.ViiworkConfigs = []string{vwCfg} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	if st.Path != model || fx.hfHits.Load() != 0 {
		t.Fatalf("should seed viiwork's own file in place without downloading: %+v, hf hits %d", st, fx.hfHits.Load())
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")); err == nil {
		t.Fatal("a second copy was stored in data_dir")
	}
}

func TestDuplicatesReportedNotDeleted(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	data, _ := os.ReadFile(src)
	adoptDir := t.TempDir()
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.Adopt = []string{adoptDir} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	primary := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	os.WriteFile(primary, data, 0o644)
	copy2 := filepath.Join(adoptDir, "copy.gguf")
	os.WriteFile(copy2, data, 0o644)                          // a real second copy
	os.Symlink(primary, filepath.Join(adoptDir, "link.gguf")) // same inode: not a duplicate
	os.Link(primary, filepath.Join(adoptDir, "hard.gguf"))    // same inode: not a duplicate
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	if st.Path != primary || len(st.Duplicates) != 1 || st.Duplicates[0] != copy2 {
		t.Fatalf("path %s duplicates %v", st.Path, st.Duplicates)
	}
	if _, err := os.Stat(copy2); err != nil {
		t.Fatal("duplicates must never be deleted")
	}
}

// TestStopThenRestartReachesSeedingAgain exercises rapid catalog churn: the
// job for "tiny" is stopped (unwanted) and immediately replaced (wanted
// again) before the stopped job's goroutine necessarily got a chance to
// finish. The replacement must wait its predecessor out rather than racing
// it for the same incoming/data paths and the same anacrolix torrent.
func TestStopThenRestartReachesSeedingAgain(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)

	n.SetCatalog(&catalog.Catalog{Version: 1}) // unwant it
	n.SetCatalog(fx.cat)                       // and want it again, immediately

	st := waitState(t, n, "tiny", StateSeeding)
	if st.Error != "" {
		t.Fatalf("unexpected error after restart: %s", st.Error)
	}
	final := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	if st.Path != final {
		t.Fatalf("path %s", st.Path)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("file must still be intact: %v", err)
	}
}

// TestAdoptRootIsSymlinkToDir: the adopt root itself (not an entry within
// it) is a symlink to a directory holding the model. It must still be
// walked, not silently skipped (which would fall through to a redundant
// download).
func TestAdoptRootIsSymlinkToDir(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	realDir := t.TempDir()
	data, _ := os.ReadFile(src)
	os.WriteFile(filepath.Join(realDir, "tiny.gguf"), data, 0o644)
	adoptRoot := filepath.Join(t.TempDir(), "link-to-models")
	if err := os.Symlink(realDir, adoptRoot); err != nil {
		t.Fatal(err)
	}
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.Adopt = []string{adoptRoot} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	if fx.hfHits.Load() != 0 {
		t.Fatalf("adopted via symlinked root must not be downloaded; status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")); err == nil {
		t.Fatal("adoption via symlinked root must not copy into data_dir")
	}
}

// TestPrunePersistsAcrossRestart simulates a daemon restart: node A
// downloads a file with prune on and shuts down; node B, sharing the same
// state_dir/data_dir, later sees the model gone from the catalog and must
// still recognize (and prune) the file A downloaded, from the persisted
// record alone. An adopted file is never touched.
func TestPrunePersistsAcrossRestart(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("dl", "dl.gguf", 1<<20)
	adoptSrc, _ := fx.addModel("ad", "ad.gguf", 1<<20)

	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := t.TempDir()
	adoptDir := t.TempDir()
	data, _ := os.ReadFile(adoptSrc)
	adopted := filepath.Join(adoptDir, "mine.gguf")
	os.WriteFile(adopted, data, 0o644)

	cfgA := testConfig(t)
	cfgA.Node.DataDir = dataDir
	cfgA.Node.StateDir = stateDir
	cfgA.Models.Want = []string{"*"}
	cfgA.Models.Prune = true
	cfgA.Models.Adopt = []string{adoptDir}
	nA, err := New(Options{Config: cfgA, Source: fx.src, Policy: testPolicy(t, cfgA.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	nA.SetCatalog(fx.cat)
	waitState(t, nA, "dl", StateSeeding)
	waitState(t, nA, "ad", StateSeeding)
	nA.Close()

	dl := filepath.Join(dataDir, "dl.gguf")
	if _, err := os.Stat(dl); err != nil {
		t.Fatalf("downloaded file missing before restart: %v", err)
	}

	cfgB := testConfig(t)
	cfgB.Node.DataDir = dataDir
	cfgB.Node.StateDir = stateDir
	cfgB.Models.Prune = true
	cfgB.Models.Adopt = []string{adoptDir}
	nB, err := New(Options{Config: cfgB, Source: fx.src, Policy: testPolicy(t, cfgB.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer nB.Close()
	nB.SetCatalog(&catalog.Catalog{Version: 1}) // both models gone from the catalog now

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(dl); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downloaded file not pruned after restart")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(adopted); err != nil {
		t.Fatal("adopted file must never be pruned")
	}
}

// TestQuarantineFailureReported: when quarantining a mismatched download
// itself fails (here, .quarantine can't be created because a plain file
// already occupies that path), the real error must show up in the failed
// status instead of a false "moved to ..." claim.
func TestQuarantineFailureReported(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	fx.cat.Models[0].Files[0].SHA256 = strings.Repeat("0", 64)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	os.WriteFile(filepath.Join(n.cfg.Node.DataDir, ".quarantine"), []byte("x"), 0o644)
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateFailed)
	if !strings.Contains(st.Error, "quarantine failed") {
		t.Fatalf("expected the real quarantine error reported, got %q", st.Error)
	}
}

// TestFindLocalUsesHashDespiteCacheWriteFailure: hashcache.Cache.SHA256 can
// return a valid sum together with a non-nil error when only writing the
// cache to disk failed. findLocal must still use that sum (and just warn)
// instead of treating the whole lookup as failed and re-downloading.
func TestFindLocalUsesHashDespiteCacheWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't apply as root")
	}
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	data, _ := os.ReadFile(src)
	os.WriteFile(filepath.Join(n.cfg.Node.DataDir, "tiny.gguf"), data, 0o644)
	if err := os.Chmod(n.cfg.Node.StateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(n.cfg.Node.StateDir, 0o755) })
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)
	if fx.hfHits.Load() != 0 {
		t.Fatal("a file whose hash is known despite a cache-write failure must not be re-downloaded")
	}
}

// blockingSource wraps a TorrentSource and makes every Torrent() call block
// until release() is called (once, for all calls), regardless of the
// caller's context — standing in for something slow and uninterruptible,
// like the multi-minute HashFile of a huge model file.
type blockingSource struct {
	inner fakeSource
	gate  chan struct{}
}

func newBlockingSource(inner fakeSource) *blockingSource {
	return &blockingSource{inner: inner, gate: make(chan struct{})}
}

func (b *blockingSource) release() { close(b.gate) }

func (b *blockingSource) Torrent(ctx context.Context, ih string) (*metainfo.MetaInfo, error) {
	<-b.gate
	return b.inner.Torrent(ctx, ih)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestChurnWaitsOutBlockedPredecessor covers double churn on a job that is
// stuck (not merely cancelled) in a slow, uninterruptible step: A is
// downloading and blocked fetching its torrent; it's stopped and replaced
// by B, which is itself stopped and replaced by C before A ever unblocks.
// Every one of A/B/C must still fully serialize: B (and transitively C)
// must not run their own file-touching cleanup, and C must not start real
// work, until A actually finishes — even though A's cancellation happened
// long before A notices it.
func TestChurnWaitsOutBlockedPredecessor(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	ih := fx.cat.Models[0].Files[0].InfoHash
	blk := newBlockingSource(fx.src)

	cfg := testConfig(t)
	cfg.Models.Want = []string{"tiny"}
	cfg.Models.Prune = true
	n, err := New(Options{Config: cfg, Source: blk, Policy: testPolicy(t, cfg.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)

	n.SetCatalog(fx.cat) // creates A
	n.mu.Lock()
	a := n.jobs[ih]
	n.mu.Unlock()
	if a == nil {
		t.Fatal("job A not created")
	}

	// Wait until A is inside download() (past findLocal, past MkdirAll),
	// blocked in its (fake) torrent fetch.
	incomingDir := filepath.Join(cfg.Node.DataDir, ".incoming")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(incomingDir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for A to start downloading")
		}
		time.Sleep(10 * time.Millisecond)
	}
	incoming := filepath.Join(incomingDir, "tiny.gguf")
	if err := os.WriteFile(incoming, []byte("partial-bytes-from-A"), 0o644); err != nil {
		t.Fatal(err) // stands in for bytes anacrolix would have written for A
	}

	n.SetCatalog(&catalog.Catalog{Version: 1}) // churn: stop A
	n.SetCatalog(fx.cat)                       // churn: create B (waits on A)
	n.mu.Lock()
	b := n.jobs[ih]
	n.mu.Unlock()
	if b == nil || b == a {
		t.Fatal("job B not created")
	}

	n.SetCatalog(&catalog.Catalog{Version: 1}) // churn: stop B
	n.SetCatalog(fx.cat)                       // churn: create C (waits on B, transitively A)
	n.mu.Lock()
	c := n.jobs[ih]
	n.mu.Unlock()
	if c == nil || c == b {
		t.Fatal("job C not created")
	}

	// Give the goroutines time to settle into their blocked-on-predecessor
	// states before asserting nothing has happened yet.
	time.Sleep(200 * time.Millisecond)

	if isClosed(a.done) {
		t.Fatal("A should still be stuck in its slow torrent fetch")
	}
	if isClosed(b.done) {
		t.Fatal("B (waiting on A) closed done before A finished")
	}
	if isClosed(c.done) {
		t.Fatal("C (waiting on B) closed done before starting")
	}
	if data, err := os.ReadFile(incoming); err != nil || string(data) != "partial-bytes-from-A" {
		t.Fatalf("A's in-progress file was disturbed before A finished: %v %q", err, data)
	}

	blk.release() // let A (and, later, C) proceed

	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("A never finished after being released")
	}
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		t.Fatal("B never finished after A did")
	}

	st := waitState(t, n, "tiny", StateSeeding)
	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}
}

// TestPruneLeavesFileUserReplaced: the in-process prune path in
// job.cleanup() must not delete data_dir/<disk> just because this job
// downloaded it — only if it's still the same file (size+mtime) it left
// there. A user who dropped in their own file of the same name keeps it.
func TestPruneLeavesFileUserReplaced(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("dl", "dl.gguf", 1<<20)
	ih := fx.cat.Models[0].Files[0].InfoHash
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"}; n.cfg.Models.Prune = true })
	n.SetCatalog(fx.cat)
	waitState(t, n, "dl", StateSeeding)

	dl := filepath.Join(n.cfg.Node.DataDir, "dl.gguf")
	replaced := bytes.Repeat([]byte{9}, 2<<20) // different size than the original 1<<20
	if err := os.WriteFile(dl, replaced, 0o644); err != nil {
		t.Fatal(err)
	}

	n.mu.Lock()
	j := n.jobs[ih]
	n.mu.Unlock()
	if j == nil {
		t.Fatal("job not found")
	}

	n.SetCatalog(&catalog.Catalog{Version: 1}) // model removed: job stopped with prune
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not finish stopping in time")
	}

	got, err := os.ReadFile(dl)
	if err != nil {
		t.Fatalf("replaced file must be left in place: %v", err)
	}
	if !bytes.Equal(got, replaced) {
		t.Fatal("replaced file content must not be touched")
	}
}

// TestPrunePersistsAcrossRestartLeavesReplacedFile: same as above but for
// the persisted (restart-surviving) prune path in pruneRecordedLocked.
func TestPrunePersistsAcrossRestartLeavesReplacedFile(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("dl", "dl.gguf", 1<<20)

	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := t.TempDir()

	cfgA := testConfig(t)
	cfgA.Node.DataDir = dataDir
	cfgA.Node.StateDir = stateDir
	cfgA.Models.Want = []string{"*"}
	cfgA.Models.Prune = true
	nA, err := New(Options{Config: cfgA, Source: fx.src, Policy: testPolicy(t, cfgA.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	nA.SetCatalog(fx.cat)
	waitState(t, nA, "dl", StateSeeding)
	nA.Close()

	dl := filepath.Join(dataDir, "dl.gguf")
	replaced := bytes.Repeat([]byte{9}, 2<<20)
	if err := os.WriteFile(dl, replaced, 0o644); err != nil {
		t.Fatal(err)
	}

	cfgB := testConfig(t)
	cfgB.Node.DataDir = dataDir
	cfgB.Node.StateDir = stateDir
	cfgB.Models.Prune = true
	nB, err := New(Options{Config: cfgB, Source: fx.src, Policy: testPolicy(t, cfgB.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer nB.Close()
	nB.SetCatalog(&catalog.Catalog{Version: 1}) // synchronous: pruneRecordedLocked already ran by the time this returns

	got, err := os.ReadFile(dl)
	if err != nil {
		t.Fatalf("replaced file must be left in place: %v", err)
	}
	if !bytes.Equal(got, replaced) {
		t.Fatal("replaced file content must not be touched")
	}
}

// TestAddPeersNotTrusted: discovered local peers (tailscale, static) are
// not marked Trusted, so anacrolix's bad-piece banning still applies to
// them.
func TestAddPeersNotTrusted(t *testing.T) {
	fx := newFixture(t)
	n := fx.node(nil)
	n.AddPeers([]string{"100.64.0.5:42069", "192.168.1.9:42069"})
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.peers) != 2 {
		t.Fatalf("%v", n.peers)
	}
	for _, p := range n.peers {
		if p.Trusted {
			t.Errorf("peer %v marked trusted", p.Addr)
		}
	}
}

// TestSetCatalogAfterCloseIsNoop: a late SetCatalog/Ensure (e.g. a catalog
// refresh finishing during shutdown) must not start jobs on a closed node.
func TestSetCatalogAfterCloseIsNoop(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.Close()
	n.SetCatalog(fx.cat)
	n.Ensure("tiny")
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.jobs) != 0 {
		t.Fatalf("jobs started after Close: %d", len(n.jobs))
	}
}

// TestNetworkSourcesComeFromSignedMagnet: the .torrent's own announce and
// url-list are not covered by the catalog signature (only its info dict
// is), so a tampered torrent store must not be able to point nodes at other
// web-seeds or trackers. The web-seed used is the signed magnet's ws.
func TestNetworkSourcesComeFromSignedMagnet(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	var evilHits atomic.Int64
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(evil.Close)
	ih := fx.cat.Models[0].Files[0].InfoHash
	mi := *fx.src[ih]
	mi.UrlList = metainfo.UrlList{evil.URL + "/tiny.gguf"}
	mi.Announce = evil.URL + "/announce"
	mi.AnnounceList = [][]string{{evil.URL + "/announce"}}
	fx.src[ih] = &mi

	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateSeeding)
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(st.Path)
	if !bytes.Equal(a, b) {
		t.Fatal("downloaded bytes differ")
	}
	if evilHits.Load() != 0 {
		t.Fatalf("the .torrent's unsigned url-list/announce was used: %d requests", evilHits.Load())
	}
	n.mu.Lock()
	j := n.jobs[ih]
	n.mu.Unlock()
	got := j.torrent().Metainfo()
	for _, tier := range got.UpvertedAnnounceList() {
		for _, tr := range tier {
			if strings.HasPrefix(tr, evil.URL) {
				t.Fatalf("unsigned tracker %s added", tr)
			}
		}
	}
}

// TestDanglingSymlinkAtDataPathLeftUntouched: a symlink at data_dir/<disk>
// whose target doesn't exist is not "absent": it's someone's entry, and
// the download must not replace it (nor create its target).
func TestDanglingSymlinkAtDataPathLeftUntouched(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	target := filepath.Join(t.TempDir(), "gone", "tiny.gguf")
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	if err := os.Symlink(target, dp); err != nil {
		t.Fatal(err)
	}
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny", StateFailed)
	if !strings.Contains(st.Error, "left untouched") {
		t.Fatalf("error %q", st.Error)
	}
	if got, err := os.Readlink(dp); err != nil || got != target {
		t.Fatalf("symlink replaced: %q %v", got, err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("symlink target was created")
	}
	if fx.hfHits.Load() != 0 {
		t.Fatal("must not download over an occupied data path")
	}
}

// TestSymlinkAtDataPathToVerifiedFileIsSeeded: a symlink at
// data_dir/<disk> pointing at a verified copy is adopted, not replaced.
func TestSymlinkAtDataPathToVerifiedFileIsSeeded(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	real := filepath.Join(t.TempDir(), "Real.gguf")
	data, _ := os.ReadFile(src)
	os.WriteFile(real, data, 0o444)
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	os.Symlink(real, dp)
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)
	if got, err := os.Readlink(dp); err != nil || got != real {
		t.Fatalf("symlink replaced: %q %v", got, err)
	}
	if fx.hfHits.Load() != 0 {
		t.Fatal("verified symlink target must not be downloaded")
	}
}

// TestFileAppearingDuringDownloadNotOverwritten: a file the user puts at
// data_dir/<disk> while viiwork-parrot is downloading is never replaced by the
// promote; the job fails "left untouched" and keeps its verified download
// in .incoming.
func TestFileAppearingDuringDownloadNotOverwritten(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	blk := newBlockingSource(fx.src)
	cfg := testConfig(t)
	cfg.Models.Want = []string{"tiny"}
	n, err := New(Options{Config: cfg, Source: blk, Policy: testPolicy(t, cfg.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	n.SetCatalog(fx.cat)
	incomingDir := filepath.Join(cfg.Node.DataDir, ".incoming")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(incomingDir); err == nil {
			break // past findLocal, blocked fetching the torrent
		}
		if time.Now().After(deadline) {
			t.Fatal("download never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dp := filepath.Join(cfg.Node.DataDir, "tiny.gguf")
	mine := []byte("the user's own file")
	if err := os.WriteFile(dp, mine, 0o644); err != nil {
		t.Fatal(err)
	}
	blk.release()
	st := waitState(t, n, "tiny", StateFailed)
	if !strings.Contains(st.Error, "left untouched") {
		t.Fatalf("error %q", st.Error)
	}
	if got, _ := os.ReadFile(dp); !bytes.Equal(got, mine) {
		t.Fatal("user's file was overwritten")
	}
	if _, err := os.Stat(filepath.Join(incomingDir, "tiny.gguf")); err != nil {
		t.Fatalf("verified download should stay in .incoming: %v", err)
	}
}

// TestParallelDownloadsReserveDiskSpace: two downloads that each fit in the
// free space alone, but not together, must not both start (sparse files
// would then hit ENOSPC mid-download). The second waits, paused with
// no_space, until the first has finished, then proceeds on its own.
func TestParallelDownloadsReserveDiskSpace(t *testing.T) {
	const size = 1 << 20
	old := diskFree
	diskFree = func(string) (uint64, error) { return spaceMargin + size + size/2, nil }
	defer func() { diskFree = old }()
	fx := newFixture(t)
	fx.addModel("a", "a.gguf", size)
	fx.addModel("b", "b.gguf", size)
	gate := make(chan struct{})
	fx.mu.Lock()
	fx.gates["/a/a.gguf"], fx.gates["/b/b.gguf"] = gate, gate
	fx.mu.Unlock()
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"} })
	n.SetCatalog(fx.cat)

	var first, second string
	deadline := time.Now().Add(10 * time.Second)
	for first == "" {
		a, _ := n.ModelStatus("a")
		b, _ := n.ModelStatus("b")
		switch {
		case a.State == StateDownloading && b.State == StatePaused && b.NoSpace:
			first, second = "a", "b"
		case b.State == StateDownloading && a.State == StatePaused && a.NoSpace:
			first, second = "b", "a"
		case a.State == StateDownloading && b.State == StateDownloading:
			t.Fatal("both downloads started although only one fits")
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected one downloading and one paused/no_space: a=%+v b=%+v", a, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(gate)
	for {
		s2, _ := n.ModelStatus(second) // read second first: if it left paused, first is already done
		s1, _ := n.ModelStatus(first)
		if s2.State != StatePaused && s1.State != StateVerifying && s1.State != StateSeeding {
			t.Fatalf("%s left paused (%s) while %s was still %s", second, s2.State, first, s1.State)
		}
		if s1.State == StateSeeding && s2.State == StateSeeding {
			break
		}
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatalf("not both seeding: %s=%s %s=%s (%q)", first, s1.State, second, s2.State, s2.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRevisionBumpReplacesOwnDownload: the catalog replaces a model's file
// with a new revision (new infohash, same disk name). The old file at
// data_dir/<disk> is viiwork-parrot's own unchanged download of an infohash no
// longer in the catalog, so the new revision replaces it — without a
// restart, with prune off or on.
func TestRevisionBumpReplacesOwnDownload(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprintf("prune=%v", prune), func(t *testing.T) {
			fx := newFixture(t)
			srcA, _ := fx.addModel("m", "m.gguf", 1<<20)
			sizeB := 1 << 20
			if prune {
				sizeB += 4096 // exercise the size-mismatch path too
			}
			srcB, _ := fx.addModel("m", "m.gguf", sizeB) // same id, disk name and URL
			catA := &catalog.Catalog{Version: 1, Models: fx.cat.Models[:1]}
			catB := &catalog.Catalog{Version: 1, Models: fx.cat.Models[1:]}
			ihA, ihB := catA.Models[0].Files[0].InfoHash, catB.Models[0].Files[0].InfoHash
			setServed := func(p string) {
				fx.mu.Lock()
				fx.content["/m/m.gguf"] = p
				fx.mu.Unlock()
			}

			setServed(srcA)
			n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"m"}; n.cfg.Models.Prune = prune })
			n.SetCatalog(catA)
			st := waitState(t, n, "m", StateSeeding)
			dp := filepath.Join(n.cfg.Node.DataDir, "m.gguf")
			if st.Path != dp {
				t.Fatalf("rev A path %s", st.Path)
			}

			setServed(srcB)
			n.SetCatalog(catB)
			deadline := time.Now().Add(60 * time.Second)
			for {
				st, _ = n.ModelStatus("m")
				if st.State == StateSeeding && st.Files[0].Size == int64(sizeB) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("rev B never seeding: %+v", st)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if st.Path != dp {
				t.Fatalf("rev B path %s, want %s", st.Path, dp)
			}
			want, _ := os.ReadFile(srcB)
			if got, _ := os.ReadFile(dp); !bytes.Equal(got, want) {
				t.Fatal("data path does not hold rev B")
			}
			if _, ok := n.dl.Get(ihA); ok {
				t.Error("rev A still recorded as downloaded")
			}
			if rec, ok := n.dl.Get(ihB); !ok || rec.Path != dp {
				t.Errorf("rev B not recorded: %+v %v", rec, ok)
			}
		})
	}
}

// TestRetryFailedOnSetCatalog: a job that failed is restarted by the next
// SetCatalog (catalog refresh) while its file is still wanted, so a cleared
// obstacle doesn't need a daemon restart.
func TestRetryFailedOnSetCatalog(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 1<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	os.MkdirAll(n.cfg.Node.DataDir, 0o755)
	p := filepath.Join(n.cfg.Node.DataDir, "tiny.gguf")
	os.WriteFile(p, []byte("in the way"), 0o644)
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateFailed)
	os.Remove(p) // the owner clears it
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)
}

package node

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

// dirFiles is a small safetensors-style repo: nested paths, a name that
// needs URL escaping, several pieces' worth of shards, and an empty file.
var dirFiles = map[string]int{
	"config.json":                      217,
	"model-00001-of-00002.safetensors": 3<<20 + 11,
	"model-00002-of-00002.safetensors": 1<<20 + 5,
	"inference/kernel.py":              4000,
	"inference/sub dir/a b+c.json":     99,
	"empty.txt":                        0,
}

// addDirModel creates a folder model with files (relative path -> size),
// served by fake HF at /<id>/<revision>/<path> (BEP-19: web-seed base +
// info.name + "/" + path), and returns the source directory.
func (fx *fixture) addDirModel(id, rev string, files map[string]int) string {
	fx.t.Helper()
	root := fx.t.TempDir()
	var names []string
	for rel, size := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, size)
		rand.Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fx.t.Fatal(err)
		}
		names = append(names, rel)
	}
	r, err := mktorrent.BuildDir(mktorrent.DirOptions{
		Root: root, Name: rev, DisplayName: id, Files: names, WebSeed: fx.hf.URL + "/" + id + "/",
		Announce: [][]string{{"http://127.0.0.1:1/announce"}},
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	info, _ := r.MetaInfo.UnmarshalInfo()
	m := catalog.Model{ID: id, HFRepo: "o/" + id, Revision: rev, License: "mit", Layout: catalog.LayoutDir, InfoHash: r.InfoHash, Magnet: r.Magnet}
	fx.mu.Lock()
	for _, f := range info.Files {
		rel := strings.Join(f.Path, "/")
		src := filepath.Join(root, filepath.FromSlash(rel))
		sum, _ := hashcache.HashFile(src)
		fx.content["/"+id+"/"+rev+"/"+rel] = src
		m.Files = append(m.Files, catalog.File{Name: rel, Size: f.Length, SHA256: sum})
	}
	fx.mu.Unlock()
	fx.src[r.InfoHash] = r.MetaInfo
	fx.cat.Models = append(fx.cat.Models, m)
	return root
}

// sameTree fails unless every file of files under want is byte-identical
// under got.
func sameTree(t *testing.T, want, got string, files map[string]int) {
	t.Helper()
	for rel := range files {
		a, err := os.ReadFile(filepath.Join(want, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(got, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s differs under %s (%v)", rel, got, err)
		}
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}

var rev1 = strings.Repeat("1", 40)

func TestDirDownloadFromWebSeed(t *testing.T) {
	fx := newFixture(t)
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	n := fx.node(nil) // wants nothing: /ensure adds it
	n.SetCatalog(fx.cat)
	if _, err := n.Ensure("tiny-dir"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, n, "tiny-dir", StateSeeding)
	final := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
	if st.Path != final || st.Percent != 100 || len(st.Files) != 1 {
		t.Fatalf("%+v", st)
	}
	if st2, err := n.Ensure("tiny-dir"); err != nil || st2.Path != final {
		t.Fatalf("ensure: %+v %v", st2, err)
	}
	sameTree(t, src, final, dirFiles)
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, rev1)); err == nil {
		t.Fatal("the torrent's info.name (revision) must not appear on disk")
	}
	if left, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".incoming")); len(left) != 0 {
		t.Fatalf(".incoming not empty: %v", left)
	}
	rec, ok := n.dl.Get(fx.cat.Models[0].InfoHash)
	if !ok || rec.Path != final || len(rec.Files) != len(dirFiles) {
		t.Fatalf("not recorded: %+v", rec)
	}
	if _, match := statRecorded(rec); !match {
		t.Fatal("record does not match the promoted directory (while seeding)")
	}
}

func TestDirQuarantineOnHashMismatch(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	fx.cat.Models[0].Files[1].SHA256 = strings.Repeat("0", 64)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny-dir", StateFailed)
	if !strings.Contains(st.Error, "sha256 mismatch") {
		t.Fatal(st.Error)
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny-dir")); err == nil {
		t.Fatal("unverified folder reached data_dir")
	}
	if q, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".quarantine")); len(q) != 1 || !q[0].IsDir() {
		t.Fatalf("quarantine: %v", q)
	}
}

// TestDirAdoptInPlace: a matching copy under models.adopt (with an extra
// file of its own, as an `hf download` dir has) is seeded where it is, not
// copied and not downloaded; a second full copy is a duplicate; a partial
// copy is ignored.
func TestDirAdoptInPlace(t *testing.T) {
	fx := newFixture(t)
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	adoptDir := t.TempDir()
	first := filepath.Join(adoptDir, "a", "DeepSeek-Tiny")
	second := filepath.Join(adoptDir, "b", "copy")
	partial := filepath.Join(adoptDir, "0-partial")
	copyTree(t, src, first)
	copyTree(t, src, second)
	copyTree(t, src, partial)
	os.Remove(filepath.Join(partial, "config.json"))
	os.MkdirAll(filepath.Join(first, ".cache", "huggingface"), 0o755)
	os.WriteFile(filepath.Join(first, "README.md"), []byte("extra"), 0o644)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"}; n.cfg.Models.Adopt = []string{adoptDir} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny-dir", StateSeeding)
	if st.Path != first || len(st.Duplicates) != 1 || st.Duplicates[0] != second {
		t.Fatalf("path %s duplicates %v", st.Path, st.Duplicates)
	}
	if fx.hfHits.Load() != 0 {
		t.Fatal("an adopted folder must not be downloaded")
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "tiny-dir")); err == nil {
		t.Fatal("adoption must not copy into data_dir")
	}
	if _, ok := n.dl.Get(fx.cat.Models[0].InfoHash); ok {
		t.Fatal("an adopted folder must not be recorded as downloaded")
	}
	for _, p := range []string{second, partial} {
		if _, err := os.Stat(filepath.Join(p, "inference", "kernel.py")); err != nil {
			t.Fatal("duplicates and partial copies are never deleted")
		}
	}
}

func TestDirPartialNotAdopted(t *testing.T) {
	fx := newFixture(t)
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	adoptDir := t.TempDir()
	partial := filepath.Join(adoptDir, "partial")
	copyTree(t, src, partial)
	bad := filepath.Join(partial, "model-00002-of-00002.safetensors")
	junk := bytes.Repeat([]byte{7}, dirFiles["model-00002-of-00002.safetensors"]) // right size, wrong content
	os.WriteFile(bad, junk, 0o644)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"}; n.cfg.Models.Adopt = []string{adoptDir} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny-dir", StateSeeding)
	if want := filepath.Join(n.cfg.Node.DataDir, "tiny-dir"); st.Path != want || fx.hfHits.Load() == 0 {
		t.Fatalf("a partial match must be downloaded, not adopted: %s, hf hits %d", st.Path, fx.hfHits.Load())
	}
	if got, _ := os.ReadFile(bad); !bytes.Equal(got, junk) {
		t.Fatal("the partial copy was modified")
	}
}

func TestDirFromViiworkConfig(t *testing.T) {
	fx := newFixture(t)
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	model := filepath.Join(t.TempDir(), "Tiny") // not under models.adopt
	copyTree(t, src, model)
	vwCfg := filepath.Join(t.TempDir(), "viiwork.yaml")
	os.WriteFile(vwCfg, []byte("models:\n  - name: tiny\n    path: "+model+"\n"), 0o644)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"}; n.cfg.Models.ViiworkConfigs = []string{vwCfg} })
	n.SetCatalog(fx.cat)
	st := waitState(t, n, "tiny-dir", StateSeeding)
	if st.Path != model || fx.hfHits.Load() != 0 {
		t.Fatalf("should seed viiwork's model directory in place: %+v, hf hits %d", st, fx.hfHits.Load())
	}
}

// TestDirAtTarget: data_dir/<id> is seeded in place when it matches, and
// left untouched (no download) when it is someone else's directory that
// doesn't — or not a directory at all.
func TestDirAtTarget(t *testing.T) {
	t.Run("matching", func(t *testing.T) {
		fx := newFixture(t)
		src := fx.addDirModel("tiny-dir", rev1, dirFiles)
		n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
		dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
		copyTree(t, src, dp)
		n.SetCatalog(fx.cat)
		if st := waitState(t, n, "tiny-dir", StateSeeding); st.Path != dp || fx.hfHits.Load() != 0 {
			t.Fatalf("%+v hits %d", st, fx.hfHits.Load())
		}
	})
	t.Run("user directory", func(t *testing.T) {
		fx := newFixture(t)
		src := fx.addDirModel("tiny-dir", rev1, dirFiles)
		n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
		dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
		copyTree(t, src, dp)
		mine := filepath.Join(dp, "config.json")
		os.WriteFile(mine, []byte("my own config"), 0o644)
		n.SetCatalog(fx.cat)
		st := waitState(t, n, "tiny-dir", StateFailed)
		if !strings.Contains(st.Error, "left untouched") {
			t.Fatal(st.Error)
		}
		if got, _ := os.ReadFile(mine); string(got) != "my own config" {
			t.Fatal("user directory modified")
		}
		time.Sleep(200 * time.Millisecond)
		if fx.hfHits.Load() != 0 {
			t.Fatal("must not download over a user directory")
		}
	})
	t.Run("file", func(t *testing.T) {
		fx := newFixture(t)
		fx.addDirModel("tiny-dir", rev1, dirFiles)
		n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
		os.MkdirAll(n.cfg.Node.DataDir, 0o755)
		dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
		os.WriteFile(dp, []byte("x"), 0o644)
		n.SetCatalog(fx.cat)
		if st := waitState(t, n, "tiny-dir", StateFailed); !strings.Contains(st.Error, "left untouched") {
			t.Fatal(st.Error)
		}
	})
}

// TestDirPromoteNoReplace: something that appears at data_dir/<id> while
// the folder downloads (even an empty directory, which rename(2) would
// silently replace) is left untouched, and the verified download stays in
// .incoming.
func TestDirPromoteNoReplace(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	gate := make(chan struct{})
	fx.mu.Lock()
	for p := range fx.content {
		fx.gates[p] = gate
	}
	fx.mu.Unlock()
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny-dir", StateDownloading)
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
	os.MkdirAll(dp, 0o755)
	close(gate)
	st := waitState(t, n, "tiny-dir", StateFailed)
	if !strings.Contains(st.Error, "left untouched") {
		t.Fatal(st.Error)
	}
	if e, _ := os.ReadDir(dp); len(e) != 0 {
		t.Fatalf("directory at the target was filled: %v", e)
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, ".incoming", "tiny-dir", "config.json")); err != nil {
		t.Fatal("verified download should stay in .incoming")
	}
}

// TestDirPrune: a recorded folder download is removed when its model
// leaves the catalog (prune: true) only while it is exactly as downloaded;
// a folder the user changed is left and its record forgotten.
func TestDirPrune(t *testing.T) {
	for _, change := range []string{"", "modified", "added"} {
		t.Run("change="+change, func(t *testing.T) {
			fx := newFixture(t)
			fx.addDirModel("tiny-dir", rev1, dirFiles)
			n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"}; n.cfg.Models.Prune = true })
			n.SetCatalog(fx.cat)
			waitState(t, n, "tiny-dir", StateSeeding)
			dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
			ih := fx.cat.Models[0].InfoHash
			switch change {
			case "modified":
				p := filepath.Join(dp, "inference", "kernel.py")
				b, _ := os.ReadFile(p)
				b[0] ^= 1
				os.WriteFile(p, b, 0o644)
				os.Chtimes(p, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
			case "added":
				os.WriteFile(filepath.Join(dp, "inference", "notes.txt"), []byte("mine"), 0o644)
			}
			n.SetCatalog(&catalog.Catalog{Version: 1})
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, recorded := n.dl.Get(ih)
				_, err := os.Stat(dp)
				gone := errors.Is(err, fs.ErrNotExist)
				if !recorded && (change == "") == gone {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("recorded %v, dir gone %v", recorded, gone)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if change != "" {
				if _, err := os.Stat(filepath.Join(dp, "config.json")); err != nil {
					t.Fatal("a changed folder must be left in place")
				}
			}
		})
	}
}

// TestDirPruneAcrossRestart: the record survives a restart, and a later
// catalog without the model prunes the (unchanged) directory.
func TestDirPruneAcrossRestart(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	cfg := testConfig(t)
	cfg.Models.Want = []string{"*"}
	cfg.Models.Prune = true
	mk := func() *Node {
		n, err := New(Options{Config: cfg, Source: fx.src, Policy: testPolicy(t, cfg.Local.CIDRs), Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := mk()
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny-dir", StateSeeding)
	n.Close()
	n = mk()
	defer n.Close()
	n.SetCatalog(&catalog.Catalog{Version: 1})
	dp := filepath.Join(cfg.Node.DataDir, "tiny-dir")
	if _, err := os.Stat(dp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("recorded folder not pruned after restart: %v", err)
	}
}

// TestDirRevisionBump: a new revision of a folder model (new infohash, same
// id) replaces viiwork-parrot's own unchanged download of the old one.
func TestDirRevisionBump(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	filesB := map[string]int{"config.json": 300, "model.safetensors": 2<<20 + 1, "inference/kernel.py": 10}
	srcB := fx.addDirModel("tiny-dir", strings.Repeat("2", 40), filesB)
	catA := &catalog.Catalog{Version: 2, Models: fx.cat.Models[:1]}
	catB := &catalog.Catalog{Version: 2, Models: fx.cat.Models[1:]}
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
	n.SetCatalog(catA)
	waitState(t, n, "tiny-dir", StateSeeding)
	n.SetCatalog(catB)
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, _ := n.ModelStatus("tiny-dir")
		if st.State == StateSeeding && len(st.Files) == 1 && st.Files[0].Size == catB.Models[0].TotalSize() {
			if st.Path != dp {
				t.Fatalf("path %s", st.Path)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rev B never seeding: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	sameTree(t, srcB, dp, filesB)
	if _, err := os.Stat(filepath.Join(dp, "model-00001-of-00002.safetensors")); err == nil {
		t.Fatal("rev A files left in the replaced folder")
	}
	if _, ok := n.dl.Get(catA.Models[0].InfoHash); ok {
		t.Error("rev A still recorded")
	}
	if rec, ok := n.dl.Get(catB.Models[0].InfoHash); !ok || rec.Path != dp {
		t.Errorf("rev B not recorded: %+v", rec)
	}
	if left, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".incoming")); len(left) != 0 {
		t.Fatalf(".incoming not empty: %v", left)
	}
}

// TestDirSeedingLeavesEmptyFilesAlone: seeding an adopted folder in place
// must not write to it — in particular anacrolix's storage must not
// re-create (O_TRUNC) its zero-length files, which would rewrite their
// mtime (and could truncate a file that changed since it was verified).
func TestDirSeedingLeavesEmptyFilesAlone(t *testing.T) {
	fx := newFixture(t)
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	adoptDir := t.TempDir()
	model := filepath.Join(adoptDir, "Tiny")
	copyTree(t, src, model)
	empty := filepath.Join(model, "empty.txt")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(empty, old, old)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"}; n.cfg.Models.Adopt = []string{adoptDir} })
	n.SetCatalog(fx.cat)
	if st := waitState(t, n, "tiny-dir", StateSeeding); st.Path != model {
		t.Fatalf("path %s", st.Path)
	}
	fi, err := os.Stat(empty)
	if err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("seeding touched the adopted empty file: %v %v", fi.ModTime(), err)
	}
}

// TestDirRevisionBumpPromoteFailureRestoresOld: if the new revision cannot
// be moved in after the old one was moved aside, the old one is moved back,
// still recorded as viiwork-parrot's own, and nothing leaks in .incoming.
func TestDirRevisionBumpPromoteFailureRestoresOld(t *testing.T) {
	fx := newFixture(t)
	srcA := fx.addDirModel("tiny-dir", rev1, dirFiles)
	fx.addDirModel("tiny-dir", strings.Repeat("2", 40), map[string]int{"config.json": 300, "model.safetensors": 1<<20 + 1})
	catA := &catalog.Catalog{Version: 2, Models: fx.cat.Models[:1]}
	catB := &catalog.Catalog{Version: 2, Models: fx.cat.Models[1:]}
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
	n.SetCatalog(catA)
	waitState(t, n, "tiny-dir", StateSeeding)
	lost := filepath.Join(t.TempDir(), "lost")
	testHookBeforeDirPromote = func(incoming, _ string) { os.Rename(incoming, lost) } // the new download vanishes
	t.Cleanup(func() { testHookBeforeDirPromote = nil })
	n.SetCatalog(catB)
	waitState(t, n, "tiny-dir", StateFailed)
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
	sameTree(t, srcA, dp, dirFiles)
	rec, ok := n.dl.Get(catA.Models[0].InfoHash)
	if !ok || rec.Path != dp {
		t.Fatal("rev A record lost")
	}
	if _, match := statRecorded(rec); !match {
		t.Fatal("restored rev A no longer matches its record")
	}
	e, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, ".incoming"))
	for _, x := range e {
		if strings.Contains(x.Name(), replacedMarker) {
			t.Fatalf(".incoming leaks the older revision: %v", x.Name())
		}
	}
}

// TestCleanReplacedAtStartup: an older revision left aside by a crash
// between promoteDir's renames is removed when the node starts.
func TestCleanReplacedAtStartup(t *testing.T) {
	cfg := testConfig(t)
	inc := filepath.Join(cfg.Node.DataDir, ".incoming")
	left := filepath.Join(inc, ".tiny-dir.replaced.123", "sub")
	os.MkdirAll(left, 0o755)
	os.WriteFile(filepath.Join(left, "f"), []byte("x"), 0o644)
	keep := filepath.Join(inc, "tiny-dir")
	os.MkdirAll(keep, 0o755)
	n, err := New(Options{Config: cfg, Source: fakeSource{}, Policy: testPolicy(t, cfg.Local.CIDRs), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := os.Stat(filepath.Join(inc, ".tiny-dir.replaced.123")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("leftover replaced revision not removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("an in-progress download was removed")
	}
}

// TestDirStaleIncoming: whatever another torrent left at .incoming/<id> (a
// per-file download's file of that name, or an older revision's partial
// folder, here with a directory where this torrent has a file) is cleared
// before the download starts; this torrent's own partial download is kept.
func TestDirStaleIncoming(t *testing.T) {
	for _, kind := range []string{"file", "older revision"} {
		t.Run(kind, func(t *testing.T) {
			fx := newFixture(t)
			src := fx.addDirModel("tiny-dir", rev1, dirFiles)
			n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
			inc := filepath.Join(n.cfg.Node.DataDir, ".incoming")
			stale := filepath.Join(inc, "tiny-dir")
			os.MkdirAll(inc, 0o755)
			if kind == "file" {
				os.WriteFile(stale, []byte("a per-file partial download"), 0o644)
			} else {
				os.MkdirAll(filepath.Join(stale, "config.json", "x"), 0o755)
				os.WriteFile(filepath.Join(stale, "old-only.bin"), []byte("x"), 0o644)
				os.WriteFile(filepath.Join(inc, ".tiny-dir.infohash"), []byte(strings.Repeat("f", 40)+"\n"), 0o644)
			}
			n.SetCatalog(fx.cat)
			st := waitState(t, n, "tiny-dir", StateSeeding)
			sameTree(t, src, st.Path, dirFiles)
			if _, err := os.Stat(filepath.Join(st.Path, "old-only.bin")); err == nil {
				t.Fatal("a stale file was promoted")
			}
			if e, _ := os.ReadDir(inc); len(e) != 0 {
				t.Fatalf(".incoming not empty: %v", e)
			}
		})
	}
	t.Run("own partial kept", func(t *testing.T) {
		fx := newFixture(t)
		fx.addDirModel("tiny-dir", rev1, dirFiles)
		n := fx.node(nil)
		m := fx.cat.Models[0]
		j := newJob(n, m.ID, dirUnit(m), nil)
		j.dir = &m
		if err := j.prepareIncomingDir(); err != nil {
			t.Fatal(err)
		}
		part := filepath.Join(j.incomingPath(), "config.json")
		os.MkdirAll(filepath.Dir(part), 0o755)
		os.WriteFile(part, []byte("partial"), 0o644)
		if err := j.prepareIncomingDir(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(part); err != nil {
			t.Fatal("this torrent's own partial download was removed")
		}
	})
}

// TestAddTorrentStorageOpenFailure: when anacrolix cannot open a torrent's
// storage (a file where the folder must be, so its empty file can't be
// created), it swallows the error and leaves the torrent without info;
// addTorrent must return an error instead of handing that torrent on
// (DownloadAll on it panics the daemon).
func TestAddTorrentStorageOpenFailure(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	n := fx.node(nil)
	m := fx.cat.Models[0]
	j := newJob(n, m.ID, dirUnit(m), nil)
	j.dir = &m
	notADir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(notADir, []byte("x"), 0o644)
	tt, err := j.addTorrent(j.storageAt(notADir), true)
	if err == nil || tt != nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("got %v, %v", tt, err)
	}
}

func TestCheckDirInfoRequiresRevisionName(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	m := fx.cat.Models[0]
	mi := fx.src[m.InfoHash]
	if err := checkDirInfo(mi, &m); err != nil {
		t.Fatal(err)
	}
	m.Revision = strings.Repeat("3", 40)
	if err := checkDirInfo(mi, &m); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("info name != revision must fail: %v", err)
	}
	m = fx.cat.Models[0]
	m.Files = m.Files[1:]
	if err := checkDirInfo(mi, &m); err == nil {
		t.Fatal("file list mismatch must fail")
	}
}

// TestDirReplacesOwnPerFileDownload: data_dir/<id> holds viiwork-parrot's own
// unchanged download of a per-file model's file that had that name in an
// older catalog; the folder model replaces it rather than failing "left
// untouched".
func TestDirReplacesOwnPerFileDownload(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("old", "tiny-dir", 1<<20) // a per-file model stored as data_dir/tiny-dir
	src := fx.addDirModel("tiny-dir", rev1, dirFiles)
	catA := &catalog.Catalog{Version: 1, Models: fx.cat.Models[:1]}
	catB := &catalog.Catalog{Version: 2, Models: fx.cat.Models[1:]}
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"} })
	n.SetCatalog(catA)
	waitState(t, n, "old", StateSeeding)
	dp := filepath.Join(n.cfg.Node.DataDir, "tiny-dir")
	if fi, err := os.Stat(dp); err != nil || !fi.Mode().IsRegular() {
		t.Fatal("per-file download not at data_dir/tiny-dir")
	}
	n.SetCatalog(catB)
	st := waitState(t, n, "tiny-dir", StateSeeding)
	if st.Path != dp {
		t.Fatal(st.Path)
	}
	sameTree(t, src, dp, dirFiles)
	if _, ok := n.dl.Get(catA.Models[0].Files[0].InfoHash); ok {
		t.Error("the replaced file is still recorded")
	}

	// A file that is not viiwork-parrot's own is still left untouched.
	fx2 := newFixture(t)
	fx2.addDirModel("tiny-dir", rev1, dirFiles)
	n2 := fx2.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny-dir"} })
	os.MkdirAll(n2.cfg.Node.DataDir, 0o755)
	os.WriteFile(filepath.Join(n2.cfg.Node.DataDir, "tiny-dir"), []byte("mine"), 0o644)
	n2.SetCatalog(fx2.cat)
	if st := waitState(t, n2, "tiny-dir", StateFailed); !strings.Contains(st.Error, "left untouched") {
		t.Fatal(st.Error)
	}
}

// checkDirInfo refuses a torrent whose file list differs from the catalog's
// in any way: a wrong size, or a duplicated path standing in for a missing
// file (same count, so only a per-path check catches it).
func TestCheckDirInfoRejectsMismatches(t *testing.T) {
	fx := newFixture(t)
	fx.addDirModel("tiny-dir", rev1, dirFiles)
	m := fx.cat.Models[0]
	mi := fx.src[m.InfoHash]

	m.Files = append([]catalog.File(nil), m.Files...)
	m.Files[0].Size++
	if err := checkDirInfo(mi, &m); err == nil {
		t.Fatal("size mismatch must fail")
	}

	m = fx.cat.Models[0]
	info, _ := mi.UnmarshalInfo()
	info.Files[1] = info.Files[0] // same count, file 1 missing, file 0 twice
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	dup := &metainfo.MetaInfo{InfoBytes: ib}
	if err := checkDirInfo(dup, &m); err == nil {
		t.Fatal("a duplicated path in place of a catalog file must fail")
	}
}

package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/hfapi"
)

// setupDir fakes an HF safetensors repo: nested paths, LFS shards and plain
// git files, plus .gitattributes and a local .cache/ (hf download
// --local-dir) that must both be ignored. lfsOid overrides the first
// shard's lfs.oid when non-empty.
func setupDir(t *testing.T, lfsOid string) Options {
	dir := t.TempDir()
	write := func(rel string, b []byte) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
	contents := map[string][]byte{
		"config.json":                      []byte(`{"a":1}`),
		"inference/kernel.py":              []byte("print(1)\n"),
		"model-00001-of-00002.safetensors": bytes.Repeat([]byte("s"), 90_000),
		"model-00002-of-00002.safetensors": bytes.Repeat([]byte("t"), 40_000),
		"LICENSE":                          []byte("license text\n"),
		".gitattributes":                   []byte("*.safetensors filter=lfs\n"),
	}
	for rel, b := range contents {
		write(rel, b)
	}
	write(".cache/huggingface/download/config.json.metadata", []byte("x"))
	var tree []map[string]any
	for rel, b := range contents {
		e := map[string]any{"type": "file", "size": len(b), "path": rel}
		if strings.HasSuffix(rel, ".safetensors") {
			sum, _ := hashcache.HashFile(filepath.Join(dir, rel))
			if lfsOid != "" && rel == "model-00001-of-00002.safetensors" {
				sum = lfsOid
			}
			e["oid"] = "zz"
			e["lfs"] = map[string]any{"oid": sum, "size": len(b)}
		} else {
			blob, _ := hfapi.GitBlobSHA1(bytes.NewReader(b), int64(len(b)))
			e["oid"] = blob
		}
		tree = append(tree, e)
	}
	tree = append(tree, map[string]any{"type": "directory", "oid": "dd", "size": 0, "path": "inference"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			json.NewEncoder(w).Encode(map[string]string{"sha": sha})
		case strings.Contains(r.URL.Path, "/tree/"):
			json.NewEncoder(w).Encode(tree)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Options{
		ID: "tiny-dir", Repo: "o/r", Revision: "main", License: "other", LicenseURL: "https://example.org/license",
		Layout: catalog.LayoutDir, LocalDir: dir, All: true, TorrentsDir: t.TempDir(),
		HF: &hfapi.Client{BaseURL: srv.URL, HTTP: srv.Client()},
	}
}

func TestAddDirModel(t *testing.T) {
	o := setupDir(t, "")
	var progress bytes.Buffer
	o.Progress = &progress
	m, err := AddModel(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsDir() || m.Revision != sha || m.LicenseURL != "https://example.org/license" {
		t.Fatalf("%+v", m)
	}
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
		want, _ := hashcache.HashFile(filepath.Join(o.LocalDir, filepath.FromSlash(f.Name)))
		if f.SHA256 != want || f.InfoHash != "" || f.Magnet != "" {
			t.Fatalf("%+v", f)
		}
	}
	if strings.Join(names, ",") != "LICENSE,config.json,inference/kernel.py,model-00001-of-00002.safetensors,model-00002-of-00002.safetensors" {
		t.Fatalf("files %v (want sorted, no .gitattributes or .cache)", names)
	}
	if !strings.Contains(progress.String(), "[4/5] model-00001-of-00002.safetensors") {
		t.Fatalf("progress:\n%s", progress.String())
	}
	mi, err := metainfo.LoadFromFile(filepath.Join(o.TorrentsDir, m.InfoHash+".torrent"))
	if err != nil || mi.HashInfoBytes().HexString() != m.InfoHash {
		t.Fatalf("torrent: %v", err)
	}
	info, _ := mi.UnmarshalInfo()
	if info.Name != sha || len(info.Files) != 5 || mi.UrlList[0] != "https://huggingface.co/o/r/resolve/" {
		t.Fatalf("info %q files %d url-list %v", info.Name, len(info.Files), mi.UrlList)
	}
	if err := catalog.Validate([]catalog.Model{m}); err != nil {
		t.Fatal(err)
	}

	// Same files and revision -> same torrent.
	m2, err := AddModel(context.Background(), o)
	if err != nil || m2.InfoHash != m.InfoHash {
		t.Fatalf("not deterministic: %v", err)
	}

	// An explicit subset works too.
	o.All, o.Files = false, []string{"config.json", "inference/kernel.py"}
	if m3, err := AddModel(context.Background(), o); err != nil || len(m3.Files) != 2 {
		t.Fatalf("%+v %v", m3, err)
	}
}

func TestAddDirModelRejectsMismatch(t *testing.T) {
	o := setupDir(t, strings.Repeat("0", 64))
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "lfs oid") {
		t.Fatalf("expected lfs sha256 mismatch, got %v", err)
	}
	if left, _ := os.ReadDir(o.TorrentsDir); len(left) != 0 {
		t.Fatalf("no torrent may be written on a mismatch: %v", left)
	}

	o = setupDir(t, "")
	os.WriteFile(filepath.Join(o.LocalDir, "inference", "kernel.py"), []byte("print(2)\n"), 0o644) // same size
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "git blob") {
		t.Fatalf("expected git blob mismatch, got %v", err)
	}

	o = setupDir(t, "")
	os.WriteFile(filepath.Join(o.LocalDir, "config.json"), []byte("short"), 0o644)
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected size mismatch, got %v", err)
	}

	o = setupDir(t, "")
	os.Remove(filepath.Join(o.LocalDir, "LICENSE"))
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("a missing local file must fail")
	}

	o = setupDir(t, "")
	o.All, o.Files = false, []string{"nope.json"}
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("a file missing from the HF tree must fail")
	}
	o.Files = []string{"inference"}
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("a directory entry is not a file")
	}
	o.All = true
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("--all with a file list must fail")
	}
	o = setupDir(t, "")
	o.Layout = ""
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("--all without layout dir must fail")
	}
}

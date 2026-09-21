package mktorrent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/webseed"
)

func dirTree(t *testing.T, files map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for rel, size := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, size)
		rand.Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildDir(t *testing.T) {
	rev := strings.Repeat("c", 40)
	base := "https://huggingface.co/o/r/resolve/"
	root := dirTree(t, map[string]int{
		"config.json":                      300,
		"model-00001-of-00002.safetensors": 2<<20 + 5,
		"model-00002-of-00002.safetensors": 1<<20 + 7,
		"inference/kernel.py":              1234,
		"inference/a b+c#d.json":           10,
		"empty.txt":                        0,
	})
	files := []string{"model-00002-of-00002.safetensors", "config.json", "inference/kernel.py", "model-00001-of-00002.safetensors", "inference/a b+c#d.json", "empty.txt"}
	sums := map[string]hash.Hash{}
	r, err := BuildDir(DirOptions{Root: root, Name: rev, DisplayName: "tiny-dir", Files: files, WebSeed: base,
		Tee: func(rel string, _ int64) io.Writer { h := sha256.New(); sums[rel] = h; return h }})
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.MetaInfo.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != rev || !info.IsDir() || info.PieceLength != metainfo.ChoosePieceLength(info.TotalLength()) {
		t.Fatalf("info: name %q dir %v piece %d", info.Name, info.IsDir(), info.PieceLength)
	}
	var got []string
	for _, f := range info.Files {
		got = append(got, strings.Join(f.Path, "/"))
	}
	want := []string{"config.json", "empty.txt", "inference/a b+c#d.json", "inference/kernel.py", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files %v, want sorted %v", got, want)
	}
	if !reflect.DeepEqual(info.Files[4].Path, []string{"model-00001-of-00002.safetensors"}) || !reflect.DeepEqual(info.Files[3].Path, []string{"inference", "kernel.py"}) {
		t.Fatal("paths must be split into components")
	}
	if len(r.MetaInfo.UrlList) != 1 || r.MetaInfo.UrlList[0] != base {
		t.Fatalf("url-list %v", r.MetaInfo.UrlList)
	}
	if !strings.Contains(r.Magnet, "ws="+strings.ReplaceAll(strings.ReplaceAll(base, ":", "%3A"), "/", "%2F")) || !strings.Contains(r.Magnet, "dn=tiny-dir") {
		t.Fatalf("magnet %s", r.Magnet)
	}

	// The tee saw every byte of every file.
	for _, rel := range want {
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		s := sha256.Sum256(b)
		if h := sums[rel]; h == nil || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(s[:]) {
			t.Fatalf("%s: tee sha256 wrong", rel)
		}
	}

	// BEP-19 as anacrolix builds it (webseed.urlForFileIndex: url + escape(
	// info.BestName(), file path...) when the url ends in "/"): HF's
	// resolve/<revision>/<path>, each component path-escaped (space -> %20,
	// + -> %2B, # -> %23), which HF decodes back to the file name.
	urls := map[string]string{}
	for _, f := range info.UpvertedFiles() {
		urls[strings.Join(f.BestPath(), "/")] = base + webseed.EscapePath(append([]string{info.BestName()}, f.BestPath()...))
	}
	if u := urls["inference/kernel.py"]; u != base+rev+"/inference/kernel.py" {
		t.Fatal(u)
	}
	if u := urls["inference/a b+c#d.json"]; u != base+rev+"/inference/a%20b%2Bc%23d.json" {
		t.Fatal(u)
	}

	// Deterministic: same files and revision -> same infohash, whatever
	// the listing order; another revision -> another torrent.
	r2, err := BuildDir(DirOptions{Root: root, Name: rev, Files: want, WebSeed: base})
	if err != nil || r2.InfoHash != r.InfoHash {
		t.Fatalf("infohash not deterministic: %v", err)
	}
	r3, _ := BuildDir(DirOptions{Root: root, Name: strings.Repeat("e", 40), Files: want, WebSeed: base})
	if r3.InfoHash == r.InfoHash {
		t.Fatal("revision must be part of the infohash (info.name)")
	}
}

func TestBuildDirRejects(t *testing.T) {
	root := dirTree(t, map[string]int{"a.json": 10})
	ok := DirOptions{Root: root, Name: strings.Repeat("c", 40), Files: []string{"a.json"}, WebSeed: "https://h/o/r/resolve/"}
	for name, mut := range map[string]func(o *DirOptions){
		"no trailing slash": func(o *DirOptions) { o.WebSeed = "https://h/o/r/resolve" },
		"escaping path":     func(o *DirOptions) { o.Files = []string{"../a.json"} },
		"absolute path":     func(o *DirOptions) { o.Files = []string{"/a.json"} },
		"missing file":      func(o *DirOptions) { o.Files = []string{"b.json"} },
		"duplicate":         func(o *DirOptions) { o.Files = []string{"a.json", "a.json"} },
		"no files":          func(o *DirOptions) { o.Files = nil },
		"name with slash":   func(o *DirOptions) { o.Name = "a/b" },
	} {
		o := ok
		mut(&o)
		if _, err := BuildDir(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := BuildDir(ok); err != nil {
		t.Fatal(err)
	}
}

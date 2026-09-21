package mktorrent

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func randFile(t *testing.T, size int) string {
	p := filepath.Join(t.TempDir(), "src.bin")
	b := make([]byte, size)
	rand.Read(b)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuild(t *testing.T) {
	p := randFile(t, 3<<20+123)
	ws := "https://huggingface.co/o/r/resolve/" + strings.Repeat("a", 40) + "/m.gguf"
	r, err := Build(Options{Path: p, Name: "m.gguf", WebSeed: ws})
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.MetaInfo.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "m.gguf" || info.Length != 3<<20+123 || len(info.Files) != 0 {
		t.Fatalf("info: %+v", info)
	}
	if want := int((info.Length + info.PieceLength - 1) / info.PieceLength); info.NumPieces() != want {
		t.Fatalf("pieces %d want %d", info.NumPieces(), want)
	}
	if len(r.MetaInfo.UrlList) != 1 || r.MetaInfo.UrlList[0] != ws {
		t.Fatalf("url-list %v", r.MetaInfo.UrlList)
	}
	wantAnnounce := [][]string{
		{"https://parrot.lnx.fi/announce"},
		{"udp://tracker.pirateface.co:6969/announce"},
		{"udp://tracker.opentrackr.org:1337/announce"},
		{"udp://open.stealth.si:80/announce"},
	}
	if !reflect.DeepEqual(r.MetaInfo.AnnounceList, metainfo.AnnounceList(wantAnnounce)) {
		t.Fatalf("announce %v, want %v", r.MetaInfo.AnnounceList, wantAnnounce)
	}
	if len(r.InfoHash) != 40 || !strings.Contains(r.Magnet, "xt=urn:btih:"+r.InfoHash) || !strings.Contains(r.Magnet, "ws=") {
		t.Fatalf("magnet %s", r.Magnet)
	}

	// Same bytes and name -> same infohash (nothing time-dependent in info).
	r2, _ := Build(Options{Path: p, Name: "m.gguf", WebSeed: ws})
	if r2.InfoHash != r.InfoHash {
		t.Fatal("infohash not deterministic")
	}

	out := filepath.Join(t.TempDir(), r.InfoHash+".torrent")
	if err := Write(out, r.MetaInfo); err != nil {
		t.Fatal(err)
	}
	mi, err := metainfo.LoadFromFile(out)
	if err != nil || mi.HashInfoBytes().HexString() != r.InfoHash {
		t.Fatalf("reload: %v", err)
	}
}

func TestBuildRejects(t *testing.T) {
	p := randFile(t, 10)
	if _, err := Build(Options{Path: p, Name: "x", WebSeed: "https://h/dir/"}); err == nil {
		t.Fatal("trailing-slash web-seed must be rejected (BEP-19 would append the name)")
	}
	if _, err := Build(Options{Path: filepath.Dir(p), Name: "x", WebSeed: "https://h/x"}); err == nil {
		t.Fatal("directories must be rejected")
	}
}

// TestWriteIsAtomic: Write replaces the target by rename, never by
// truncating it in place — a reader (or a hardlink to the old file, as
// here) never sees a half-written torrent, and no temp file is left over.
func TestWriteIsAtomic(t *testing.T) {
	p := randFile(t, 1<<20)
	r, err := Build(Options{Path: p, Name: "m.gguf", WebSeed: "https://h/m.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "x.torrent")
	old := []byte("old torrent bytes")
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Link(dst, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(dst, r.MetaInfo); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(link); string(got) != string(old) {
		t.Fatal("Write modified the existing file in place instead of replacing it")
	}
	if _, err := metainfo.LoadFromFile(dst); err != nil {
		t.Fatalf("written torrent does not load: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("leftover files: %v", entries)
	}
}

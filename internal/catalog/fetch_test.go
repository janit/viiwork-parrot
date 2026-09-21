package catalog

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

type fakeServer struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[r.URL.Path]++
	b, ok := s.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(b)
}

func signedServer(t *testing.T) (*fakeServer, *httptest.Server, string) {
	pub, priv, _ := GenerateKey()
	data, err := Build(sampleModels(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := Sign(priv, data)
	fs := &fakeServer{files: map[string][]byte{"/v/catalog.json": data, "/v/catalog.json.sig": sig}, hits: map[string]int{}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return fs, srv, pub
}

func TestFetchVerifiesAndCaches(t *testing.T) {
	fs, srv, pub := signedServer(t)
	f := &Fetcher{URL: srv.URL + "/v/catalog.json", PubKey: pub, CacheDir: t.TempDir(), HTTP: srv.Client()}
	c, err := f.Fetch(context.Background())
	if err != nil || len(c.Models) != 1 {
		t.Fatalf("%v %v", c, err)
	}

	// Tamper: server now serves a modified body with the old signature.
	fs.mu.Lock()
	fs.files["/v/catalog.json"] = bytes.Replace(fs.files["/v/catalog.json"], []byte("gemma"), []byte("gemmb"), 1)
	fs.mu.Unlock()
	c, err = f.Fetch(context.Background())
	if err == nil || c == nil || c.Models[0].License != "gemma" {
		t.Fatalf("tampered catalog must fall back to cache with error: %v %+v", err, c)
	}

	// No cache + failure -> nil.
	f2 := &Fetcher{URL: srv.URL + "/v/catalog.json", PubKey: pub, CacheDir: t.TempDir(), HTTP: srv.Client()}
	if c, err := f2.Fetch(context.Background()); c != nil || err == nil {
		t.Fatal("expected nil catalog without cache")
	}
}

func TestFetchGarbledCache(t *testing.T) {
	_, srv, pub := signedServer(t)
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "catalog.cache"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{URL: srv.URL + "/does-not-exist.json", PubKey: pub, CacheDir: cacheDir, HTTP: srv.Client()}
	c, err := f.Fetch(context.Background())
	if c != nil || err == nil {
		t.Fatalf("garbled cache must not be served: %v %+v", err, c)
	}
}

func TestFetchTorrent(t *testing.T) {
	fs, srv, pub := signedServer(t)
	src := filepath.Join(t.TempDir(), "m.gguf")
	os.WriteFile(src, bytes.Repeat([]byte("x"), 100_000), 0o644)
	r, err := mktorrent.Build(mktorrent.Options{Path: src, Name: "m.gguf", WebSeed: "https://h/m.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	r.MetaInfo.Write(&buf)
	fs.files["/v/torrents/"+r.InfoHash+".torrent"] = buf.Bytes()

	f := &Fetcher{URL: srv.URL + "/v/catalog.json", PubKey: pub, CacheDir: t.TempDir(), HTTP: srv.Client()}
	for i := 0; i < 2; i++ {
		mi, err := f.Torrent(context.Background(), r.InfoHash)
		if err != nil || mi.HashInfoBytes().HexString() != r.InfoHash {
			t.Fatalf("torrent: %v", err)
		}
	}
	if fs.hits["/v/torrents/"+r.InfoHash+".torrent"] != 1 {
		t.Fatal("second Torrent() should come from cache")
	}

	wrong := strings.Repeat("0", 40)
	fs.files["/v/torrents/"+wrong+".torrent"] = buf.Bytes()
	if _, err := f.Torrent(context.Background(), wrong); err == nil {
		t.Fatal("infohash mismatch must be rejected")
	}
}

// TestFetchRejectsRollback: a validly signed catalog that is older than the
// cached verified one (a replayed old catalog) is not accepted; the cached
// catalog stays in use and Fetch reports an error. An equal or newer one is
// accepted.
func TestFetchRejectsRollback(t *testing.T) {
	pub, priv, _ := GenerateKey()
	sign := func(gen time.Time, license string) ([]byte, []byte) {
		m := sampleModels()
		m[0].License = license
		data, err := Build(m, gen)
		if err != nil {
			t.Fatal(err)
		}
		sig, _ := Sign(priv, data)
		return data, sig
	}
	t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	newData, newSig := sign(t0, "new")
	oldData, oldSig := sign(t0.Add(-time.Hour), "old")
	fs := &fakeServer{files: map[string][]byte{"/v/catalog.json": newData, "/v/catalog.json.sig": newSig}, hits: map[string]int{}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	f := &Fetcher{URL: srv.URL + "/v/catalog.json", PubKey: pub, CacheDir: t.TempDir(), HTTP: srv.Client()}
	if c, err := f.Fetch(context.Background()); err != nil || c.Models[0].License != "new" {
		t.Fatalf("%v %v", c, err)
	}

	fs.mu.Lock()
	fs.files["/v/catalog.json"], fs.files["/v/catalog.json.sig"] = oldData, oldSig
	fs.mu.Unlock()
	c, err := f.Fetch(context.Background())
	if err == nil || c == nil || c.Models[0].License != "new" {
		t.Fatalf("older signed catalog must be refused in favour of the cache: %v %+v", err, c)
	}
	// The refused catalog must not have replaced the cache either.
	if c, err := f.loadCache(); err != nil || c.Models[0].License != "new" {
		t.Fatalf("cache overwritten by rolled-back catalog: %v %+v", err, c)
	}

	sameData, sameSig := sign(t0, "same-time")
	fs.mu.Lock()
	fs.files["/v/catalog.json"], fs.files["/v/catalog.json.sig"] = sameData, sameSig
	fs.mu.Unlock()
	if c, err := f.Fetch(context.Background()); err != nil || c.Models[0].License != "same-time" {
		t.Fatalf("same-generation catalog should be accepted: %v %+v", err, c)
	}
}

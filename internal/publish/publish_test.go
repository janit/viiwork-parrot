package publish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/hfapi"
)

var sha = strings.Repeat("ab", 20)

func setup(t *testing.T, ggufLFSOid string) (Options, string) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "q"), 0o755)
	os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "q", "m.gguf"), []byte(strings.Repeat("w", 70_000)), 0o644)
	ggufSHA, _ := hashcache.HashFile(filepath.Join(dir, "q", "m.gguf"))
	if ggufLFSOid == "" {
		ggufLFSOid = ggufSHA
	}
	tree := []map[string]any{
		{"type": "file", "oid": "ce013625030ba8dba906f756967f9e9ca394464a", "size": 6, "path": "LICENSE"},
		{"type": "file", "oid": "zz", "size": 70_000, "path": "q/m.gguf", "lfs": map[string]any{"oid": ggufLFSOid, "size": 70_000}},
	}
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
		ID: "tiny-q", Repo: "o/r", Revision: "main", License: "apache-2.0",
		LocalDir: dir, Files: []string{"q/m.gguf", "LICENSE"}, TorrentsDir: t.TempDir(),
		HF: &hfapi.Client{BaseURL: srv.URL, HTTP: srv.Client()},
	}, ggufSHA
}

func TestAddModel(t *testing.T) {
	o, ggufSHA := setup(t, "")
	m, err := AddModel(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != sha || len(m.Files) != 2 {
		t.Fatalf("%+v", m)
	}
	g, lic := m.Files[0], m.Files[1]
	if g.DiskName() != "m.gguf" || g.SHA256 != ggufSHA || g.Size != 70_000 {
		t.Fatalf("gguf: %+v", g)
	}
	if lic.DiskName() != "tiny-q.LICENSE" || lic.SHA256 != "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" {
		t.Fatalf("license: %+v", lic)
	}
	for _, f := range m.Files {
		if _, err := os.Stat(filepath.Join(o.TorrentsDir, f.InfoHash+".torrent")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAddModelRejectsMismatch(t *testing.T) {
	o, _ := setup(t, strings.Repeat("0", 64))
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
	o, _ = setup(t, "")
	os.WriteFile(filepath.Join(o.LocalDir, "LICENSE"), []byte("hellO\n"), 0o644)
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "git blob") {
		t.Fatalf("expected git blob mismatch, got %v", err)
	}
	o, _ = setup(t, "")
	o.Files = []string{"nope.gguf"}
	if _, err := AddModel(context.Background(), o); err == nil {
		t.Fatal("file missing from HF tree must fail")
	}
}

func TestDiskName(t *testing.T) {
	if DiskName("x", "a/b.gguf", nil) != "b.gguf" || DiskName("x", "README.md", nil) != "x.README.md" || DiskName("x", "a.gguf", map[string]string{"a.gguf": "z.gguf"}) != "z.gguf" {
		t.Fatal("DiskName")
	}
}

// A model that fails catalog validation writes no .torrent at all.
func TestAddModelInvalidWritesNothing(t *testing.T) {
	o, _ := setup(t, "")
	o.LicenseURL = "http://example.com/license"
	if _, err := AddModel(context.Background(), o); err == nil || !strings.Contains(err.Error(), "license_url") {
		t.Fatalf("expected license_url rejection, got %v", err)
	}
	if ents, _ := os.ReadDir(o.TorrentsDir); len(ents) != 0 {
		t.Fatalf("rejected model left torrents behind: %v", ents)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork-parrot/internal/catalog"
)

// TestCatalogPageCLI exercises `catalog build|sign|verify|page` end to end
// against the repo's real catalog.yaml, the way scripts/catalog-publish.sh
// does, and checks the generated index.html looks sane.
func TestCatalogPageCLI(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	indexPath := filepath.Join(dir, "index.html")
	keyPath := filepath.Join(dir, "catalog.key")

	pub, priv, err := catalog.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte(priv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runCatalog([]string{"build", "--in", "../../catalog.yaml", "--out", catalogPath}); err != nil {
		t.Fatalf("catalog build: %v", err)
	}
	if err := runCatalog([]string{"sign", "--key", keyPath, "--in", catalogPath}); err != nil {
		t.Fatalf("catalog sign: %v", err)
	}
	if err := runCatalog([]string{"verify", "--pubkey", pub, "--in", catalogPath}); err != nil {
		t.Fatalf("catalog verify: %v", err)
	}
	if err := runCatalog([]string{"page", "--in", catalogPath, "--out", indexPath}); err != nil {
		t.Fatalf("catalog page: %v", err)
	}

	html, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading generated index.html: %v", err)
	}
	out := string(html)

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Error("generated page does not start with <!doctype html>")
	}
	if !strings.Contains(out, "qwen3.8-27b-q4kxl") {
		t.Error("generated page missing a known model id from catalog.yaml")
	}
	if strings.Contains(strings.ToLower(out), "<script") {
		t.Error("generated page contains a <script tag")
	}
}

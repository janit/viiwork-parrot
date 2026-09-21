package hashcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const helloSHA = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" // "hello\n"

func TestHashFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("hello\n"), 0o644)
	got, err := HashFile(p)
	if err != nil || got != helloSHA {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestCacheHitMissAndPersistence(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "real.gguf")
	link := filepath.Join(dir, "alias.gguf")
	os.WriteFile(data, []byte("hello\n"), 0o644)
	os.Symlink(data, link)

	c, err := Open(filepath.Join(dir, "hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.hash = func(p string) (string, error) { calls++; return HashFile(p) }

	for _, p := range []string{data, link, data} {
		if got, err := c.SHA256(p); err != nil || got != helloSHA {
			t.Fatalf("%s: %s %v", p, got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("symlink and repeat should hit cache; hashed %d times", calls)
	}

	// mtime change invalidates.
	later := time.Now().Add(time.Minute)
	os.Chtimes(data, later, later)
	c.SHA256(link)
	if calls != 2 {
		t.Fatalf("mtime change should rehash; hashed %d times", calls)
	}

	// Persisted across Open.
	c2, _ := Open(filepath.Join(dir, "hashes.json"))
	c2.hash = func(string) (string, error) { t.Fatal("should be cached on disk"); return "", nil }
	if got, _ := c2.SHA256(data); got != helloSHA {
		t.Fatal(got)
	}
}

func TestPut(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "x")
	os.WriteFile(data, []byte("hello\n"), 0o644)
	c, _ := Open(filepath.Join(dir, "hashes.json"))
	c.hash = func(string) (string, error) { t.Fatal("Put value should be used"); return "", nil }
	if err := c.Put(data, helloSHA); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.SHA256(data); got != helloSHA {
		t.Fatal(got)
	}
}

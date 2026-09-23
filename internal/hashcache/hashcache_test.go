package hashcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	c.hash = func(ctx context.Context, p string) (string, error) { calls++; return HashFileContext(ctx, p) }

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
	c2.hash = func(context.Context, string) (string, error) { t.Fatal("should be cached on disk"); return "", nil }
	if got, _ := c2.SHA256(data); got != helloSHA {
		t.Fatal(got)
	}
}

func TestPut(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "x")
	os.WriteFile(data, []byte("hello\n"), 0o644)
	c, _ := Open(filepath.Join(dir, "hashes.json"))
	c.hash = func(context.Context, string) (string, error) { t.Fatal("Put value should be used"); return "", nil }
	if err := c.Put(data, helloSHA); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.SHA256(data); got != helloSHA {
		t.Fatal(got)
	}
}

func TestHashFileContextCancelled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, make([]byte, 1<<20), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashFileContext(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	c, _ := Open(filepath.Join(t.TempDir(), "h.json"))
	if sum, err := c.SHA256Context(ctx, p); sum != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %q %v", sum, err)
	}
}

// Concurrent callers for one file hash it once; hashes of different files
// run at most maxHashes at a time.
func TestConcurrentHashesDedupedAndBounded(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(filepath.Join(dir, "hashes.json"))
	var mu sync.Mutex
	calls, running, peak := map[string]int{}, 0, 0
	c.hash = func(ctx context.Context, p string) (string, error) {
		mu.Lock()
		calls[p]++
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return HashFileContext(ctx, p)
	}
	var files []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d", i))
		os.WriteFile(p, []byte("hello\n"), 0o644)
		files = append(files, p)
	}
	var wg sync.WaitGroup
	for _, p := range files {
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got, err := c.SHA256(p); err != nil || got != helloSHA {
					t.Errorf("%s: %s %v", p, got, err)
				}
			}()
		}
	}
	wg.Wait()
	for p, n := range calls {
		if n != 1 {
			t.Errorf("%s hashed %d times", p, n)
		}
	}
	if peak > maxHashes {
		t.Errorf("%d hashes ran at once, limit %d", peak, maxHashes)
	}
}

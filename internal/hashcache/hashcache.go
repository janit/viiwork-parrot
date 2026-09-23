// Package hashcache hashes model files and remembers results by resolved
// path, size and mtime so multi-GB files are hashed once.
package hashcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

func HashFile(path string) (string, error) {
	return HashFileContext(context.Background(), path)
}

// HashFileContext is HashFile, abandoned with ctx's error once ctx is done:
// a multi-GB hash must not hold up shutdown.
func HashFileContext(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type entry struct {
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime_ns"`
	SHA256 string `json:"sha256"`
}

// maxHashes bounds concurrent cache-miss hashes: at startup every model's
// job hashes its candidates at once, and N multi-GB sequential reads in
// parallel only thrash the disk.
const maxHashes = 2

type Cache struct {
	path string
	mu   sync.Mutex
	m    map[string]entry
	busy map[string]chan struct{} // real path -> closed when its in-flight hash ends
	sem  chan struct{}
	hash func(context.Context, string) (string, error)
}

func Open(path string) (*Cache, error) {
	c := &Cache{path: path, m: map[string]entry{}, busy: map[string]chan struct{}{}, sem: make(chan struct{}, maxHashes), hash: HashFileContext}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &c.m); err != nil {
		// A corrupt cache only costs rehashing.
		c.m = map[string]entry{}
	}
	return c, nil
}

func (c *Cache) key(path string) (string, os.FileInfo, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", nil, err
	}
	return real, fi, nil
}

func (c *Cache) SHA256(path string) (string, error) {
	return c.SHA256Context(context.Background(), path)
}

// SHA256Context is SHA256 with the hashing (if the cache misses) abandoned
// once ctx is done.
func (c *Cache) SHA256Context(ctx context.Context, path string) (string, error) {
	real, fi, err := c.key(path)
	if err != nil {
		return "", err
	}
	for {
		c.mu.Lock()
		e, ok := c.m[real]
		if ok && e.Size == fi.Size() && e.MTime == fi.ModTime().UnixNano() {
			c.mu.Unlock()
			return e.SHA256, nil
		}
		// One hash per file at a time: a second caller waits for the
		// first and then finds its result cached (or hashes itself if the
		// first failed).
		if ch, busy := c.busy[real]; busy {
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		ch := make(chan struct{})
		c.busy[real] = ch
		c.mu.Unlock()
		sum, err := c.hashLimited(ctx, real)
		var serr error
		if err == nil {
			serr = c.store(real, fi, sum) // in memory before waiters wake
		}
		c.mu.Lock()
		delete(c.busy, real)
		close(ch)
		c.mu.Unlock()
		if err != nil {
			return "", err
		}
		return sum, serr
	}
}

func (c *Cache) hashLimited(ctx context.Context, real string) (string, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.sem }()
	return c.hash(ctx, real)
}

func (c *Cache) Put(path, sha string) error {
	real, fi, err := c.key(path)
	if err != nil {
		return err
	}
	return c.store(real, fi, sha)
}

func (c *Cache) store(real string, fi os.FileInfo, sum string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[real] = entry{Size: fi.Size(), MTime: fi.ModTime().UnixNano(), SHA256: sum}
	data, err := json.MarshalIndent(c.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

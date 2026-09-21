// Package hashcache hashes model files and remembers results by resolved
// path, size and mtime so multi-GB files are hashed once.
package hashcache

import (
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
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type entry struct {
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime_ns"`
	SHA256 string `json:"sha256"`
}

type Cache struct {
	path string
	mu   sync.Mutex
	m    map[string]entry
	hash func(string) (string, error)
}

func Open(path string) (*Cache, error) {
	c := &Cache{path: path, m: map[string]entry{}, hash: HashFile}
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
	real, fi, err := c.key(path)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	e, ok := c.m[real]
	c.mu.Unlock()
	if ok && e.Size == fi.Size() && e.MTime == fi.ModTime().UnixNano() {
		return e.SHA256, nil
	}
	sum, err := c.hash(real)
	if err != nil {
		return "", err
	}
	return sum, c.store(real, fi, sum)
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

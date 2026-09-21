package node

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// downloadedFile is what viiwork-parrot remembers about a file it downloaded
// itself: where it put it, and its size/mtime at that moment, so a later
// prune can confirm the file is still what viiwork-parrot put there before ever
// deleting it (the user may have replaced it with their own file of the
// same name).
type downloadedFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime_ns"`
}

// downloadRecord persists which files under data_dir viiwork-parrot itself
// downloaded (as opposed to adopted in place), keyed by infohash, so that
// prune still recognizes them after a restart even though the in-memory
// job.downloaded flag doesn't survive one.
type downloadRecord struct {
	path string
	mu   sync.Mutex
	m    map[string]downloadedFile // infohash -> recorded file
}

func openDownloadRecord(path string) (*downloadRecord, error) {
	r := &downloadRecord{path: path, m: map[string]downloadedFile{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.m); err != nil {
		// A corrupt record only costs re-detecting downloads as adoptions.
		r.m = map[string]downloadedFile{}
	}
	return r, nil
}

func (r *downloadRecord) Get(infohash string) (downloadedFile, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.m[infohash]
	return f, ok
}

// All returns a snapshot copy of the infohash -> recorded-file map.
func (r *downloadRecord) All() map[string]downloadedFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]downloadedFile, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func (r *downloadRecord) Put(infohash string, f downloadedFile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[infohash] = f
	return r.saveLocked()
}

func (r *downloadRecord) Delete(infohash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[infohash]; !ok {
		return nil
	}
	delete(r.m, infohash)
	return r.saveLocked()
}

func (r *downloadRecord) saveLocked() error {
	data, err := json.MarshalIndent(r.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// statRecorded reports whether the recorded file is missing, and if it's
// not, whether its current size and mtime still match what was recorded —
// the cheap guard every deletion of a "viiwork-parrot downloaded this" path goes
// through, so a file the user replaced (or restored from elsewhere) with
// their own content of the same name never gets deleted.
func statRecorded(f downloadedFile) (missing, match bool) {
	fi, err := os.Lstat(f.Path)
	if err != nil {
		return true, false
	}
	// Lstat: a symlink now sitting at the path is not the regular file we
	// downloaded there, whatever it points to.
	return false, fi.Mode().IsRegular() && fi.Size() == f.Size && fi.ModTime().UnixNano() == f.ModTime
}

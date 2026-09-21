//go:build unix

package node

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

func statfsFree(dir string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil
}

var diskFree = statfsFree

// allocatedBytes is how much disk the file at path already occupies
// (st_blocks, so a sparse, partly downloaded file counts only what was
// written), or for a directory (a folder model's download) the sum over the
// regular files under it. 0 if it doesn't exist.
func allocatedBytes(path string) int64 {
	var s syscall.Stat_t
	if err := syscall.Stat(path, &s); err != nil {
		return 0
	}
	if s.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return s.Blocks * 512
	}
	var total int64
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			var st syscall.Stat_t
			if syscall.Lstat(p, &st) == nil {
				total += st.Blocks * 512
			}
		}
		return nil
	})
	return total
}

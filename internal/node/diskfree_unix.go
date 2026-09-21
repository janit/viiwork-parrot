//go:build unix

package node

import "syscall"

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
// written). 0 if it doesn't exist.
func allocatedBytes(path string) int64 {
	var s syscall.Stat_t
	if err := syscall.Stat(path, &s); err != nil {
		return 0
	}
	return s.Blocks * 512
}

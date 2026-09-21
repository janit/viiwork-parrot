package node

import (
	"fmt"
	"os"
	"syscall"
)

// FileIOEnv is anacrolix's storage file-IO selector ("mmap", its default,
// or "classic"). It is read once, in the storage package's init, so it has
// to be in the environment when the process starts; there is no per-client
// or per-storage option at the pinned commit.
const FileIOEnv = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// EnsureClassicFileIO makes sure this process uses classic (pread/pwrite)
// file IO for every torrent storage. With mmap IO, a seeded file that is
// truncated underneath us (adopted files belong to their owner) turns the
// next read into a SIGBUS that kills the whole daemon; classic IO turns it
// into a read error.
//
// If FileIOEnv is unset it sets it to "classic" and re-execs the running
// binary with the same arguments, so it must be called before anything
// else happens. It returns only when no re-exec was needed (the variable
// was already set — an explicit "mmap" is respected) or the exec failed.
func EnsureClassicFileIO() error {
	if _, ok := os.LookupEnv(FileIOEnv); ok {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("selecting classic file IO: %w", err)
	}
	env := append(os.Environ(), FileIOEnv+"=classic")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		return fmt.Errorf("selecting classic file IO: re-exec %s: %w", exe, err)
	}
	return nil // unreachable
}

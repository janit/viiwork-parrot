package node

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath, failing with an fs.ErrExist
// error if anything at all is at newpath. Unlike rename(2), it never
// replaces an existing (empty) directory or file there. It uses
// renameat2(RENAME_NOREPLACE); on a filesystem without support for it,
// it falls back to lstat-then-rename, which only narrows the race.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameNoReplaceFallback(oldpath, newpath)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

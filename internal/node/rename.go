package node

import (
	"errors"
	"io/fs"
	"os"
)

func renameNoReplaceFallback(oldpath, newpath string) error {
	if _, err := os.Lstat(newpath); err == nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(oldpath, newpath)
}

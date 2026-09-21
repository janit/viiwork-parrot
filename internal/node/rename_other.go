//go:build !linux

package node

// renameNoReplace: see rename_linux.go. Without renameat2 this is
// lstat-then-rename, which only narrows the race.
func renameNoReplace(oldpath, newpath string) error {
	return renameNoReplaceFallback(oldpath, newpath)
}

package mktorrent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// DirOptions describes a folder model's one v1 multi-file torrent.
type DirOptions struct {
	Root        string   // local directory holding Files at their relative paths
	Name        string   // info.name: the HF revision sha
	DisplayName string   // magnet dn (the model id)
	Files       []string // relative slash paths (HF paths); order does not matter
	// WebSeed is the BEP-19 base URL, ending in "/": a client requests
	// WebSeed + <info.name>/<escaped path> for each file, i.e.
	// https://huggingface.co/<repo>/resolve/<revision>/<path>.
	WebSeed  string
	Announce [][]string // nil = DefaultAnnounce
	Comment  string
	// Tee, if set, is called as each file is opened for piece hashing and
	// may return a writer that receives every byte of that file (nil for
	// none), so a caller can compute its own content hashes in the same
	// single read pass.
	Tee func(rel string, size int64) io.Writer
}

// BuildDir builds the multi-file torrent of a folder model. Files are
// sorted by path, so the same files and revision always give the same
// infohash. Every byte is streamed; nothing is read whole into memory.
func BuildDir(o DirOptions) (Result, error) {
	if !strings.HasSuffix(o.WebSeed, "/") {
		return Result{}, errors.New("folder web-seed must end with / (BEP-19 appends <name>/<path>)")
	}
	if o.Name == "" || strings.ContainsAny(o.Name, "/\\") {
		return Result{}, fmt.Errorf("bad info name %q", o.Name)
	}
	if len(o.Files) == 0 {
		return Result{}, errors.New("no files")
	}
	files := slices.Clone(o.Files)
	slices.Sort(files)
	info := metainfo.Info{Name: o.Name}
	for i, rel := range files {
		if i > 0 && files[i-1] == rel {
			return Result{}, fmt.Errorf("%s: listed twice", rel)
		}
		if rel == "" || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) || strings.Contains(rel, "\\") {
			return Result{}, fmt.Errorf("%s: not a clean relative path", rel)
		}
		fi, err := os.Stat(filepath.Join(o.Root, filepath.FromSlash(rel)))
		if err != nil {
			return Result{}, err
		}
		if !fi.Mode().IsRegular() {
			return Result{}, fmt.Errorf("%s: not a regular file", rel)
		}
		info.Files = append(info.Files, metainfo.FileInfo{Path: strings.Split(rel, "/"), Length: fi.Size()})
	}
	info.PieceLength = metainfo.ChoosePieceLength(info.TotalLength())
	err := info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
		rel := strings.Join(fi.Path, "/")
		f, err := os.Open(filepath.Join(o.Root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if o.Tee == nil {
			return f, nil
		}
		w := o.Tee(rel, fi.Length)
		if w == nil {
			return f, nil
		}
		return teeCloser{io.TeeReader(f, w), f}, nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("hashing pieces: %w", err)
	}
	dn := o.DisplayName
	if dn == "" {
		dn = o.Name
	}
	return finish(info, o.Announce, o.WebSeed, o.Comment, dn)
}

type teeCloser struct {
	io.Reader
	io.Closer
}

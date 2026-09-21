package publish

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hfapi"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

// fileCheck is one folder-model file being verified during the single
// streaming read that also computes the torrent's piece hashes.
type fileCheck struct {
	entry  hfapi.Entry
	size   int64
	sha256 hash.Hash
	blob   hash.Hash // git blob sha1 (non-LFS files only)
}

// addDirModel builds a folder model (catalog.LayoutDir): one multi-file
// torrent over the model's files at their HF paths under o.LocalDir (the
// `hf download --local-dir` layout). Every file is checked exactly like a
// per-file model's — local size against HF, then sha256 against lfs.oid for
// LFS files or the git blob sha1 against oid for plain git files — in the
// same single read pass that hashes the torrent pieces; nothing is written
// unless every file matches.
func addDirModel(o Options, m catalog.Model, entries []hfapi.Entry, byPath map[string]hfapi.Entry) (catalog.Model, error) {
	files := o.Files
	if o.All {
		if len(files) > 0 {
			return m, errors.New("give either --all or a file list, not both")
		}
		for _, e := range entries {
			if e.Type == "file" && e.Path != ".gitattributes" {
				files = append(files, e.Path)
			}
		}
	}
	if len(files) == 0 {
		return m, errors.New("no files")
	}
	files = slices.Clone(files)
	slices.Sort(files)
	checks := map[string]*fileCheck{}
	var total int64
	for _, hfPath := range files {
		e, ok := byPath[hfPath]
		if !ok || e.Type != "file" {
			return m, fmt.Errorf("%s: not a file in %s@%s", hfPath, o.Repo, m.Revision)
		}
		if checks[hfPath] != nil {
			return m, fmt.Errorf("%s: listed twice", hfPath)
		}
		fi, err := os.Stat(filepath.Join(o.LocalDir, filepath.FromSlash(hfPath)))
		if err != nil {
			return m, err
		}
		size := e.Size
		if e.LFS != nil {
			size = e.LFS.Size
		}
		if fi.Size() != size {
			return m, fmt.Errorf("%s: local size %d, HF size %d", hfPath, fi.Size(), size)
		}
		checks[hfPath] = &fileCheck{entry: e, size: size}
		total += size
	}
	progress := o.Progress
	if progress == nil {
		progress = io.Discard
	}
	fmt.Fprintf(progress, "hashing %d files, %s, from %s\n", len(files), humanBytes(total), o.LocalDir)
	var n int
	var last string
	start := time.Now()
	r, err := mktorrent.BuildDir(mktorrent.DirOptions{
		Root: o.LocalDir, Name: m.Revision, DisplayName: o.ID, Files: files,
		WebSeed: catalog.DirWebSeed(o.Repo), Announce: o.Announce, Comment: o.Repo + "@" + m.Revision,
		Tee: func(rel string, size int64) io.Writer {
			c := checks[rel]
			n++
			if last != "" {
				fmt.Fprintf(progress, "  done %s (%s elapsed)\n", last, time.Since(start).Round(time.Second))
			}
			last = rel
			fmt.Fprintf(progress, "[%d/%d] %s (%s)\n", n, len(files), rel, humanBytes(size))
			c.sha256 = sha256.New()
			if c.entry.LFS != nil {
				return c.sha256
			}
			c.blob = sha1.New()
			fmt.Fprintf(c.blob, "blob %d\x00", size)
			return io.MultiWriter(c.sha256, c.blob)
		},
	})
	if err != nil {
		return m, err
	}
	fmt.Fprintf(progress, "  done %s (%s elapsed)\n", last, time.Since(start).Round(time.Second))
	var bad []string
	for _, hfPath := range files {
		c := checks[hfPath]
		if c.sha256 == nil {
			return m, fmt.Errorf("%s: never hashed", hfPath)
		}
		sum := hex.EncodeToString(c.sha256.Sum(nil))
		if c.entry.LFS != nil {
			if sum != c.entry.LFS.OID {
				bad = append(bad, fmt.Sprintf("%s: local sha256 %s != HF lfs oid %s", hfPath, sum, c.entry.LFS.OID))
			}
		} else if blob := hex.EncodeToString(c.blob.Sum(nil)); blob != c.entry.OID {
			bad = append(bad, fmt.Sprintf("%s: local git blob sha1 %s != HF oid %s", hfPath, blob, c.entry.OID))
		}
		m.Files = append(m.Files, catalog.File{Name: hfPath, Size: c.size, SHA256: sum})
	}
	if len(bad) > 0 {
		return m, errors.New(strings.Join(bad, "; "))
	}
	m.Layout, m.InfoHash, m.Magnet = catalog.LayoutDir, r.InfoHash, r.Magnet
	if err := catalog.Validate([]catalog.Model{m}); err != nil {
		return m, err
	}
	if err := os.MkdirAll(o.TorrentsDir, 0o755); err != nil {
		return m, err
	}
	if err := mktorrent.Write(filepath.Join(o.TorrentsDir, r.InfoHash+".torrent"), r.MetaInfo); err != nil {
		return m, err
	}
	return m, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

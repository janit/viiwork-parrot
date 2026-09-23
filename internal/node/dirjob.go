package node

// Folder models (catalog layout: dir): one job, one multi-file torrent,
// stored as data_dir/<model id>/<hf path>.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hashcache"
)

// dirUnit is the torrent of folder model m as the catalog.File its job
// serves: Name is the model id (the directory name), Size the total.
func dirUnit(m catalog.Model) catalog.File {
	return catalog.File{Name: m.ID, Size: m.TotalSize(), InfoHash: m.InfoHash, Magnet: m.Magnet}
}

// units is what a model's jobs serve, one per torrent: its files, or its
// one folder torrent.
func units(m catalog.Model) []catalog.File {
	if m.IsDir() {
		return []catalog.File{dirUnit(m)}
	}
	return m.Files
}

// mainInfoHash is the torrent whose path /ensure returns: the folder, or
// the model's main file.
func mainInfoHash(m catalog.Model) string {
	if m.IsDir() {
		return m.InfoHash
	}
	return m.MainFile().InfoHash
}

// checkDirInfo makes sure the torrent's files are exactly the catalog's
// (path and size, same order is not required). The info dict is already
// bound to the signed infohash; this just refuses a catalog entry and a
// torrent that disagree, before anything is written.
func checkDirInfo(mi *metainfo.MetaInfo, m *catalog.Model) error {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return fmt.Errorf("torrent %s: %w", m.InfoHash, err)
	}
	want := make(map[string]int64, len(m.Files))
	for _, f := range m.Files {
		want[f.Name] = f.Size
	}
	if info.BestName() != m.Revision {
		return fmt.Errorf("torrent %s: info name %q is not the catalog revision %s", m.InfoHash, info.BestName(), m.Revision)
	}
	files := info.UpvertedFiles()
	if !info.IsDir() || len(files) != len(want) {
		return fmt.Errorf("torrent %s: has %d files, catalog lists %d", m.InfoHash, len(files), len(want))
	}
	for _, f := range files {
		p := strings.Join(f.BestPath(), "/")
		size, ok := want[p]
		if !ok || size != f.Length {
			return fmt.Errorf("torrent %s: file %q (%d bytes) does not match the catalog", m.InfoHash, p, f.Length)
		}
		// Each catalog path once: with a duplicate, the count check above
		// would pass while some catalog file is missing from the torrent.
		delete(want, p)
	}
	return nil
}

// verifyDir checks that root holds every file of the model at its path
// with the catalog size and sha256 (extra files are ignored: they are not
// part of the torrent). All sizes are checked before anything is hashed.
// mismatch reports that err is a content mismatch (missing file, size or
// sha256), as opposed to a failure to hash or a stop.
func (j *job) verifyDir(root string) (mismatch bool, err error) {
	for _, f := range j.dir.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Name))
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			return true, fmt.Errorf("%s: %s missing (%v)", root, f.Name, err)
		case !fi.Mode().IsRegular():
			return true, fmt.Errorf("%s: %s is not a regular file", root, f.Name)
		case fi.Size() != f.Size:
			return true, fmt.Errorf("%s: %s has size %d, catalog says %d", root, f.Name, fi.Size(), f.Size)
		}
	}
	for _, f := range j.dir.Files {
		if err := j.ctx.Err(); err != nil {
			return false, err
		}
		p := filepath.Join(root, filepath.FromSlash(f.Name))
		sum, err := j.n.hashes.SHA256Context(j.ctx, p)
		if err != nil {
			if sum == "" {
				return false, err
			}
			j.n.log.Warn("hash cache", "path", p, "err", err)
		}
		if sum != f.SHA256 {
			return true, fmt.Errorf("%s: %s has sha256 %s, catalog says %s", root, f.Name, sum, f.SHA256)
		}
	}
	return false, nil
}

// findLocalDir is findLocal for a folder model: data_dir/<id> first, then
// every adoption candidate directory. The first directory holding every
// catalog file (path, size, sha256) is seeded in place; further full
// matches that are not the same directory are duplicates. A partial match
// is never adopted. Something at data_dir/<id> that is not a matching
// directory is an error and left untouched — unless it is viiwork-parrot's
// own unchanged download of an older revision (or of a per-file model's
// file that used this name in an older catalog), which the new download may
// replace once verified.
func (j *job) findLocalDir() (string, []string, error) {
	var found string
	var dups []string
	var kept []os.FileInfo
	sameAsKept := func(fi os.FileInfo) bool {
		for _, k := range kept {
			if os.SameFile(fi, k) {
				return true
			}
		}
		return false
	}
	dp := j.dataPath()
	fi, err := statDirPath(dp)
	if err != nil {
		// viiwork-parrot's own unchanged download of a per-file model's file
		// that used this name in an older catalog may be replaced.
		old, ok := j.n.staleOwnDownload(dp, j.f.InfoHash)
		if !ok {
			return "", nil, err
		}
		j.n.log.Info("data path holds a file viiwork-parrot downloaded for an older catalog; replacing it with the folder once verified", "path", dp, "old_infohash", old)
		j.mu.Lock()
		j.replaceIH = old
		j.mu.Unlock()
		fi = nil
	}
	if fi != nil {
		mismatch, err := j.verifyDir(dp)
		switch {
		case err == nil:
			found = dp
			kept = append(kept, fi)
			j.claimRecord(dp)
		case !mismatch:
			return "", nil, err
		default:
			old, ok := j.n.staleOwnDownload(dp, j.f.InfoHash)
			if !ok {
				return "", nil, fmt.Errorf("%v; left untouched", err)
			}
			j.n.log.Info("model directory holds an older revision viiwork-parrot downloaded; replacing it once the new one is verified", "path", dp, "old_infohash", old)
			j.mu.Lock()
			j.replaceIH = old
			j.mu.Unlock()
		}
	}
	for _, p := range j.adopt {
		if j.ctx.Err() != nil {
			return "", nil, j.ctx.Err()
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() || sameAsKept(fi) {
			continue
		}
		mismatch, err := j.verifyDir(p)
		if err != nil {
			if !mismatch && j.ctx.Err() == nil {
				j.n.log.Warn("adopt candidate", "path", p, "err", err)
			}
			continue
		}
		kept = append(kept, fi)
		if found == "" {
			found = p
		} else {
			dups = append(dups, p)
		}
	}
	return found, dups, nil
}

// statDirPath looks at data_dir/<id>: (nil, nil) when nothing is there; the
// directory (or a symlink resolving to one) otherwise. Anything else is
// someone else's entry, an error, and left untouched.
func statDirPath(dp string) (os.FileInfo, error) {
	lfi, err := os.Lstat(dp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w; left untouched", dp, err)
	}
	if lfi.IsDir() {
		return lfi, nil
	}
	if lfi.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("%s exists and is not a directory (%s); left untouched", dp, lfi.Mode().Type())
	}
	fi, err := os.Stat(dp)
	if err != nil {
		return nil, fmt.Errorf("%s is a symlink that does not resolve (%v); left untouched", dp, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is a symlink to a non-directory; left untouched", dp)
	}
	return fi, nil
}

// incomingOwner is the marker file recording which torrent (infohash)
// data_dir/.incoming/<id> belongs to.
func (j *job) incomingOwner() string {
	return filepath.Join(j.incomingDir(), "."+j.f.DiskName()+".infohash")
}

// prepareIncomingDir makes .incoming/<id> this torrent's: whatever is there
// from another torrent — an older revision's partial download, or a
// per-file download that once used the same name — is removed (it is our
// own .incoming, and anacrolix could not open the torrent over a file or a
// conflicting directory, while the space reservation would miscount it). A
// partial download of this same infohash is kept, to be resumed.
func (j *job) prepareIncomingDir() error {
	owner := j.incomingOwner()
	if b, err := os.ReadFile(owner); err == nil && strings.TrimSpace(string(b)) == j.f.InfoHash {
		return nil
	}
	if _, err := os.Lstat(j.incomingPath()); err == nil {
		j.n.log.Info("removing a stale partial download from another torrent", "path", j.incomingPath())
		if err := os.RemoveAll(j.incomingPath()); err != nil {
			return fmt.Errorf("removing stale %s: %w", j.incomingPath(), err)
		}
	}
	if err := os.MkdirAll(j.incomingDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(owner, []byte(j.f.InfoHash+"\n"), 0o644)
}

// finishDir verifies a completed folder download in .incoming/<id> (every
// file's sha256), then promotes the whole directory to data_dir/<id>,
// records it and seeds it. A mismatch quarantines the directory.
func (j *job) finishDir(pieces int) {
	incoming := j.incomingPath()
	want := make(map[string]bool, len(j.dir.Files))
	for _, f := range j.dir.Files {
		want[f.Name] = true
	}
	// Files an earlier, abandoned revision left in our .incoming/<id> are
	// not part of this torrent: drop them so they are never promoted.
	if err := pruneExtras(incoming, want); err != nil {
		j.fail(err)
		return
	}
	sums := make(map[string]string, len(j.dir.Files))
	for _, f := range j.dir.Files {
		p := filepath.Join(incoming, filepath.FromSlash(f.Name))
		sum, err := hashcache.HashFileContext(j.ctx, p)
		if err != nil {
			j.fail(err)
			return
		}
		if j.ctx.Err() != nil {
			return // stopping: don't quarantine or promote a download we're abandoning
		}
		if sum != f.SHA256 {
			j.quarantineDir(f, sum, pieces)
			return
		}
		sums[f.Name] = sum
	}
	final := j.dataPath()
	if err := j.promoteDir(incoming, final); err != nil {
		j.fail(err)
		return
	}
	os.Remove(j.incomingOwner())
	j.mu.Lock()
	j.downloaded = true
	j.mu.Unlock()
	names := make([]string, 0, len(j.dir.Files))
	for _, f := range j.dir.Files {
		names = append(names, f.Name)
		j.n.hashes.Put(filepath.Join(final, filepath.FromSlash(f.Name)), sums[f.Name])
	}
	if rec, err := recordDir(final, names); err != nil {
		j.n.log.Warn("record downloaded model directory", "path", final, "err", err)
	} else if err := j.n.dl.Put(j.f.InfoHash, rec); err != nil {
		j.n.log.Warn("record downloaded model directory", "path", final, "err", err)
	}
	j.seed(final)
}

// pruneExtras removes regular files under root (our own .incoming
// directory) that are not in want, and then directories left empty.
func pruneExtras(root string, want map[string]bool) error {
	dirs := map[string]bool{}
	var firstErr error
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		if d.IsDir() {
			if p != root {
				dirs[p] = true
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !want[filepath.ToSlash(rel)] {
			if err := os.Remove(p); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return nil
	})
	if firstErr != nil {
		return firstErr
	}
	return removeEmptyDirs(dirs)
}

// promoteDir moves a verified folder download to data_dir/<id> with
// no-replace semantics: if anything at all appeared there meanwhile, it is
// left untouched, the job fails, and the verified download stays in
// .incoming.
//
// The one exception is viiwork-parrot's own unchanged download of an older
// revision found there by findLocalDir (j.replaceIH): re-checked (still
// recorded, still out of the catalog, still exactly as downloaded), it is
// moved aside into .incoming, the new directory is moved in, and only then
// is the old one deleted (and its record dropped). If the new one cannot be
// moved in, the old one is moved back.
func (j *job) promoteDir(incoming, final string) error {
	j.mu.Lock()
	old := j.replaceIH
	j.mu.Unlock()
	var aside string
	if old != "" {
		if cur, ok := j.n.staleOwnDownload(final, j.f.InfoHash); ok && cur == old {
			aside = filepath.Join(j.incomingDir(), fmt.Sprintf(".%s%s%d", j.f.DiskName(), replacedMarker, time.Now().UnixNano()))
			if err := renameNoReplace(final, aside); err != nil {
				return fmt.Errorf("moving older revision at %s aside: %w", final, err)
			}
		}
	}
	if testHookBeforeDirPromote != nil {
		testHookBeforeDirPromote(incoming, final)
	}
	if err := renameNoReplace(incoming, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s exists; left untouched (verified download kept in %s)", final, incoming)
		}
		if aside != "" {
			// Put the older revision back; its record was kept, so it is
			// still recognized (and replaceable) as viiwork-parrot's own.
			if berr := renameNoReplace(aside, final); berr != nil {
				err = fmt.Errorf("%w; the older revision could not be moved back from %s: %v", err, aside, berr)
			}
		}
		return err
	}
	if aside != "" {
		j.n.dl.Delete(old)
		if err := os.RemoveAll(aside); err != nil {
			j.n.log.Warn("removing replaced older revision", "path", aside, "err", err)
		}
	}
	return nil
}

// replacedMarker names an older folder revision moved aside inside
// .incoming during promoteDir (".<id>.replaced.<ns>"). Only promoteDir
// creates such entries, so any left over (a crash between the two renames)
// are removed at startup by cleanReplaced.
const replacedMarker = ".replaced."

// testHookBeforeDirPromote, if set (tests only), runs between moving an
// older revision aside and moving the new download into place.
var testHookBeforeDirPromote func(incoming, final string)

// cleanReplaced removes older folder revisions that promoteDir moved aside
// but never got to delete (a crash between its renames). They live in
// data_dir/.incoming and are viiwork-parrot's by construction.
func cleanReplaced(dataDir string, log interface{ Warn(string, ...any) }) {
	dir := filepath.Join(dataDir, ".incoming")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), replacedMarker) {
			p := filepath.Join(dir, e.Name())
			if err := os.RemoveAll(p); err != nil {
				log.Warn("removing leftover replaced folder revision", "path", p, "err", err)
			}
		}
	}
}

func (j *job) quarantineDir(f catalog.File, got string, pieces int) {
	base := fmt.Errorf("sha256 mismatch: downloaded %s/%s has sha256 %s, catalog says %s", j.f.DiskName(), f.Name, got, f.SHA256)
	q := filepath.Join(j.n.cfg.Node.DataDir, ".quarantine")
	if err := os.MkdirAll(q, 0o755); err != nil {
		j.fail(fmt.Errorf("%w; quarantine failed: %v", base, err))
		return
	}
	dst, err := quarantineTo(j.incomingPath(), filepath.Join(q, fmt.Sprintf("%s.%s", j.f.DiskName(), got[:12])))
	if err != nil {
		j.fail(fmt.Errorf("%w; quarantine failed: %v", base, err))
		return
	}
	os.Remove(j.incomingOwner())
	if err := j.forgetPieces(pieces); err != nil {
		j.fail(fmt.Errorf("%w; moved to %s, but forgetting piece completion failed: %v", base, dst, err))
		return
	}
	j.fail(fmt.Errorf("%w; moved to %s", base, dst))
}

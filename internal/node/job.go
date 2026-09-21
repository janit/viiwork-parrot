package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hashcache"
)

type State string

const (
	StateAbsent      State = "absent"
	StateQueued      State = "queued"
	StateAdopting    State = "adopting"
	StateDownloading State = "downloading"
	StateVerifying   State = "verifying"
	StateSeeding     State = "seeding"
	StatePaused      State = "paused"
	StateFailed      State = "failed"
)

const spaceMargin = 1 << 30

type job struct {
	n     *Node
	model string
	// f is the torrent this job serves: a catalog file, or for a folder
	// model (dir != nil) the model's one torrent as a File (Name = model id,
	// so DiskName() is the directory name; Size = total; no SHA256).
	f      catalog.File
	dir    *catalog.Model
	adopt  []string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}     // closed when run() returns, after cleanup
	waits  []<-chan struct{} // predecessor jobs' done channels to wait out before touching anything (nil entries ignored)

	mu         sync.Mutex
	state      State
	path       string
	err        string
	noSpace    bool
	t          *torrent.Torrent
	downloaded bool
	prune      bool
	dups       []string         // further verified copies on this host (never deleted)
	maxConns   int              // last applied SetMaxEstablishedConns (Task 16)
	uploadOK   bool             // last applied upload permission (Task 16)
	appliedTo  *torrent.Torrent // the *torrent.Torrent maxConns/uploadOK were last applied to (Task 16)
	replaceIH  string           // infohash of viiwork-parrot's own stale download at dataPath() that the promote may replace
}

func newJob(n *Node, model string, f catalog.File, adopt []string, waits ...chan struct{}) *job {
	ctx, cancel := context.WithCancel(n.ctx)
	j := &job{n: n, model: model, f: f, adopt: adopt, ctx: ctx, cancel: cancel, done: make(chan struct{}), state: StateQueued, uploadOK: true}
	for _, w := range waits {
		if w != nil {
			j.waits = append(j.waits, w)
		}
	}
	return j
}

func (j *job) dataPath() string { return filepath.Join(j.n.cfg.Node.DataDir, j.f.DiskName()) }
func (j *job) incomingDir() string {
	return filepath.Join(j.n.cfg.Node.DataDir, ".incoming")
}
func (j *job) incomingPath() string { return filepath.Join(j.incomingDir(), j.f.DiskName()) }

func (j *job) set(s State, path, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state, j.err = s, msg
	if path != "" {
		j.path = path
	}
}

func (j *job) fail(err error) {
	if j.ctx.Err() != nil {
		return // stopping, not failing
	}
	j.n.log.Error("model file failed", "model", j.model, "file", j.f.DiskName(), "err", err)
	j.set(StateFailed, "", err.Error())
}

func (j *job) stop(prune bool) {
	j.mu.Lock()
	j.prune = prune
	j.mu.Unlock()
	j.cancel()
}

func (j *job) failed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == StateFailed
}

func (j *job) torrent() *torrent.Torrent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.t
}

func (j *job) run() {
	defer close(j.done)
	defer j.cleanup()
	for _, w := range j.waits {
		// Always let the predecessors finish first, even if we're already
		// being stopped ourselves: our own done must imply every
		// predecessor is done too, or a job further down the chain could
		// start (and touch the shared incoming/data paths, or the shared
		// torrent) while an earlier one is still active.
		<-w
	}
	if j.ctx.Err() != nil {
		return
	}
	j.set(StateAdopting, "", "")
	find := j.findLocal
	if j.dir != nil {
		find = j.findLocalDir
	}
	p, dups, err := find()
	if len(dups) > 0 {
		j.n.log.Warn("model file stored more than once on this host", "model", j.model, "file", j.f.DiskName(), "seeding", p, "duplicates", dups)
		j.mu.Lock()
		j.dups = dups
		j.mu.Unlock()
	}
	switch {
	case err != nil:
		j.fail(err)
	case p != "":
		j.seed(p)
	case j.n.cfg.Models.SeedOnlyExisting:
		j.set(StateAbsent, "", "no local copy and models.seed_only_existing is set, so it is never downloaded")
	default:
		j.download()
	}
}

func (j *job) cleanup() {
	j.mu.Lock()
	t, prune, downloaded := j.t, j.prune, j.downloaded
	j.t = nil
	j.mu.Unlock()
	if t != nil {
		t.Drop()
	}
	if prune {
		if j.dir != nil {
			os.RemoveAll(j.incomingPath()) // our own .incoming/<id>/
			os.Remove(j.incomingOwner())
		} else {
			os.Remove(j.incomingPath())
		}
		if downloaded {
			j.pruneDataPathIfUnchanged()
		}
	}
}

// pruneDataPathIfUnchanged removes j.dataPath() only if it still matches
// what viiwork-parrot recorded downloading there (same size and mtime). If the
// user has replaced the file, or it already vanished, the record is
// forgotten but the file itself is left alone.
func (j *job) pruneDataPathIfUnchanged() {
	rec, ok := j.n.dl.Get(j.f.InfoHash)
	if !ok {
		return
	}
	missing, match := statRecorded(rec)
	if missing {
		j.n.dl.Delete(j.f.InfoHash)
		return
	}
	if !match {
		j.n.log.Warn("prune: file no longer matches what viiwork-parrot downloaded (replaced?); leaving it and forgetting the record", "path", rec.Path)
		j.n.dl.Delete(j.f.InfoHash)
		return
	}
	if err := removeRecorded(rec); err == nil {
		j.n.dl.Delete(j.f.InfoHash)
	}
}

// findLocal returns a verified existing copy (data_dir first, then adoption
// candidates) and the other verified copies of the same content that are
// not the same inode: duplicates on disk. Mismatching files in data_dir are
// an error and left untouched.
func (j *job) findLocal() (string, []string, error) {
	var found string
	var dups []string
	var kept []os.FileInfo // inodes already accounted for
	sameAsKept := func(fi os.FileInfo) bool {
		for _, k := range kept {
			if os.SameFile(fi, k) {
				return true
			}
		}
		return false
	}
	dp := j.dataPath()
	fi, err := j.statDataPath(dp)
	if err != nil {
		return "", nil, err
	}
	if fi != nil {
		mismatch, err := j.verifyDataPath(dp, fi)
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
				return "", nil, err
			}
			// viiwork-parrot's own unchanged download of a revision that left the
			// catalog: a new download may replace it, once verified.
			j.n.log.Info("data path holds an older revision viiwork-parrot downloaded; replacing it once the new one is verified", "path", dp, "old_infohash", old)
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
		if err != nil || sameAsKept(fi) {
			continue
		}
		sum, err := j.n.hashes.SHA256(p)
		if err != nil {
			if sum == "" {
				j.n.log.Warn("adopt candidate", "path", p, "err", err)
				continue
			}
			j.n.log.Warn("hash cache", "path", p, "err", err)
		}
		if sum != j.f.SHA256 {
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

// verifyDataPath checks the regular file at dp against the catalog entry.
// mismatch reports that err is a content mismatch (size or sha256), as
// opposed to a failure to hash.
func (j *job) verifyDataPath(dp string, fi os.FileInfo) (mismatch bool, err error) {
	if fi.Size() != j.f.Size {
		return true, fmt.Errorf("%s exists with size %d, catalog says %d; left untouched", dp, fi.Size(), j.f.Size)
	}
	sum, err := j.n.hashes.SHA256(dp)
	if err != nil {
		if sum == "" {
			return false, err
		}
		// The hash itself is good; only persisting it to the cache failed.
		j.n.log.Warn("hash cache", "path", dp, "err", err)
	}
	if sum != j.f.SHA256 {
		return true, fmt.Errorf("%s exists but sha256 %s != catalog %s; left untouched", dp, sum, j.f.SHA256)
	}
	return false, nil
}

// claimRecord marks the verified file at dp as viiwork-parrot's own download if
// it is recorded as one — under this infohash, or under an older revision
// that left the catalog with the same content (the record then moves to
// this infohash, so a later prune sees it as in use).
func (j *job) claimRecord(dp string) {
	rec, ok := j.n.dl.Get(j.f.InfoHash)
	if !ok || rec.Path != dp {
		old, stale := j.n.staleOwnDownload(dp, j.f.InfoHash)
		if !stale {
			return
		}
		rec, _ = j.n.dl.Get(old)
		if err := j.n.dl.Put(j.f.InfoHash, rec); err != nil {
			j.n.log.Warn("record downloaded file", "path", dp, "err", err)
			return
		}
		j.n.dl.Delete(old)
	}
	j.mu.Lock()
	j.downloaded = true
	j.mu.Unlock()
}

// statDataPath looks at data_dir/<disk name> without following it blindly:
// (nil, nil) only when nothing at all is there. A regular file, or a
// symlink resolving to one, is returned for verification (a symlink to a
// verified copy is adopted as before). Anything else — a dangling symlink,
// a directory, a device — is someone else's entry that a download would
// replace, so it's an error and left untouched.
func (j *job) statDataPath(dp string) (os.FileInfo, error) {
	lfi, err := os.Lstat(dp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w; left untouched", dp, err)
	}
	if lfi.Mode().IsRegular() {
		return lfi, nil
	}
	if lfi.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("%s exists and is not a regular file (%s); left untouched", dp, lfi.Mode().Type())
	}
	fi, err := os.Stat(dp)
	if err != nil {
		return nil, fmt.Errorf("%s is a symlink that does not resolve (%v); left untouched", dp, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a symlink to a non-regular file (%s); left untouched", dp, fi.Mode().Type())
	}
	return fi, nil
}

func (j *job) fetchMetainfo() (*metainfo.MetaInfo, error) {
	delay := 5 * time.Second
	for {
		mi, err := j.n.src.Torrent(j.ctx, j.f.InfoHash)
		if err == nil {
			return mi, nil
		}
		if j.ctx.Err() != nil {
			return nil, j.ctx.Err()
		}
		j.set(StateQueued, "", "fetching torrent: "+err.Error())
		select {
		case <-time.After(delay):
		case <-j.ctx.Done():
			return nil, j.ctx.Err()
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

func (j *job) addTorrent(st storage.ClientImpl, download bool) (*torrent.Torrent, error) {
	// Trackers and web-seed come from the signed catalog's magnet. The
	// fetched .torrent is only trusted for its info dict (its hash is the
	// signed infohash); its top-level announce/url-list are not signed and
	// are ignored.
	trackers, webSeeds, err := j.f.MagnetSources()
	if err != nil {
		return nil, err
	}
	mi, err := j.fetchMetainfo()
	if err != nil {
		return nil, err
	}
	if j.dir != nil {
		if err := checkDirInfo(mi, j.dir); err != nil {
			return nil, err
		}
	}
	t, _ := j.n.sw.cl.AddTorrentOpt(torrent.AddTorrentOpts{
		InfoHash: mi.HashInfoBytes(), InfoBytes: mi.InfoBytes, Storage: st,
		DisallowDataDownload: !download,
	})
	if t.Info() == nil {
		// anacrolix drops the error when the storage can't open the torrent
		// (e.g. a directory where a file must go) and leaves it without
		// info; DownloadAll on it would panic the whole daemon.
		t.Drop()
		return nil, errors.New("could not open the torrent's storage (see the log)")
	}
	// One tier per tracker, the layout mktorrent publishes.
	tiers := make([][]string, 0, len(trackers))
	for _, tr := range trackers {
		tiers = append(tiers, []string{tr})
	}
	t.AddTrackers(tiers)
	if download && len(webSeeds) > 0 {
		// Throttling happens in the client's WebTransport (see swarm.go),
		// not via WebSeedResponseBodyRateLimiter: anacrolix's own body rate
		// limiter sleeps out its reservation delay uncancellably, which can
		// still be in flight after this torrent's Drop() and race the
		// client's internal webseed-request bookkeeping. See
		// throttle.Transport / throttle.WrapBody.
		t.AddWebSeeds(webSeeds)
	}
	j.n.mu.Lock()
	peers := j.n.peers
	j.n.mu.Unlock()
	if len(peers) > 0 {
		t.AddPeers(peers)
	}
	j.mu.Lock()
	j.t = t
	j.mu.Unlock()
	return t, nil
}

// seed serves path in place until the job stops. It never downloads into it.
func (j *job) seed(path string) {
	t, err := j.addTorrent(j.storageAt(path), false)
	if err != nil {
		j.fail(err)
		return
	}
	j.set(StateAdopting, path, "")
	select {
	case <-t.Complete().On():
	case <-j.ctx.Done():
		return
	}
	j.set(StateSeeding, path, "")
	<-j.ctx.Done()
}

func (j *job) download() {
	ih := j.f.InfoHash
	if j.dir != nil {
		if err := j.prepareIncomingDir(); err != nil {
			j.fail(err)
			return
		}
	}
	for {
		ok, reason, freed := j.n.reserveSpace(ih, j.incomingPath(), j.f.Size)
		if ok {
			break
		}
		j.mu.Lock()
		j.noSpace = true
		j.mu.Unlock()
		j.set(StatePaused, "", reason)
		select {
		case <-freed: // another download finished or stopped: re-check now
		case <-time.After(time.Minute):
		case <-j.ctx.Done():
			return
		}
	}
	defer j.n.releaseSpace(ih) // stop/failure; a completed download releases below
	j.mu.Lock()
	j.noSpace = false
	j.mu.Unlock()
	if err := os.MkdirAll(j.incomingDir(), 0o755); err != nil {
		j.fail(err)
		return
	}
	t, err := j.addTorrent(j.storageAt(j.incomingPath()), true)
	if err != nil {
		j.fail(err)
		return
	}
	j.set(StateDownloading, "", "")
	t.DownloadAll()
	select {
	case <-t.Complete().On():
	case <-j.ctx.Done():
		return
	}
	j.n.releaseSpace(ih) // every byte is on disk now
	j.set(StateVerifying, "", "")
	pieces := t.NumPieces()
	t.Drop()
	j.mu.Lock()
	j.t = nil
	j.mu.Unlock()
	if j.dir != nil {
		j.finishDir(pieces)
		return
	}
	sum, err := hashcache.HashFile(j.incomingPath())
	if err != nil {
		j.fail(err)
		return
	}
	if j.ctx.Err() != nil {
		return // stopping: don't quarantine or promote a download we're abandoning
	}
	if sum != j.f.SHA256 {
		j.quarantine(sum, pieces)
		return
	}
	final := j.dataPath()
	if err := j.promote(j.incomingPath(), final); err != nil {
		j.fail(err)
		return
	}
	j.mu.Lock()
	j.downloaded = true
	j.mu.Unlock()
	j.n.hashes.Put(final, sum)
	if fi, err := os.Stat(final); err != nil {
		j.n.log.Warn("record downloaded file", "path", final, "err", err)
	} else if err := j.n.dl.Put(j.f.InfoHash, downloadedFile{Path: final, Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}); err != nil {
		j.n.log.Warn("record downloaded file", "path", final, "err", err)
	}
	j.seed(final)
}

// promote moves a verified download from .incoming to its final path
// without ever replacing anything there: link(2) fails with EEXIST if the
// path is taken (a file, or even a dangling symlink, that appeared while we
// downloaded), unlike rename(2), which would silently overwrite it. On that
// failure the download stays in .incoming.
//
// The one exception is viiwork-parrot's own download of an older revision found
// there by findLocal (j.replaceIH): it is re-checked (still recorded, still
// out of the catalog, still unchanged) and removed immediately before the
// link, so data_dir/<disk> is missing only for the instant between the
// unlink and the link.
func (j *job) promote(incoming, final string) error {
	j.mu.Lock()
	old := j.replaceIH
	j.mu.Unlock()
	if old != "" {
		if cur, ok := j.n.staleOwnDownload(final, j.f.InfoHash); ok && cur == old {
			if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("replacing older revision at %s: %w", final, err)
			}
			j.n.dl.Delete(old)
		}
	}
	if err := os.Link(incoming, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s exists; left untouched (verified download kept in %s)", final, incoming)
		}
		return err
	}
	return os.Remove(incoming)
}

func (j *job) quarantine(got string, pieces int) {
	base := fmt.Errorf("sha256 mismatch: downloaded %s has sha256 %s, catalog says %s", j.f.DiskName(), got, j.f.SHA256)
	q := filepath.Join(j.n.cfg.Node.DataDir, ".quarantine")
	if err := os.MkdirAll(q, 0o755); err != nil {
		j.fail(fmt.Errorf("%w; quarantine failed: %v", base, err))
		return
	}
	dst := filepath.Join(q, j.f.DiskName()+"."+got[:12])
	if err := os.Rename(j.incomingPath(), dst); err != nil {
		j.fail(fmt.Errorf("%w; quarantine failed: %v", base, err))
		return
	}
	if err := j.forgetPieces(pieces); err != nil {
		j.fail(fmt.Errorf("%w; moved to %s, but forgetting piece completion failed: %v", base, dst, err))
		return
	}
	j.fail(fmt.Errorf("%w; moved to %s", base, dst))
}

// forgetPieces clears the recorded completion of this torrent's pieces, so
// a later retry downloads them again instead of trusting data that was
// moved away.
func (j *job) forgetPieces(pieces int) error {
	var ih metainfo.Hash
	if err := ih.FromHexString(j.f.InfoHash); err != nil {
		return err
	}
	for i := 0; i < pieces; i++ {
		if err := j.n.sw.pc.Set(metainfo.PieceKey{InfoHash: ih, Index: i}, g.None[bool]()); err != nil {
			return err
		}
	}
	return nil
}

// storageAt is the torrent storage rooted at path: the file itself for a
// per-file job, the model directory for a folder model.
func (j *job) storageAt(path string) storage.ClientImpl {
	if j.dir != nil {
		return dirStorage(j.n.sw.pc, path)
	}
	return fileStorage(j.n.sw.pc, filepath.Dir(path), filepath.Base(path))
}

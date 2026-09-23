package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/config"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/throttle"
)

var (
	ErrUnknownModel = errors.New("unknown model")
	ErrNoCatalog    = errors.New("no catalog loaded yet")
	ErrNoSpace      = errors.New("insufficient disk space")
)

type TorrentSource interface {
	Torrent(ctx context.Context, infohash string) (*metainfo.MetaInfo, error)
}

type Options struct {
	Config *config.Config
	Source TorrentSource
	Policy *throttle.Policy
	Logger *slog.Logger
}

type Node struct {
	cfg    *config.Config
	src    TorrentSource
	pol    *throttle.Policy
	log    *slog.Logger
	sw     *swarm
	hashes *hashcache.Cache
	now    func() time.Time
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	dl *downloadRecord

	startOnce sync.Once
	// applyMu serializes applyLimits end to end (schedule ticks vs.
	// SetOverride/ClearOverride), so an override can never be reverted by a
	// concurrently-running tick and the per-job/rate caches are never
	// updated by two interleaved runs. It is a lock distinct from mu/j.mu:
	// nothing else acquires it, so holding it across an applyLimits call
	// (anacrolix calls included) can't deadlock with anything.
	applyMu sync.Mutex

	mu        sync.Mutex
	closed    bool // set once by Close; nothing new is started after it
	cat       *catalog.Catalog
	extraWant map[string]bool
	jobs      map[string]*job          // by infohash
	stopping  map[string]chan struct{} // by infohash: a just-stopped job's done channel, until it fires
	// stoppingDisk is the same, by disk name: a new revision of a file
	// (new infohash, same data_dir name) waits for the old one's job.
	stoppingDisk map[string]chan struct{}
	peers        []torrent.PeerInfo
	lim          limitsState
	// reserved: disk space held by admitted downloads (by infohash), so
	// parallel downloads never overcommit the free space between them.
	reserved   map[string]reservation
	spaceFreed chan struct{} // closed (and replaced) whenever a reservation is released
}

type reservation struct {
	path string // the .incoming file being written
	size int64
}

// outstanding is what the download still has to write: its size minus the
// blocks already allocated to its file (which the free-space figure already
// accounts for).
func (r reservation) outstanding() int64 {
	return max(0, r.size-allocatedBytes(r.path))
}

// reserveSpace admits a download of size bytes into path if the free space
// minus every other admitted download's outstanding bytes still leaves its
// own outstanding bytes plus a 1GiB margin. Admission and reservation are
// one step under n.mu. If it doesn't fit, it returns a reason and a channel
// that is closed when some reservation is released.
func (n *Node) reserveSpace(ih, path string, size int64) (ok bool, reason string, freed <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := reservation{path: path, size: size}
	free, err := diskFree(n.cfg.Node.DataDir)
	if err != nil {
		n.log.Warn("free space unknown; not reserving", "dir", n.cfg.Node.DataDir, "err", err)
		n.reserved[ih] = r
		return true, "", nil
	}
	var others int64
	for k, o := range n.reserved {
		if k != ih {
			others += o.outstanding()
		}
	}
	need := r.outstanding()
	if int64(free)-others >= need+spaceMargin {
		n.reserved[ih] = r
		return true, "", nil
	}
	return false, fmt.Sprintf("%v: need %d bytes plus 1GiB margin, %d free, %d reserved by other downloads", ErrNoSpace, need, free, others), n.spaceFreed
}

// staleOwnDownload finds viiwork-parrot's own download at path of a revision
// that is gone: a downloaded.json record for path under an infohash other
// than exceptIH that is no longer in the catalog, with the file unchanged
// since (size+mtime). Such a file may be replaced by the new revision.
func (n *Node) staleOwnDownload(path, exceptIH string) (string, bool) {
	n.mu.Lock()
	inCatalog := map[string]bool{}
	if n.cat != nil {
		for _, m := range n.cat.Models {
			for _, f := range units(m) {
				inCatalog[f.InfoHash] = true
			}
		}
	}
	n.mu.Unlock()
	for ih, rec := range n.dl.All() {
		if ih == exceptIH || rec.Path != path || inCatalog[ih] {
			continue
		}
		if _, match := statRecorded(rec); match {
			return ih, true
		}
	}
	return "", false
}

func (n *Node) releaseSpace(ih string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.reserved[ih]; !ok {
		return
	}
	delete(n.reserved, ih)
	close(n.spaceFreed)
	n.spaceFreed = make(chan struct{})
}

func New(o Options) (*Node, error) {
	sw, err := newSwarm(o.Config, o.Policy, o.Logger)
	if err != nil {
		return nil, err
	}
	hc, err := hashcache.Open(filepath.Join(o.Config.Node.StateDir, "hashes.json"))
	if err != nil {
		sw.Close()
		return nil, err
	}
	dl, err := openDownloadRecord(filepath.Join(o.Config.Node.StateDir, "downloaded.json"))
	if err != nil {
		sw.Close()
		return nil, err
	}
	cleanReplaced(o.Config.Node.DataDir, o.Logger)
	ctx, cancel := context.WithCancel(context.Background())
	return &Node{
		cfg: o.Config, src: o.Source, pol: o.Policy, log: o.Logger, sw: sw, hashes: hc, dl: dl,
		now: time.Now, ctx: ctx, cancel: cancel,
		extraWant: map[string]bool{}, jobs: map[string]*job{}, stopping: map[string]chan struct{}{}, stoppingDisk: map[string]chan struct{}{},
		lim:      limitsState{rule: -1, rates: map[string]rateSample{}},
		reserved: map[string]reservation{}, spaceFreed: make(chan struct{}),
	}, nil
}

func (n *Node) Port() int { return n.sw.Port() }

// Close stops every job and the swarm. It is idempotent. Once it has
// started, SetCatalog/Ensure/Start no longer launch anything, so no
// goroutine is ever added to n.wg while (or after) it is being waited on.
func (n *Node) Close() {
	n.mu.Lock()
	already := n.closed
	n.closed = true
	n.mu.Unlock()
	if already {
		return
	}
	n.cancel()
	n.wg.Wait()
	n.sw.Close()
}

func (n *Node) SetCatalog(c *catalog.Catalog) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cat = c
	n.reconcileScanLocked(true)
}

// reconcileScanLocked is reconcileLocked, with the adoption scan (a
// filesystem walk of every adopt root, possibly slow) done without n.mu
// held, so /status, /ensure and the limits loop don't stall behind it.
// Every step of reconcileLocked is idempotent, so running it again on
// whatever changed meanwhile is safe. n.mu is held on entry and on return.
func (n *Node) reconcileScanLocked(retryFailed bool) {
	if !n.reconcileLocked(retryFailed, nil) {
		return
	}
	n.mu.Unlock()
	idx := n.scanAdopt()
	n.mu.Lock()
	n.reconcileLocked(retryFailed, idx)
}

func (n *Node) wantedLocked(id string) bool {
	if n.extraWant[id] {
		return true
	}
	for _, w := range n.cfg.Models.Want {
		if w == "*" || w == id {
			return true
		}
	}
	return false
}

// retireLocked stops j and remembers its done channel by infohash and by
// disk name until it fires, so a successor for the same infohash — or for a
// new revision stored under the same disk name — waits it out before
// touching the same paths.
func (n *Node) retireLocked(ih string, j *job, prune bool) {
	j.stop(prune)
	disk := j.f.DiskName()
	n.stopping[ih] = j.done
	n.stoppingDisk[disk] = j.done
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		<-j.done
		n.mu.Lock()
		if n.stopping[ih] == j.done {
			delete(n.stopping, ih)
		}
		if n.stoppingDisk[disk] == j.done {
			delete(n.stoppingDisk, disk)
		}
		n.mu.Unlock()
	}()
	delete(n.jobs, ih)
}

// reconcileLocked starts/stops jobs to match the catalog and want list.
// retryFailed (catalog updates) also restarts failed jobs that are still
// wanted. New jobs need the adoption index: with adoptIdx nil and a job to
// start, it starts none and reports true so the caller can scan without
// n.mu held and call again.
func (n *Node) reconcileLocked(retryFailed bool, adoptIdx *adoptIndex) (needScan bool) {
	if n.cat == nil || n.closed {
		return false
	}
	type unit struct {
		model string
		f     catalog.File
	}
	keep := map[string]unit{}
	inCatalog := map[string]bool{}
	for _, m := range n.cat.Models {
		for _, f := range units(m) {
			inCatalog[f.InfoHash] = true
		}
		if !n.wantedLocked(m.ID) {
			continue
		}
		for _, f := range units(m) {
			keep[f.InfoHash] = unit{m.ID, f}
		}
	}
	for ih, j := range n.jobs {
		u, kept := keep[ih]
		switch {
		case !kept:
			n.retireLocked(ih, j, n.cfg.Models.Prune && !inCatalog[ih])
		case u.model != j.model || u.f != j.f:
			// Same torrent, new catalog entry (disk name, trackers,
			// web-seed or owning model changed): restart it (below) on the
			// new entry, or it would keep writing to its old disk name —
			// perhaps one another torrent now uses.
			n.retireLocked(ih, j, false)
		case retryFailed && j.failed():
			// Still wanted: restart it (below) so a cleared obstacle or a
			// transient error doesn't need a daemon restart.
			n.retireLocked(ih, j, false)
		}
	}
	// Prune before launching any new job: a newly (re)wanted infohash can
	// share a data_dir disk name with a now-gone model whose file is only
	// waiting on this sweep to be removed, and a job that hashed the stale
	// file first would fail "left untouched" instead of downloading fresh.
	n.pruneRecordedLocked(inCatalog)
	if adoptIdx == nil {
		for ih := range keep {
			if n.jobs[ih] == nil {
				return true
			}
		}
		return false
	}
	for _, m := range n.cat.Models {
		if !n.wantedLocked(m.ID) {
			continue
		}
		for _, f := range units(m) {
			if _, ok := n.jobs[f.InfoHash]; ok {
				continue
			}
			candidates := adoptIdx.files[f.Size]
			if m.IsDir() {
				candidates = adoptIdx.dirs
			}
			// A predecessor job for this infohash, or for an older revision
			// with the same disk name, may still be winding down (stopped
			// but not yet cleaned up); the replacement waits for both so
			// they never touch the same incoming/data paths or torrent at
			// once.
			j := newJob(n, m.ID, f, candidates, n.stopping[f.InfoHash], n.stoppingDisk[f.DiskName()])
			if m.IsDir() {
				dm := m
				j.dir = &dm
			}
			n.jobs[f.InfoHash] = j
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				j.run()
			}()
		}
	}
	return false
}

// pruneRecordedLocked removes files viiwork-parrot downloaded (recorded in
// n.dl, so this survives a restart) whose infohash is no longer in the
// catalog at all and has no job (running or winding down) still using it.
// It never touches a path outside data_dir, and it never touches adopted
// files: those were never recorded here. Nor does it touch a recorded file
// whose size/mtime no longer match what was recorded at download time (the
// user replaced it, or it's already gone) — the record is simply forgotten
// in that case, without touching whatever is (or isn't) at that path.
func (n *Node) pruneRecordedLocked(inCatalog map[string]bool) {
	if !n.cfg.Models.Prune {
		return
	}
	// A live job's data path is that job's to verify, claim or replace
	// (claimRecord moves a record from an old revision's infohash to the
	// job's own outside n.mu; promote replaces a stale own download), so
	// a record for that path is never pruned from under it.
	live := map[string]bool{}
	for _, j := range n.jobs {
		live[j.dataPath()] = true
	}
	for ih, rec := range n.dl.All() {
		if inCatalog[ih] || n.jobs[ih] != nil || n.stopping[ih] != nil || live[rec.Path] {
			continue
		}
		if !pathInDir(n.cfg.Node.DataDir, rec.Path) {
			n.log.Warn("prune: recorded path is not inside data_dir, refusing to remove", "path", rec.Path)
			continue
		}
		missing, match := statRecorded(rec)
		if missing {
			n.dl.Delete(ih)
			continue
		}
		if !match {
			n.log.Warn("prune: file no longer matches what viiwork-parrot downloaded (replaced?); leaving it and forgetting the record", "path", rec.Path)
			n.dl.Delete(ih)
			continue
		}
		if err := removeRecorded(rec); err != nil && !errors.Is(err, fs.ErrNotExist) {
			n.log.Warn("prune", "path", rec.Path, "err", err)
			continue
		}
		n.dl.Delete(ih)
	}
}

// pathInDir reports whether p is strictly inside dir (not dir itself, no
// escaping "..").
func pathInDir(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// adoptIndex holds adoption candidates: regular files by size (per-file
// models) and directories (folder models).
type adoptIndex struct {
	files map[int64][]string
	dirs  []string
}

// scanAdopt indexes adoption candidates: files under models.adopt (dirs
// walked, symlinked files followed, symlinked dirs not descended *within* a
// walk; a root itself that is a symlink to a directory is resolved first so
// it's still walked; file entries taken as-is) and each
// models.viiwork_configs model path plus its sibling files (split shards).
// Every directory met (the roots included, hidden ones skipped, symlinks to
// directories taken as-is) and every viiwork model path that is a directory
// is a folder-model candidate. findLocal/findLocalDir dedupe by inode.
// testHookScanAdopt, if set (tests only), runs at the start of every
// adoption scan.
var testHookScanAdopt func()

func (n *Node) scanAdopt() *adoptIndex {
	if testHookScanAdopt != nil {
		testHookScanAdopt()
	}
	idx := &adoptIndex{files: map[int64][]string{}}
	add := func(p string) {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
		case fi.Mode().IsRegular():
			idx.files[fi.Size()] = append(idx.files[fi.Size()], p)
		case fi.IsDir():
			idx.dirs = append(idx.dirs, p)
		}
	}
	for _, root := range n.cfg.Models.Adopt {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			n.log.Warn("adopt root", "path", root, "err", err)
			continue
		}
		filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				n.log.Warn("adopt scan", "path", p, "err", err)
				return nil
			}
			if d.IsDir() {
				if p != real && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				idx.dirs = append(idx.dirs, p)
				return nil
			}
			add(p)
			return nil
		})
	}
	for _, vc := range n.cfg.Models.ViiworkConfigs {
		paths, err := viiworkModelPaths(vc)
		if err != nil {
			n.log.Warn("viiwork config", "file", vc, "err", err)
			continue
		}
		for _, p := range paths {
			fi, err := os.Stat(p)
			if err != nil {
				n.log.Warn("viiwork model path not on this host (container path?); add its host directory to models.adopt", "config", vc, "path", p)
				continue
			}
			if fi.IsDir() {
				// A model directory (vLLM/SGLang): a folder-model candidate.
				idx.dirs = append(idx.dirs, p)
				continue
			}
			dir := filepath.Dir(p)
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if !e.IsDir() {
					add(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	return idx
}

func (n *Node) jobsLocked() []*job {
	out := make([]*job, 0, len(n.jobs))
	for _, j := range n.jobs {
		out = append(out, j)
	}
	return out
}

func (n *Node) AddPeers(addrs []string) {
	ps := make([]torrent.PeerInfo, 0, len(addrs))
	for _, a := range addrs {
		// Not Trusted: a local peer can still serve bad pieces (broken
		// disk, stale file), and anacrolix only bans untrusted peers for it.
		ps = append(ps, torrent.PeerInfo{Addr: torrent.StringAddr(a), Source: torrent.PeerSourceDirect})
	}
	n.mu.Lock()
	n.peers = ps
	jobs := n.jobsLocked()
	n.mu.Unlock()
	for _, j := range jobs {
		if t := j.torrent(); t != nil && len(ps) > 0 {
			t.AddPeers(ps)
		}
	}
}

func (n *Node) Ensure(id string) (ModelStatus, error) {
	n.mu.Lock()
	if n.cat == nil {
		n.mu.Unlock()
		return ModelStatus{}, ErrNoCatalog
	}
	if _, ok := n.cat.Model(id); !ok {
		n.mu.Unlock()
		return ModelStatus{}, ErrUnknownModel
	}
	if !n.wantedLocked(id) {
		n.extraWant[id] = true
		n.reconcileScanLocked(false)
	}
	n.mu.Unlock()
	st, ok := n.ModelStatus(id)
	if !ok {
		// A catalog refresh dropped the model since the check above.
		return ModelStatus{}, ErrUnknownModel
	}
	if st.NoSpace {
		return st, ErrNoSpace
	}
	return st, nil
}

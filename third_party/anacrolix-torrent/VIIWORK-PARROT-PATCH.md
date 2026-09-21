# viiwork-parrot local patch

This directory is a local copy of `github.com/anacrolix/torrent` at the
pinned commit, used via a `replace` directive in the repo root's `go.mod`:

```
replace github.com/anacrolix/torrent => ./third_party/anacrolix-torrent
```

**Upstream commit**: `76452a2c8a2f` (module pseudo-version
`v1.61.1-0.20260911233437-76452a2c8a2f`). This is upstream `HEAD` as of the
patch date (2026-09-21) — there is no newer commit to float to instead.

Copied from the local module cache
(`$(go env GOMODCACHE)/github.com/anacrolix/torrent@v1.61.1-0.20260911233437-76452a2c8a2f`),
made writable. Nothing was excluded except two upstream-only files that
don't affect the module's build (see "What was excluded" below).

## The bug

Stopping, pruning, or re-cataloguing a viiwork-parrot model while its web-seed
download is genuinely in flight could crash the whole daemon with:

```
panic: false
github.com/anacrolix/missinggo/v2/panicif.False(...)
github.com/anacrolix/torrent.(*Client).updateWebseedRequests(...)
	webseed-requesting.go:62
github.com/anacrolix/torrent.(*Client).updateWebseedRequestsAndResetTimer.func1(...)
github.com/anacrolix/torrent.(*Client).updateWebseedRequestsAndResetTimer(...)
	webseed-requesting.go:532
github.com/anacrolix/torrent.(*Client).updateWebseedRequestsTimerFunc(...)
	webseed-requesting.go:528
created by time.goFunc
```

Reproduced reliably (100% across many runs) with **zero** viiwork-parrot-side
rate limiting involved — it is not specific to viiwork-parrot's Task 16 bandwidth
scheduler, and not caused by anacrolix's own (separately, uncancellable)
webseed response-body rate limiter. It is a structural race inside
anacrolix itself, present since the module first shipped webseed support,
triggered by calling `Torrent.Drop()` while that torrent has an in-flight
webseed request.

### The code trace

- `Torrent.Drop()` (`t.go`) does the entire close under the client lock:

  ```go
  func (t *Torrent) Drop() {
  	...
  	t.cl.lock()
  	defer t.cl.unlock()
  	...
  	t.close(&wg)
  	wg.Wait()
  }
  ```

- `Torrent.close()` (`torrent.go`, pre-patch) does, still under that same
  lock: cancel `t.closedCtx` early, close every peer (webseed peers
  included — closing a peer's context triggers its `onClose` handler), and
  only *near the end* remove the torrent from the client-global
  `Client.torrents` map:

  ```go
  func (t *Torrent) close(wg *sync.WaitGroup) {
  	...
  	t.closedCtxCancel(errTorrentClosed)
  	...
  	t.iterPeers(func(p *Peer) { p.close() })
  	...
  	g.MustDelete(t.cl.torrents, t)   // <- torrent gone from Client.torrents here
  	// This doesn't work yet because requests remove themselves after they close, and we don't
  	// remove them synchronously.
  	if false {
  		if len(t.cl.torrents) == 0 {
  			panicif.NotZero(len(t.cl.activeWebseedRequests))
  		}
  	}
  }
  ```

  That disabled (`if false`) check, and its comment, is upstream's own
  acknowledgment of exactly this gap.

- A webseed peer's `onClose` (`webseed-peer.go`) reacts to being closed by
  immediately requesting a request-scheduler update:

  ```go
  func (ws *webseedPeer) onClose() {
  	ws.peer.t.iterPeers(func(p *Peer) {
  		if p.isLowOnRequests() {
  			p.onNeedUpdateRequests("webseedPeer.onClose")
  		}
  	})
  }
  ```

  which flows through `Client.updateWebseedRequestsWithReason` →
  `scheduleImmediateWebseedRequestUpdate` (`webseed-requesting.go`), which
  just does `cl.webseedRequestTimer.Reset(0)` — arming a **separate**
  goroutine (`time.AfterFunc`, matching the panic's `created by
  time.goFunc` line) to run `updateWebseedRequestsTimerFunc` as soon as the
  Go runtime schedules it. That goroutine also needs the client lock.

- Meanwhile, each in-flight webseed request's own goroutine
  (`webseedPeer.sliceProcessor`, spawned by `spawnRequest`) is reading an
  HTTP response body. Once it notices its (now-cancelled) context, it
  eventually returns from `readChunks`, then does:

  ```go
  locker.Lock()
  ws.deleteActiveRequest(webseedRequest)   // removes from Client.activeWebseedRequests
  ...
  locker.Unlock()
  ```

  — also under the client lock, but **on its own schedule**, sometime
  after `Drop()` has already returned (and so already released the lock).

- `Client.updateWebseedRequests()` (`webseed-requesting.go`) asserts that
  `Client.activeWebseedRequests` (populated by `spawnRequest`, removed only
  by each request's own `deleteActiveRequest` call above) exactly equals
  what's reachable by iterating **live** `Client.torrents` and each of
  *their* webseed peers' own `activeRequests` sets:

  ```go
  func (cl *Client) updateWebseedRequests() {
  	existingRequests := maps.Collect(cl.iterCurrentWebseedRequestsFromClient())
  	panicif.False(maps.Equal(existingRequests, maps.Collect(cl.iterCurrentWebseedRequests())))
  	...
  ```

Putting it together: `Drop()` removes the torrent from `Client.torrents`
and (via `onClose`) arms a zero-delay timer to re-check webseed requests —
all before releasing the client lock. The in-flight request's own cleanup
(removing it from `Client.activeWebseedRequests`) needs that same lock but
hasn't run yet. Once `Drop()` releases the lock, the just-armed timer
goroutine and the request's own cleanup goroutine race for the lock with
**no ordering guarantee**. If the timer wins, `updateWebseedRequests` finds
a request in `Client.activeWebseedRequests` whose torrent is no longer in
`Client.torrents` — the exact inconsistency the assertion panics on.

This explains both severities observed empirically: forcing a mid-transfer
`Drop()` (many requests genuinely in flight) hits this close to 100% of the
time; a natural end-of-download `Drop()` (nothing left in flight by then)
hit it far more rarely (~9% in earlier testing), from the same root cause.

## The patch

Chosen approach: **(a)** from the ruling's options — synchronously remove
a closing torrent's webseed requests from the client-global bookkeeping
*before* the torrent is removed from `Client.torrents`, and make the
now-possibly-redundant later removal idempotent. Rejected **(b)** (make the
consistency check/iterators skip requests belonging to a closed torrent):
it only patches the symptom at the one call site that happens to assert on
it, leaving `Client.activeWebseedRequests` (and the per-host
`numWebSeedRequests` concurrency counter, used to admission-control new
requests) holding stale entries for a torrent that no longer exists until
each request's own goroutine eventually gets around to removing them —
i.e. it tolerates a temporary global-state leak/skew rather than closing
it. Option (a) keeps anacrolix's own invariant genuinely true at every
point after the client lock is released, not just at the one place that
currently checks it, matching what upstream's own disabled check and
comment (`torrent.go`, "we don't remove them synchronously") already
identify as the real fix.

### Files changed (grep `viiwork-parrot patch` to find every line)

- **`torrent.go`**: `Torrent.close()` now calls a new
  `t.dropActiveWebseedRequestsLocked()` (added right after `close()`)
  before `t.iterPeers(func(p *Peer) { p.close() })`, still under the
  client lock `Drop()` already holds. It iterates every webseed peer's
  `activeRequests` and calls the existing `deleteActiveRequest` for each,
  synchronously bringing `Client.activeWebseedRequests` in line with
  `Client.torrents` before the torrent is ever removed from it.

- **`webseed-peer.go`**: `webseedPeer.deleteActiveRequest` used
  `g.MustDelete` (panics if the key is already gone) for both map
  removals. Changed to a guarded, idempotent version: if the entry isn't
  in `ws.activeRequests` anymore (already removed by the new synchronous
  close-time cleanup above), it's a no-op. This is necessary because the
  request's own goroutine (`sliceProcessor`) still calls
  `deleteActiveRequest` itself once it notices cancellation — now simply a
  redundant, harmless call in the already-closed case.

The diff is intentionally tiny: one new call site, one new ~15-line
method, and one existing method's two `g.MustDelete` calls turned into
guarded `delete`s. No other behaviour changes.

### Why this doesn't break anacrolix's invariants

The invariant `updateWebseedRequests` checks is exactly: "every request
`Client.activeWebseedRequests` knows about is reachable by iterating live
torrents' webseed peers, and vice versa." The patch enforces the "vice
versa" direction proactively at the one point a torrent stops being live
(`close()`), instead of waiting for it to become true asynchronously and
hoping nothing observes the gap. Both `dropActiveWebseedRequestsLocked`
(new) and `deleteActiveRequest` (patched) run only under the client lock
that already protects every other reader/writer of
`Client.activeWebseedRequests`/`ws.activeRequests`/`numWebSeedRequests`
(confirmed: `Drop()` holds it for `close()`'s entire body; `sliceProcessor`
takes `ws.locker`, which is `t.cl.locker()`, before calling
`deleteActiveRequest`) — no new locking scheme, no new lock, nothing
changes about who is allowed to touch this state or when.

### How to drop this patch once upstream fixes it

1. Check whether upstream anacrolix/torrent has picked up an equivalent
   fix (search for `dropActiveWebseedRequestsLocked`, or for the
   currently-disabled check at the end of `Torrent.close()` becoming
   enabled, or for any commit touching `deleteActiveRequest`/
   `updateWebseedRequests`'s consistency check).
2. Bump the pinned version in the repo root's `go.mod` requirement for
   `github.com/anacrolix/torrent` to that commit.
3. Delete the `replace github.com/anacrolix/torrent => ./third_party/anacrolix-torrent`
   line from `go.mod`.
4. Delete this `third_party/anacrolix-torrent` directory.
5. Run `go mod tidy`, then the node/throttle test suites (in particular
   `TestDropMidDownloadDoesNotPanic` with `-count=20 -race`) to confirm the
   upstream fix covers the same scenario.

## Second patch: zero-length files in multi-file torrents (v0.2.0)

Folder models (one multi-file torrent per model) can contain zero-length
files. `storage/file-torrent-io.go`'s `ReadAt`/`WriteAt` visit every file
segment a request overlaps, including the zero-length extent of an empty
file, and open that file for IO. With anacrolix's default mmap file IO that
is `mmap` of length 0, which fails with `EINVAL` ("writing received chunk 0:
invalid argument"), and anacrolix then disables data download for the whole
torrent — the download stalls forever at 0%. The classic (pread/pwrite) IO
the daemon selects does not hit it, but tests and anyone forcing
`TORRENT_STORAGE_DEFAULT_FILE_IO=mmap` do.

The patch skips zero-length extents in both loops (`if e.Length == 0 {
continue }`, marked "viiwork-parrot patch"): they carry no bytes, and the
files themselves are created when the torrent is opened
(`CreateNativeZeroLengthFile`). `internal/node`'s folder-model tests (which
include an empty file and run with mmap IO) cover it.

### Existing zero-length files are never re-opened for writing

Upstream `CreateNativeZeroLengthFile` (`storage/file-misc.go`), called for
every zero-length file whenever a torrent is opened
(`storage/file-client.go` `OpenTorrent`), opened the path
`O_RDWR|O_CREATE|O_TRUNC`. viiwork-parrot seeds folders in place — adopted
user directories, HF-cache snapshot symlinks — so this wrote to files it
does not own: it reset their mtime/ctime (which also broke the size+mtime
identity viiwork-parrot records for its own downloads) and would truncate a
file that had gained content since it was verified. The patched function
(marked "viiwork-parrot patch") returns immediately if a regular file
(symlinks followed) already exists at the path, and otherwise creates it
with `O_CREATE|O_EXCL` and no `O_TRUNC`.
`TestDirSeedingLeavesEmptyFilesAlone` and `TestDirDownloadFromWebSeed` (the
download record still matches while seeding) in `internal/node` cover it.

## What was excluded from the copy

- `go.work` / `go.work.sum`: upstream's own multi-module development
  workspace file, listing `./storage/possum/lib/go` as a second workspace
  module. That path is a separate git submodule of the upstream repo and
  isn't included in the module's zip in the Go module cache (it's not part
  of the `github.com/anacrolix/torrent` module itself, only of the
  upstream *repository*), so the directory `storage/possum/lib/go` exists
  under `storage/possum` but has no `go.mod` in it — `go.work` pointing at
  it made every `go` command run from inside this directory fail
  immediately with `cannot load module storage/possum/lib/go listed in
  go.work file`. Removing it has no effect on building or importing the
  `github.com/anacrolix/torrent` module (Go only honours a `go.work` file
  when a command is run from that directory or a descendant of it, and
  nothing in the viiwork-parrot module tree does that); it only stops someone
  from accidentally hitting workspace mode by `cd`-ing into
  `third_party/anacrolix-torrent` directly.
- Nothing else. No file in the copy exceeds 500 KiB, and no `testdata`
  directory exceeds 1 MiB (`testdata` at the root is 456 KiB;
  `metainfo/testdata` 188 KiB; `bencode/testdata` 104 KiB;
  `peer_protocol/testdata` 40 KiB; `webtorrent/testdata` 24 KiB) — all well
  under the ruling's 1 MiB testdata threshold, so all were kept.

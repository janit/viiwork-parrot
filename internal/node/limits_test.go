package node

import (
	"reflect"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/schedule"
)

func TestRankUploads(t *testing.T) {
	c := []uploadCandidate{{"a", 1, 1 << 30}, {"b", 5, 1 << 30}, {"c", 5, 1 << 30}, {"d", 0, 1 << 30}}
	if got := rankUploads(c, 2); !reflect.DeepEqual(got, map[string]bool{"b": true, "c": true}) {
		t.Fatalf("%v", got)
	}
	if got := rankUploads(c, 0); len(got) != 4 {
		t.Fatalf("0 = unlimited: %v", got)
	}
}

// TestRankUploadsSmallFilesAlwaysAllowed: license/readme files (< 64 MiB)
// never take one of the max_active_uploads slots and are never refused one,
// however much demand the big files have.
func TestRankUploadsSmallFilesAlwaysAllowed(t *testing.T) {
	c := []uploadCandidate{
		{"lic", 9, 10 << 10}, {"readme", 8, smallUploadFile - 1},
		{"big1", 1, 20 << 30}, {"big2", 5, smallUploadFile},
	}
	want := map[string]bool{"lic": true, "readme": true, "big2": true}
	if got := rankUploads(c, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestScheduleAndOverride(t *testing.T) {
	fx := newFixture(t)
	n := fx.node(nil)
	up := int64(10_000_000)
	n.cfg.Schedule = schedule.Schedule{
		Base:  schedule.Limits{Upload: 50_000_000, MaxConns: 200, MaxConnsPerTorrent: 50, MaxActiveUploads: 4},
		Rules: []schedule.Rule{{Days: [7]bool{true, true, true, true, true, true, true}, From: 8 * 60, To: 18 * 60, Set: schedule.Partial{Upload: &up}}},
	}
	day := func(h int) time.Time { return time.Date(2026, 9, 22, h, 0, 0, 0, time.Local) }

	n.now = func() time.Time { return day(10) }
	n.applyLimits(n.now())
	if n.pol.Buckets.Up.Limit() != rate.Limit(10_000_000) || n.Limits().Rule != 0 {
		t.Fatalf("rule 0 not applied: %v", n.pol.Buckets.Up.Limit())
	}

	one := int64(1_000_000)
	n.SetOverride(schedule.Partial{Upload: &one})
	if n.pol.Buckets.Up.Limit() != rate.Limit(1_000_000) || n.Limits().Override == nil {
		t.Fatal("override not applied immediately")
	}
	n.applyLimits(day(11)) // same rule
	if n.pol.Buckets.Up.Limit() != rate.Limit(1_000_000) {
		t.Fatal("override should survive within the same rule")
	}
	n.applyLimits(day(19)) // rule boundary -> base
	if n.pol.Buckets.Up.Limit() != rate.Limit(50_000_000) || n.Limits().Override != nil || n.Limits().Rule != -1 {
		t.Fatal("override should clear at the schedule boundary")
	}

	// A jump (suspend) from one day's rule 0 to the next day's rule 0
	// passes the 18:00 boundary in between: the override is gone.
	n.now = func() time.Time { return day(10) }
	n.SetOverride(schedule.Partial{Upload: &one})
	n.applyLimits(day(10).AddDate(0, 0, 1))
	if n.pol.Buckets.Up.Limit() != rate.Limit(10_000_000) || n.Limits().Override != nil {
		t.Fatal("override should clear at a boundary passed between two ticks")
	}

	n.now = func() time.Time { return day(20) }
	n.SetOverride(schedule.Partial{Upload: &one})
	n.ClearOverride()
	if n.pol.Buckets.Up.Limit() != rate.Limit(50_000_000) {
		t.Fatal("ClearOverride should restore scheduled limits")
	}
}

func TestRatesAppearInStatus(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 8<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.cfg.Schedule.Base.Download = 1 << 20 // 1MiB/s web-seed download, applied by Start
	n.SetCatalog(fx.cat)
	n.Start()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, _ := n.ModelStatus("tiny")
		if st.State == StateDownloading && st.DownRate > 0 {
			if st.DownRate > 2<<20 {
				t.Fatalf("download rate %d exceeds 1MiB/s cap", st.DownRate)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no download rate observed: %+v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestApplyLimitsSetsConnBudgetAndUploadSlots is the fix-round-1 item-5
// regression: with two seeding torrents and MaxActiveUploads=1, applyLimits
// must actually call through to anacrolix so exactly one job ends up
// upload-disallowed, and both get the connbudget-computed max conns. This
// would have caught the item-2 bug (a job's cached maxConns/uploadOK
// surviving a torrent swap and so never getting re-applied), since it reads
// the cache back after the calls have already been forced on freshly-seeded
// torrents.
func TestApplyLimitsSetsConnBudgetAndUploadSlots(t *testing.T) {
	old := smallUploadFile
	smallUploadFile = 0 // these 64KiB files must count against the slots here
	defer func() { smallUploadFile = old }()
	fx := newFixture(t)
	fx.addModel("a", "a.gguf", 64<<10)
	fx.addModel("b", "b.gguf", 64<<10)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"*"} })
	n.cfg.Schedule.Base = schedule.Limits{MaxConns: 20, MaxConnsPerTorrent: 50, MaxActiveUploads: 1}
	n.SetCatalog(fx.cat)
	waitState(t, n, "a", StateSeeding)
	waitState(t, n, "b", StateSeeding)
	n.applyLimits(n.now())

	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(n.jobs))
	}
	var falseCount int
	for _, j := range n.jobs {
		j.mu.Lock()
		if !j.uploadOK {
			falseCount++
		}
		if j.maxConns != 10 {
			t.Errorf("job %s maxConns = %d, want 10 (MaxConns=20 split 2 ways)", j.f.Name, j.maxConns)
		}
		j.mu.Unlock()
	}
	if falseCount != 1 {
		t.Fatalf("expected exactly one job with uploadOK=false (MaxActiveUploads=1, two seeding jobs), got %d", falseCount)
	}
}

// TestConnBudgetReappliedAfterTorrentSwap is the fix-round-1 item-2
// regression, isolated: a lone job's connbudget allocation is numerically
// identical whether it's downloading or seeding (weight cancels out with
// only one torrent, and MaxConns is left unlimited here so the allocation
// is always exactly MaxConnsPerTorrent). Without tracking which *torrent
// the cached value was applied to, the download->seed swap (a brand new
// *torrent.Torrent with anacrolix's own default max conns) would never get
// SetMaxEstablishedConns called on it, because the cached value already
// "matched". SetMaxEstablishedConns's return (the previous value) is used
// to inspect what's really configured on the current, live torrent object.
func TestConnBudgetReappliedAfterTorrentSwap(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 4<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.cfg.Schedule.Base = schedule.Limits{MaxConnsPerTorrent: 37, MaxActiveUploads: 4}
	n.cfg.Schedule.Base.Download = 1 << 20 // stretch the download past at least one tick
	n.SetCatalog(fx.cat)
	n.Start()
	waitState(t, n, "tiny", StateSeeding)
	n.applyLimits(n.now()) // a tick after the download->seed swap, without waiting for the 2s ticker

	n.mu.Lock()
	var j *job
	for _, jj := range n.jobs {
		j = jj
	}
	n.mu.Unlock()
	if j == nil {
		t.Fatal("job not found")
	}
	j.mu.Lock()
	tr := j.t
	j.mu.Unlock()
	if tr == nil {
		t.Fatal("job has no seeding torrent")
	}
	if old := tr.SetMaxEstablishedConns(999); old != 37 {
		t.Fatalf("seed torrent's max established conns = %d, want 37 (re-applied after the download->seed swap)", old)
	}
}

// TestDropMidDownloadDoesNotPanic is the fix-round-1 item-1(d) regression,
// now un-skipped in fix round 2.
//
// It used to reliably reproduce a panic inside anacrolix's own
// updateWebseedRequests (webseed-requesting.go:62) when a torrent with an
// in-flight webseed request is Drop()'d. This was NOT the uncancellable
// time.Sleep the fix-round-1 ruling identified — that theory was disproven
// (see the task-16 report's fix-round-1 section: the identical panic
// reproduced 100% of the time even with throttle.Transport/WrapBody in
// place, and even with zero throttling/Task-16 code involved at all, i.e.
// from plain Task 14/15 job.cleanup() -> t.Drop() while a webseed download
// is genuinely in flight). The real, verified cause: Torrent.Drop holds
// cl.lock() for its entire body, which (a) removes the torrent from
// Client.torrents and (b) closes its peers, whose onClose triggers a
// zero-delay reset of Client.webseedRequestTimer — all before releasing the
// lock. The in-flight webseed request goroutines (reading the HTTP
// response body) only noticed the cancelled context and removed their own
// entries from Client.activeWebseedRequests later, on their own schedule,
// also under cl.lock(). Once Drop() released the lock, that timer's
// goroutine and the request goroutines' own cleanup raced for the lock with
// no ordering guarantee; updateWebseedRequests' invariant
// (Client.activeWebseedRequests must equal what's reachable by iterating
// live Client.torrents) failed whenever the timer won, since the
// still-registered request then belonged to a torrent no longer in
// Client.torrents.
//
// Fix round 2 locally patches the pinned anacrolix commit (see
// third_party/anacrolix-torrent/VIIWORK-PARROT-PATCH.md): Torrent.close now
// synchronously drops this torrent's webseed requests from
// Client.activeWebseedRequests (via the new dropActiveWebseedRequestsLocked)
// before removing the torrent from Client.torrents, so the two views can
// never be observed inconsistent; the later, now-redundant async
// deleteActiveRequest call was made idempotent to match.
func TestDropMidDownloadDoesNotPanic(t *testing.T) {
	fx := newFixture(t)
	fx.addModel("tiny", "tiny.gguf", 4<<20)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"} })
	n.cfg.Schedule.Base.Download = 256 << 10 // slow enough to still be mid-transfer when dropped
	n.SetCatalog(fx.cat)
	n.Start()

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := n.ModelStatus("tiny")
		if st.State == StateDownloading && st.Percent > 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never observed a genuinely in-flight download: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Drop the model mid-download by pointing the node at a catalog that no
	// longer has it: reconcileLocked stops the job, which cancels its
	// context and Drops its torrent while the throttled webseed read is
	// still in flight.
	n.SetCatalog(&catalog.Catalog{Version: 1})

	// Keep the node (and its limits ticker, and anacrolix's own internal
	// periodic request-scheduler) alive a few seconds so anything still
	// unwinding from the drop has time to run concurrently with further
	// ticks — this is what used to expose the panic.
	time.Sleep(3 * time.Second)

	// No panic (which would already have failed the whole test binary), and
	// the node must still be alive and responsive.
	_ = n.Status()
	if _, ok := n.ModelStatus("tiny"); ok {
		t.Fatal("tiny should no longer be wanted after the empty SetCatalog")
	}
}

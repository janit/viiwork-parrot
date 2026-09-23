package node

import (
	"sort"
	"time"

	"github.com/janit/viiwork-parrot/internal/connbudget"
	"github.com/janit/viiwork-parrot/internal/schedule"
)

type limitsState struct {
	eff      schedule.Limits
	rule     int
	override *override
	rates    map[string]rateSample // by infohash
	lastTick time.Time
}

type override struct {
	set  schedule.Partial
	rule int
	at   time.Time // when it was set
}

type rateSample struct {
	up, down         int64 // cumulative bytes
	upRate, downRate int64 // bytes/s over the last tick
}

type LimitsStatus struct {
	Effective schedule.Limits   `json:"effective"`
	Rule      int               `json:"rule"` // -1 = base limits
	Override  *schedule.Partial `json:"override,omitempty"`
}

type uploadCandidate struct {
	Key    string
	Demand int
	Size   int64
}

// smallUploadFile: files below this (license, README) are exempt from
// max_active_uploads — they cost almost nothing to serve, and a model's
// terms must stay obtainable even when every slot is taken by big files.
var smallUploadFile int64 = 64 << 20 // a var only so tests with tiny files can lower it

// rankUploads picks the n seeding torrents with the most non-seed peers.
// Small files are always allowed and never take one of the n slots.
func rankUploads(c []uploadCandidate, n int) map[string]bool {
	out := map[string]bool{}
	var sorted []uploadCandidate
	for _, u := range c {
		if u.Size < smallUploadFile {
			out[u.Key] = true
		} else {
			sorted = append(sorted, u)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Demand != sorted[j].Demand {
			return sorted[i].Demand > sorted[j].Demand
		}
		return sorted[i].Key < sorted[j].Key
	})
	for i, u := range sorted {
		if n > 0 && i >= n {
			break
		}
		out[u.Key] = true
	}
	return out
}

// Start applies limits immediately and re-applies every 2s until Close. It
// is idempotent: only the first call starts the loop.
func (n *Node) Start() {
	n.startOnce.Do(func() {
		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			return
		}
		n.wg.Add(1) // under mu, so it can never race Close's Wait
		n.mu.Unlock()
		n.applyLimits(n.now())
		go func() {
			defer n.wg.Done()
			tk := time.NewTicker(2 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-n.ctx.Done():
					return
				case <-tk.C:
					n.applyLimits(n.now())
				}
			}
		}()
	})
}

func (n *Node) Limits() LimitsStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	ls := LimitsStatus{Effective: n.lim.eff, Rule: n.lim.rule}
	if n.lim.override != nil {
		p := n.lim.override.set
		ls.Override = &p
	}
	return ls
}

// SetOverride applies p immediately. It lasts until the schedule's matching
// rule index changes (i.e. until the next schedule boundary).
func (n *Node) SetOverride(p schedule.Partial) {
	now := n.now()
	_, idx := n.cfg.Schedule.Effective(now)
	n.mu.Lock()
	n.lim.override = &override{set: p, rule: idx, at: now}
	n.mu.Unlock()
	n.applyLimits(now)
}

func (n *Node) ClearOverride() {
	n.mu.Lock()
	n.lim.override = nil
	n.mu.Unlock()
	n.applyLimits(n.now())
}

func (n *Node) applyLimits(now time.Time) {
	// Serializes end to end (schedule ticks vs. SetOverride/ClearOverride)
	// so the two can never interleave: without this, a tick racing a
	// SetOverride call could read the override, have it clobbered by the
	// tick's own (unconditional) recomputation, and revert it for up to the
	// next 2s tick; concurrent runs could also each read/modify the
	// per-job/rate caches below on stale data. applyMu is never held across
	// anacrolix calls elsewhere, and nothing but applyLimits ever takes it,
	// so holding it for this whole call (anacrolix calls included) is safe.
	n.applyMu.Lock()
	defer n.applyMu.Unlock()
	eff, idx := n.cfg.Schedule.Effective(now)
	n.mu.Lock()
	if o := n.lim.override; o != nil {
		// Cleared at the next schedule boundary, including one passed
		// between two ticks (suspend, clock jump) that lands back on the
		// same rule.
		since := n.lim.lastTick
		if since.Before(o.at) {
			since = o.at
		}
		if o.rule != idx || n.cfg.Schedule.Boundary(since, now) {
			n.lim.override = nil
		}
	}
	if o := n.lim.override; o != nil {
		eff = o.set.Apply(eff)
	}
	n.lim.eff, n.lim.rule = eff, idx
	prev := n.lim.lastTick
	n.lim.lastTick = now
	jobs := n.jobsLocked()
	n.mu.Unlock()

	n.pol.Buckets.Set(eff.Upload, eff.Download)

	type live struct {
		j  *job
		st State
	}
	var lives []live
	var cands []connbudget.Torrent
	var seeders []uploadCandidate
	samples := map[string]rateSample{}
	dt := now.Sub(prev).Seconds()
	n.mu.Lock()
	old := n.lim.rates
	n.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		t, st := j.t, j.state
		j.mu.Unlock()
		if t == nil {
			continue
		}
		local := 0
		for _, pc := range t.PeerConns() {
			if n.pol.Local.IsLocalString(pc.RemoteAddr.String()) {
				local++
			}
		}
		stats := t.Stats()
		ih := j.f.InfoHash
		cands = append(cands, connbudget.Torrent{Key: ih, Downloading: st == StateDownloading, Local: local})
		if st == StateSeeding {
			seeders = append(seeders, uploadCandidate{Key: ih, Demand: stats.ActivePeers - stats.ConnectedSeeders, Size: j.f.Size})
		}
		s := rateSample{up: stats.BytesWrittenData.Int64(), down: stats.BytesReadUsefulData.Int64()}
		if o, ok := old[ih]; ok && dt > 0 {
			// A torrent swap (download -> seed) restarts anacrolix's
			// cumulative counters at zero, which would otherwise show up
			// here as a large negative rate; clamp at 0 instead.
			s.upRate = max(0, int64(float64(s.up-o.up)/dt))
			s.downRate = max(0, int64(float64(s.down-o.down)/dt))
		}
		samples[ih] = s
		lives = append(lives, live{j, st})
	}
	n.mu.Lock()
	n.lim.rates = samples
	n.mu.Unlock()

	alloc := connbudget.Allocate(connbudget.Params{
		MaxConns: eff.MaxConns, PerTorrent: eff.MaxConnsPerTorrent, CountLocal: n.cfg.Local.CountInMaxConns,
	}, cands)
	allowed := rankUploads(seeders, eff.MaxActiveUploads)
	for _, l := range lives {
		ih := l.j.f.InfoHash
		allow := l.st != StateSeeding || allowed[ih]
		l.j.mu.Lock()
		t := l.j.t
		// A job can swap to a new *torrent.Torrent between ticks (download
		// completes -> Drop -> re-add for seeding). The new torrent starts
		// with anacrolix's own defaults, not whatever we'd last applied to
		// the old one, so maxConns/uploadOK must be (re)applied whenever t
		// isn't the same object they were last applied to, even if the
		// newly computed values happen to numerically match the cache.
		sameTorrent := t != nil && t == l.j.appliedTo
		setConns := !sameTorrent || alloc[ih] != l.j.maxConns
		setUpload := !sameTorrent || allow != l.j.uploadOK
		l.j.mu.Unlock()
		if t == nil {
			continue
		}
		if setConns {
			t.SetMaxEstablishedConns(alloc[ih])
		}
		if setUpload {
			if allow {
				t.AllowDataUpload()
			} else {
				t.DisallowDataUpload()
			}
		}
		// Only record the cache once the calls that justify it actually
		// ran, keyed to the torrent they were applied to.
		l.j.mu.Lock()
		if setConns {
			l.j.maxConns = alloc[ih]
		}
		if setUpload {
			l.j.uploadOK = allow
		}
		l.j.appliedTo = t
		l.j.mu.Unlock()
	}
}

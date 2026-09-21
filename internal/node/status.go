package node

import (
	"math"
	"path"
	"strings"
)

type FileStatus struct {
	Name       string   `json:"name"`
	Path       string   `json:"path,omitempty"`
	State      State    `json:"state"`
	Size       int64    `json:"size"`
	Done       int64    `json:"done"`
	Error      string   `json:"error,omitempty"`
	Duplicates []string `json:"duplicates,omitempty"`
}

type ModelStatus struct {
	ID       string  `json:"id"`
	State    State   `json:"state"`
	Percent  float64 `json:"percent"`
	Peers    int     `json:"peers"`
	Seeds    int     `json:"seeds"`
	Uploaded int64   `json:"uploaded"`
	UpRate   int64   `json:"up_rate"`
	DownRate int64   `json:"down_rate"`
	Path     string  `json:"path,omitempty"`
	Error    string  `json:"error,omitempty"`
	NoSpace  bool    `json:"no_space,omitempty"`
	// Duplicates lists extra verified copies of this model's files on the
	// host: disk that could be reclaimed by the owner (viiwork-parrot never deletes them).
	Duplicates []string     `json:"duplicates,omitempty"`
	Files      []FileStatus `json:"files"`
}

// Lower rank wins when summarising a model's files.
var stateRank = map[State]int{
	StateFailed: 0, StatePaused: 1, StateAbsent: 2, StateQueued: 3,
	StateAdopting: 4, StateDownloading: 5, StateVerifying: 6, StateSeeding: 7,
}

func (j *job) status() (FileStatus, int, int, int64, bool) {
	j.mu.Lock()
	fs := FileStatus{Name: j.f.DiskName(), Path: j.path, State: j.state, Size: j.f.Size, Error: j.err, Duplicates: j.dups}
	t, noSpace := j.t, j.noSpace
	j.mu.Unlock()
	var peers, seeds int
	var uploaded int64
	if fs.State == StateSeeding {
		fs.Done = fs.Size
	} else if t != nil && t.Info() != nil {
		fs.Done = t.BytesCompleted()
	}
	if t != nil {
		st := t.Stats()
		peers, seeds, uploaded = st.ActivePeers, st.ConnectedSeeders, st.BytesWrittenData.Int64()
	}
	return fs, peers, seeds, uploaded, noSpace
}

func (n *Node) Status() []ModelStatus {
	n.mu.Lock()
	var ids []string
	if n.cat != nil {
		for _, m := range n.cat.Models {
			if n.wantedLocked(m.ID) {
				ids = append(ids, m.ID)
			}
		}
	}
	n.mu.Unlock()
	out := make([]ModelStatus, 0, len(ids))
	for _, id := range ids {
		if st, ok := n.ModelStatus(id); ok {
			out = append(out, st)
		}
	}
	return out
}

func (n *Node) ModelStatus(id string) (ModelStatus, bool) {
	n.mu.Lock()
	if n.cat == nil || !n.wantedLocked(id) {
		n.mu.Unlock()
		return ModelStatus{}, false
	}
	m, ok := n.cat.Model(id)
	var jobs []*job
	want := units(m)
	for _, f := range want {
		if j := n.jobs[f.InfoHash]; j != nil {
			jobs = append(jobs, j)
		}
	}
	rates := make(map[string]rateSample, len(jobs))
	for _, j := range jobs {
		rates[j.f.InfoHash] = n.lim.rates[j.f.InfoHash]
	}
	n.mu.Unlock()
	if !ok {
		return ModelStatus{}, false
	}
	ms := ModelStatus{ID: id, State: StateSeeding}
	var size, done int64
	main := mainInfoHash(m)
	type jobStatus struct {
		fs           FileStatus
		peers, seeds int
		up           int64
		noSpace      bool
	}
	sts := make([]jobStatus, len(jobs))
	mainSeeding := false
	for i, j := range jobs {
		s := &sts[i]
		s.fs, s.peers, s.seeds, s.up, s.noSpace = j.status()
		if j.f.InfoHash == main && s.fs.State == StateSeeding && !m.IsDir() && isGGUF(j.f.Name) {
			mainSeeding = true
		}
	}
	for i, j := range jobs {
		fs := sts[i].fs
		ms.Files = append(ms.Files, fs)
		ms.Peers += sts[i].peers
		ms.Seeds += sts[i].seeds
		ms.Uploaded += sts[i].up
		ms.UpRate += rates[j.f.InfoHash].upRate
		ms.DownRate += rates[j.f.InfoHash].downRate
		if mainSeeding && fs.State == StateAbsent && isDocCompanion(j.f.Name) {
			// A documentation companion (the HF README.md) that a
			// seed_only_existing node doesn't have: it's still listed as
			// absent in Files, but it doesn't make a GGUF model whose main
			// file is seeding "absent". Any other absent file (a shard,
			// config.json, …) still does.
			continue
		}
		if stateRank[fs.State] < stateRank[ms.State] {
			ms.State = fs.State
		}
		if ms.Error == "" {
			ms.Error = fs.Error
		}
		ms.NoSpace = ms.NoSpace || sts[i].noSpace
		ms.Duplicates = append(ms.Duplicates, fs.Duplicates...)
		size += fs.Size
		done += fs.Done
		if j.f.InfoHash == main && fs.State == StateSeeding {
			ms.Path = fs.Path
		}
	}
	if len(jobs) < len(want) {
		ms.State = StateQueued
	}
	if size > 0 {
		// Rounded down to 0.1 so "100.0%" means every byte is there.
		ms.Percent = math.Floor(float64(done)*1000/float64(size)) / 10
	}
	if ms.State != StateSeeding {
		ms.Path = ""
	}
	return ms, true
}

// isGGUF reports whether a per-file model's file is a .gguf.
func isGGUF(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".gguf") }

// isDocCompanion reports whether a file is documentation that a model never
// needs to load: README*, LICENSE*, NOTICE*, *.md, *.txt (base name,
// case-insensitive).
func isDocCompanion(name string) bool {
	b := strings.ToLower(path.Base(name))
	for _, p := range []string{"readme", "license", "notice"} {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	return strings.HasSuffix(b, ".md") || strings.HasSuffix(b, ".txt")
}

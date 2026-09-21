package node

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
	for _, j := range jobs {
		fs, peers, seeds, up, noSpace := j.status()
		ms.Files = append(ms.Files, fs)
		if stateRank[fs.State] < stateRank[ms.State] {
			ms.State = fs.State
		}
		if ms.Error == "" {
			ms.Error = fs.Error
		}
		ms.NoSpace = ms.NoSpace || noSpace
		ms.Duplicates = append(ms.Duplicates, fs.Duplicates...)
		ms.Peers += peers
		ms.Seeds += seeds
		ms.Uploaded += up
		ms.UpRate += rates[j.f.InfoHash].upRate
		ms.DownRate += rates[j.f.InfoHash].downRate
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
		ms.Percent = float64(done) * 100 / float64(size)
	}
	if ms.State != StateSeeding {
		ms.Path = ""
	}
	return ms, true
}

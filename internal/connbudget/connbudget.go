// Package connbudget splits the global max_conns across torrents, since
// anacrolix only offers a per-torrent limit.
package connbudget

type Torrent struct {
	Key         string
	Downloading bool
	Local       int // currently established local connections
}

type Params struct {
	MaxConns   int // 0 = unlimited
	PerTorrent int
	CountLocal bool
}

const MinPerTorrent = 4

func Allocate(p Params, ts []Torrent) map[string]int {
	out := make(map[string]int, len(ts))
	per := p.PerTorrent
	if per <= 0 {
		per = 50
	}
	total := 0
	for _, t := range ts {
		total += weight(t)
	}
	for _, t := range ts {
		share := per
		if p.MaxConns > 0 {
			share = p.MaxConns * weight(t) / total
			share = max(share, min(MinPerTorrent, per))
			share = min(share, per)
		}
		if !p.CountLocal {
			share += t.Local
		}
		out[t.Key] = share
	}
	return out
}

func weight(t Torrent) int {
	if t.Downloading {
		return 2
	}
	return 1
}

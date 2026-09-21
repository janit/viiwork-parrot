// Package schedule resolves time-of-day limit rules into effective limits.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Limits struct {
	Upload             int64 `json:"upload"`
	Download           int64 `json:"download"`
	MaxConns           int   `json:"max_conns"`
	MaxConnsPerTorrent int   `json:"max_conns_per_torrent"`
	MaxHalfOpen        int   `json:"max_half_open"`
	MaxActiveUploads   int   `json:"max_active_uploads"`
}

// Partial holds only the keys a rule or override sets.
type Partial struct {
	Upload             *int64 `json:"upload,omitempty"`
	Download           *int64 `json:"download,omitempty"`
	MaxConns           *int   `json:"max_conns,omitempty"`
	MaxConnsPerTorrent *int   `json:"max_conns_per_torrent,omitempty"`
	MaxActiveUploads   *int   `json:"max_active_uploads,omitempty"`
}

func (p Partial) Apply(l Limits) Limits {
	if p.Upload != nil {
		l.Upload = *p.Upload
	}
	if p.Download != nil {
		l.Download = *p.Download
	}
	if p.MaxConns != nil {
		l.MaxConns = *p.MaxConns
	}
	if p.MaxConnsPerTorrent != nil {
		l.MaxConnsPerTorrent = *p.MaxConnsPerTorrent
	}
	if p.MaxActiveUploads != nil {
		l.MaxActiveUploads = *p.MaxActiveUploads
	}
	return l
}

type Rule struct {
	Days     [7]bool // indexed by time.Weekday
	From, To int     // minutes since local midnight; From > To wraps past midnight
	Set      Partial
}

// Matches reports whether t falls in the rule. A window that wraps past
// midnight belongs to the day it starts on.
func (r Rule) Matches(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	wd := t.Weekday()
	prev := (wd + 6) % 7
	switch {
	case r.From == r.To:
		return r.Days[wd]
	case r.From < r.To:
		return r.Days[wd] && m >= r.From && m < r.To
	default:
		return (r.Days[wd] && m >= r.From) || (r.Days[prev] && m < r.To)
	}
}

type Schedule struct {
	Base  Limits
	Rules []Rule
}

// Effective returns the limits active at t and the index of the first
// matching rule, or -1 when only the base limits apply.
func (s Schedule) Effective(t time.Time) (Limits, int) {
	for i, r := range s.Rules {
		if r.Matches(t) {
			return r.Set.Apply(s.Base), i
		}
	}
	return s.Base, -1
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseDays parses entries like "mon-fri", "sat", "sat-mon". No entries means
// every day.
func ParseDays(specs []string) ([7]bool, error) {
	var d [7]bool
	if len(specs) == 0 {
		for i := range d {
			d[i] = true
		}
		return d, nil
	}
	for _, s := range specs {
		s = strings.ToLower(strings.TrimSpace(s))
		a, b, isRange := strings.Cut(s, "-")
		from, ok := dayNames[a]
		if !ok {
			return d, fmt.Errorf("unknown day %q (use sun..sat)", a)
		}
		to := from
		if isRange {
			if to, ok = dayNames[b]; !ok {
				return d, fmt.Errorf("unknown day %q (use sun..sat)", b)
			}
		}
		for w := from; ; w = (w + 1) % 7 {
			d[w] = true
			if w == to {
				break
			}
		}
	}
	return d, nil
}

// ParseClock parses "HH:MM" (00:00..24:00) into minutes since midnight.
func ParseClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("time %q: out of range", s)
	}
	return h*60 + m, nil
}

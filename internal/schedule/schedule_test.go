package schedule

import (
	"testing"
	"time"
)

func at(day time.Weekday, hh, mm int) time.Time {
	// 2026-09-20 is a Sunday.
	return time.Date(2026, 9, 20+int(day), hh, mm, 0, 0, time.Local)
}

func i64(v int64) *int64 { return &v }
func iptr(v int) *int    { return &v }

func mustDays(t *testing.T, s ...string) [7]bool {
	d, err := ParseDays(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseDays(t *testing.T) {
	d := mustDays(t, "mon-fri")
	want := [7]bool{false, true, true, true, true, true, false}
	if d != want {
		t.Fatalf("mon-fri = %v", d)
	}
	if d := mustDays(t, "sat-mon"); d != [7]bool{true, true, false, false, false, false, true} {
		t.Fatalf("sat-mon wraps the week: %v", d)
	}
	if d := mustDays(t); d != [7]bool{true, true, true, true, true, true, true} {
		t.Fatalf("empty = every day: %v", d)
	}
	if _, err := ParseDays([]string{"funday"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "08:30": 510, "23:59": 1439, "24:00": 1440} {
		if got, err := ParseClock(in); err != nil || got != want {
			t.Errorf("ParseClock(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"8", "25:00", "12:60", "24:01", "ab:cd"} {
		if _, err := ParseClock(in); err == nil {
			t.Errorf("ParseClock(%q): expected error", in)
		}
	}
}

func TestEffective(t *testing.T) {
	s := Schedule{
		Base: Limits{Upload: 50, Download: 0, MaxConns: 200, MaxConnsPerTorrent: 50, MaxActiveUploads: 4},
		Rules: []Rule{
			{Days: mustDays(t, "mon-fri"), From: 8 * 60, To: 18 * 60, Set: Partial{Upload: i64(10), MaxConns: iptr(60)}},
			{Days: mustDays(t, "fri"), From: 22 * 60, To: 6 * 60, Set: Partial{Upload: i64(0)}},
			{Days: mustDays(t), From: 8 * 60, To: 20 * 60, Set: Partial{Download: i64(7)}},
		},
	}
	cases := []struct {
		name  string
		t     time.Time
		rule  int
		upl   int64
		conns int
		down  int64
	}{
		{"weekday office hours", at(time.Tuesday, 10, 0), 0, 10, 60, 0},
		{"weekday evening falls to rule 2", at(time.Tuesday, 19, 0), 2, 50, 200, 7},
		{"weekday night base", at(time.Tuesday, 23, 0), -1, 50, 200, 0},
		{"fri night window", at(time.Friday, 23, 0), 1, 0, 200, 0},
		{"wraps into saturday", at(time.Saturday, 3, 0), 1, 0, 200, 0},
		{"does not wrap into sunday", at(time.Sunday, 3, 0), -1, 50, 200, 0},
		{"end is exclusive", at(time.Monday, 18, 0), 2, 50, 200, 7},
	}
	for _, c := range cases {
		l, idx := s.Effective(c.t)
		if idx != c.rule || l.Upload != c.upl || l.MaxConns != c.conns || l.Download != c.down {
			t.Errorf("%s: got rule %d %+v", c.name, idx, l)
		}
		if l.MaxConnsPerTorrent != 50 || l.MaxActiveUploads != 4 {
			t.Errorf("%s: partial override clobbered untouched keys: %+v", c.name, l)
		}
	}
}

func TestWholeDayRule(t *testing.T) {
	r := Rule{Days: mustDays(t, "sun"), From: 0, To: 0}
	if !r.Matches(at(time.Sunday, 15, 0)) || r.Matches(at(time.Monday, 15, 0)) {
		t.Fatal("from == to means the whole listed day")
	}
}

package connbudget

import (
	"reflect"
	"testing"
)

func TestAllocate(t *testing.T) {
	cases := []struct {
		name string
		p    Params
		ts   []Torrent
		want map[string]int
	}{
		{"unlimited global = per torrent", Params{0, 50, false}, []Torrent{{"a", false, 0}, {"b", true, 0}}, map[string]int{"a": 50, "b": 50}},
		{"clamped to per torrent", Params{200, 50, false}, []Torrent{{"a", true, 0}, {"b", false, 0}, {"c", false, 0}}, map[string]int{"a": 50, "b": 50, "c": 50}},
		{"downloads weighted 2x", Params{60, 50, false}, []Torrent{{"a", true, 0}, {"b", false, 0}, {"c", false, 0}, {"d", false, 0}}, map[string]int{"a": 24, "b": 12, "c": 12, "d": 12}},
		{"floor of 4", Params{10, 50, false}, []Torrent{{"a", false, 0}, {"b", false, 0}, {"c", false, 0}, {"d", false, 0}, {"e", false, 0}}, map[string]int{"a": 4, "b": 4, "c": 4, "d": 4, "e": 4}},
		{"local conns on top", Params{60, 50, false}, []Torrent{{"a", false, 3}, {"b", false, 0}}, map[string]int{"a": 33, "b": 30}},
		{"local counted", Params{60, 50, true}, []Torrent{{"a", false, 3}, {"b", false, 0}}, map[string]int{"a": 30, "b": 30}},
		{"none", Params{60, 50, false}, nil, map[string]int{}},
	}
	for _, c := range cases {
		if got := Allocate(c.p, c.ts); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

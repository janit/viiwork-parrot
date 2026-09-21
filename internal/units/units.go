// Package units parses and formats transfer rates. Internally every rate is
// bytes per second; 0 means unlimited.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var rateUnits = map[string]float64{
	"": 1, "B": 1,
	"Kbit": 1e3 / 8, "Mbit": 1e6 / 8, "Gbit": 1e9 / 8,
	"KB": 1e3, "MB": 1e6, "GB": 1e9,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
}

// ParseRate parses "50Mbit", "10MB", "2MiB", "1000" (bytes/s) into bytes per
// second. Units are case-sensitive so "Mbit" and "MB" cannot be confused.
func ParseRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := strings.IndexFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.') })
	num, unit := s, ""
	if i >= 0 {
		num, unit = s[:i], strings.TrimSpace(s[i:])
	}
	if num == "" {
		return 0, fmt.Errorf("rate %q: missing number", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("rate %q: bad number", s)
	}
	mult, ok := rateUnits[unit]
	if !ok {
		return 0, fmt.Errorf("rate %q: unknown unit %q (use Kbit, Mbit, Gbit, KB, MB, GB, KiB, MiB, GiB)", s, unit)
	}
	return int64(math.Round(v * mult)), nil
}

// FormatRate renders bytes/s as bits/s for humans.
func FormatRate(bps int64) string {
	if bps == 0 {
		return "unlimited"
	}
	bits := float64(bps) * 8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.1fGbit", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1fMbit", bits/1e6)
	default:
		return fmt.Sprintf("%.0fKbit", bits/1e3)
	}
}

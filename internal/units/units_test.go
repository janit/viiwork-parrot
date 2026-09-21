package units

import "testing"

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0}, {"0", 0}, {"1000", 1000}, {"1000B", 1000},
		{"8Kbit", 1000}, {"50Mbit", 6_250_000}, {"1Gbit", 125_000_000},
		{"10KB", 10_000}, {"10MB", 10_000_000}, {"1GB", 1_000_000_000},
		{"1KiB", 1024}, {"2MiB", 2 << 20}, {"1GiB", 1 << 30},
		{"1.5MB", 1_500_000}, {" 20Mbit ", 2_500_000},
	}
	for _, c := range cases {
		got, err := ParseRate(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

func TestParseRateErrors(t *testing.T) {
	for _, in := range []string{"Mbit", "10 parsecs", "-5MB", "1.2.3MB", "10mbit"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q): expected error", in)
		}
	}
}

func TestFormatRate(t *testing.T) {
	cases := map[int64]string{0: "unlimited", 6_250_000: "50.0Mbit", 125_000_000: "1.0Gbit", 1000: "8Kbit"}
	for in, want := range cases {
		if got := FormatRate(in); got != want {
			t.Errorf("FormatRate(%d) = %q; want %q", in, got, want)
		}
	}
}

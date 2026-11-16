package garmin

import (
	"testing"
	"time"
)

func TestRangeChunks(t *testing.T) {
	t.Parallel()
	d := func(s string) time.Time {
		tm, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tests := []struct {
		name       string
		start, end string
		max        int
		want       [][2]string
	}{
		{"single day", "2026-11-15", "2026-11-15", 31, [][2]string{{"2026-11-15", "2026-11-15"}}},
		{"fits in one window", "2026-11-01", "2026-11-30", 31, [][2]string{{"2026-11-01", "2026-11-30"}}},
		{"exactly one full window", "2026-10-01", "2026-10-31", 31, [][2]string{{"2026-10-01", "2026-10-31"}}},
		{"one day over", "2026-10-01", "2026-11-01", 31, [][2]string{{"2026-10-01", "2026-10-31"}, {"2026-11-01", "2026-11-01"}}},
		{"three windows", "2026-01-01", "2026-03-10", 28, [][2]string{{"2026-01-01", "2026-01-28"}, {"2026-01-29", "2026-02-25"}, {"2026-02-26", "2026-03-10"}}},
		{"leap day", "2028-02-20", "2028-03-05", 7, [][2]string{{"2028-02-20", "2028-02-26"}, {"2028-02-27", "2028-03-04"}, {"2028-03-05", "2028-03-05"}}},
		{"window of one day", "2026-11-13", "2026-11-15", 1, [][2]string{{"2026-11-13", "2026-11-13"}, {"2026-11-14", "2026-11-14"}, {"2026-11-15", "2026-11-15"}}},
		{"non-positive max is treated as one", "2026-11-14", "2026-11-15", 0, [][2]string{{"2026-11-14", "2026-11-14"}, {"2026-11-15", "2026-11-15"}}},
		{"reversed range is empty", "2026-11-15", "2026-11-01", 31, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rangeChunks(d(tc.start), d(tc.end), tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d windows %v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				if dateParam(got[i][0]) != w[0] || dateParam(got[i][1]) != w[1] {
					t.Errorf("window %d = %s..%s, want %s..%s", i, dateParam(got[i][0]), dateParam(got[i][1]), w[0], w[1])
				}
			}
		})
	}
}

// Whatever the range, window size, or time zone (including daylight-saving
// changes), the windows must tile the range exactly: contiguous, in order,
// no gaps or overlaps, none larger than max.
func TestRangeChunks_TileTheRangeExactly(t *testing.T) {
	t.Parallel()
	zones := []string{"UTC", "Europe/Bucharest", "America/New_York", "Pacific/Auckland"}
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Skipf("tzdata missing for %s", z)
		}
		for _, startDay := range []int{0, 59, 85, 200, 300} { // around DST changes and a leap-year boundary
			start := time.Date(2027, 1, 1, 9, 30, 0, 0, loc).AddDate(0, 0, startDay)
			for _, span := range []int{1, 2, 27, 28, 29, 31, 32, 100, 365, 730} {
				end := start.AddDate(0, 0, span-1).Add(5 * time.Hour) // different time-of-day on purpose
				for _, max := range []int{1, 7, 28, 31, 300} {
					w := rangeChunks(start, end, max)
					wantCount := (span + max - 1) / max
					if len(w) != wantCount {
						t.Fatalf("%s start+%d span %d max %d: %d windows, want %d", z, startDay, span, max, len(w), wantCount)
					}
					if dateParam(w[0][0]) != dateParam(start) || dateParam(w[len(w)-1][1]) != dateParam(end) {
						t.Fatalf("%s: windows do not cover %s..%s: %v", z, dateParam(start), dateParam(end), w)
					}
					for i, win := range w {
						a, _ := time.Parse(time.DateOnly, dateParam(win[0]))
						b, _ := time.Parse(time.DateOnly, dateParam(win[1]))
						days := int(b.Sub(a).Hours()/24) + 1
						if days < 1 || days > max {
							t.Fatalf("%s: window %d spans %d days, max %d", z, i, days, max)
						}
						if i > 0 {
							prev, _ := time.Parse(time.DateOnly, dateParam(w[i-1][1]))
							if !a.Equal(prev.AddDate(0, 0, 1)) {
								t.Fatalf("%s: gap or overlap between windows %d and %d: %v", z, i-1, i, w)
							}
						}
					}
				}
			}
		}
	}
}

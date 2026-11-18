package garminconnect_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
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
		maxDays    int
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
			got := garminconnect.RangeChunks(d(tc.start), d(tc.end), tc.maxDays)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d windows %v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				if garminconnect.DateParam(got[i][0]) != w[0] || garminconnect.DateParam(got[i][1]) != w[1] {
					t.Errorf("window %d = %s..%s, want %s..%s", i, garminconnect.DateParam(got[i][0]), garminconnect.DateParam(got[i][1]), w[0], w[1])
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
				for _, maxDays := range []int{1, 7, 28, 31, 300} {
					w := garminconnect.RangeChunks(start, end, maxDays)
					wantCount := (span + maxDays - 1) / maxDays
					if len(w) != wantCount {
						t.Fatalf("%s start+%d span %d maxDays %d: %d windows, want %d", z, startDay, span, maxDays, len(w), wantCount)
					}
					if garminconnect.DateParam(w[0][0]) != garminconnect.DateParam(start) || garminconnect.DateParam(w[len(w)-1][1]) != garminconnect.DateParam(end) {
						t.Fatalf("%s: windows do not cover %s..%s: %v", z, garminconnect.DateParam(start), garminconnect.DateParam(end), w)
					}
					for i, win := range w {
						a, _ := time.Parse(time.DateOnly, garminconnect.DateParam(win[0]))
						b, _ := time.Parse(time.DateOnly, garminconnect.DateParam(win[1]))
						days := int(b.Sub(a).Hours()/24) + 1
						if days < 1 || days > maxDays {
							t.Fatalf("%s: window %d spans %d days, maxDays %d", z, i, days, maxDays)
						}
						if i > 0 {
							prev, _ := time.Parse(time.DateOnly, garminconnect.DateParam(w[i-1][1]))
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

func TestRangeSplitting_ShortRangesAreOneRequest(t *testing.T) {
	t.Parallel()
	rec := &windowRecorder{}
	c := newTestClient(t, rec.handler("startDate", "endDate", func(string) string { return `[]` }))
	if _, err := c.SleepRange(context.Background(), day("2026-11-01"), day("2026-11-15")); err != nil {
		t.Fatal(err)
	}
	if len(rec.windows) != 1 || rec.windows[0] != [2]string{"2026-11-01", "2026-11-15"} {
		t.Errorf("requests = %s", rec.summary())
	}
}

// The windows fetched before a failure are complete, so they are returned
// along with the error.
func TestRangeSplitting_StopsAtFirstErrorAndReturnsWhatItHad(t *testing.T) {
	t.Parallel()
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 2 {
			http.Error(w, "boom", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1}]`))
	}))
	got, err := c.SleepRange(context.Background(), day("2026-01-01"), day("2026-12-31"))
	if !errors.Is(err, garminconnect.ErrForbidden) || len(got) != 1 {
		t.Errorf("got %d results, err %v; want the first window's 1 and ErrForbidden", len(got), err)
	}
	if calls != 2 {
		t.Errorf("requests after the failure: calls = %d, want 2", calls)
	}
}

func TestRangeSplitting_RespectsCancellationAndValidation(t *testing.T) {
	t.Parallel()
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`[]`))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Steps(ctx, day("2026-01-01"), day("2026-12-31")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled err = %v", err)
	}
	if _, err := c.Steps(context.Background(), day("2026-12-31"), day("2026-01-01")); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("reversed err = %v", err)
	}
	if calls != 0 {
		t.Errorf("no request should be sent for a cancelled or invalid call (calls=%d)", calls)
	}
}

// --- Sleep records without a main sleep (naps / sleep-need only) -------------

func TestRangeSplitting_HRVAndVO2MaxSplitPast366Days(t *testing.T) {
	t.Parallel()
	var hrv windowRecorder
	c := newTestClient(t, hrv.handler("", "", func(string) string { return `{"hrvSummaries":[]}` }))
	if _, err := c.HRVRange(context.Background(), day("2025-01-01"), day("2026-06-30")); err != nil {
		t.Fatal(err)
	}
	if hrv.summary() != "2025-01-01..2026-01-01 2026-01-02..2026-06-30" {
		t.Errorf("HRV requests = %s", hrv.summary())
	}
	var vo2 windowRecorder
	c = newTestClient(t, vo2.handler("", "", func(string) string { return `[]` }))
	if _, err := c.VO2Max(context.Background(), day("2025-01-01"), day("2026-06-30"), "running"); err != nil {
		t.Fatal(err)
	}
	if len(vo2.windows) != 2 {
		t.Errorf("VO2 max requests = %s", vo2.summary())
	}
}

func TestRangeSplitting_RangesAreBounded(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	start := day("1926-01-01")
	end := day("2026-01-01")
	if _, err := c.DailySummaries(context.Background(), start, end); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("a 100-year range: err = %v, want ErrInvalidRange", err)
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests made for a range that was refused", calls.Load())
	}
	last := start.AddDate(0, 0, garminconnect.MaxRangeDays-1)
	if _, err := c.Steps(context.Background(), start, last); err != nil {
		t.Errorf("exactly MaxRangeDays: %v", err)
	}
}

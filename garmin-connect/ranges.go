package garminconnect

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidRange is returned when a date range ends before it starts, or
// spans more than MaxRangeDays.
var ErrInvalidRange = errors.New("garmin: invalid date range")

// MaxRangeDays is the longest range, in days, a range method accepts. Long
// ranges are split into several requests, so this bounds how many requests one
// call can make (13 for daily summaries, 131 for steps or stress).
const MaxRangeDays = 3660

// dateParam formats t as the YYYY-MM-DD value Garmin expects, using t's own
// location (so pass a date in the user's local time zone).
func dateParam(t time.Time) string { return t.Format(time.DateOnly) }

func checkRange(start, end time.Time) error {
	s, e := dateParam(start), dateParam(end)
	if e < s {
		return fmt.Errorf("%w: end %s is before start %s", ErrInvalidRange, e, s)
	}
	if days := rangeDays(start, end); days > MaxRangeDays {
		return fmt.Errorf("%w: %d days is more than the %d allowed", ErrInvalidRange, days, MaxRangeDays)
	}
	return nil
}

// rangeDays counts the calendar days in [start, end], inclusive, from the
// dates alone, so time of day and DST do not matter. It is at most zero when
// end is before start.
func rangeDays(start, end time.Time) int {
	sd, _ := time.Parse(time.DateOnly, dateParam(start))
	ed, _ := time.Parse(time.DateOnly, dateParam(end))
	return int(ed.Sub(sd).Hours()/24) + 1
}

// Garmin caps how many days one range request may span, and the caps differ
// per endpoint. They were measured against the live API (inclusive days, where
// a request one day over was rejected with HTTP 400):
//
//	steps, stress ........ 28 days
//	sleep range .......... 32 days (Garmin documents 31)
//	body battery ......... 31 days
//	daily summaries ...... fine at 330 days, HTTP 500 at 366
//
// Range methods split longer ranges into windows no larger than these sizes
// and join the results, so callers never see the limits. The windows sit at or
// below the measured caps, with extra margin for daily summaries where the real
// cap is uncertain. HRV and VO2 max showed no cap up to 366 days, the longest
// range tested, so they are split at that size.
const (
	maxStepsDays        = 28
	maxStressDays       = 28
	maxSleepRangeDays   = 31
	maxBodyBatteryDays  = 31
	maxDailySummaryDays = 300
	maxHRVDays          = 366
	maxVO2MaxDays       = 366
)

// rangeChunks splits the inclusive date range [start, end] into consecutive,
// non-overlapping windows of at most maxDays calendar days each.
func rangeChunks(start, end time.Time, maxDays int) [][2]time.Time {
	if maxDays < 1 {
		maxDays = 1
	}
	out := make([][2]time.Time, 0, max(rangeDays(start, end), 0)/maxDays+1)
	for cur := start; dateParam(cur) <= dateParam(end); {
		last := cur.AddDate(0, 0, maxDays-1)
		if dateParam(last) > dateParam(end) {
			last = end
		}
		out = append(out, [2]time.Time{cur, last})
		cur = last.AddDate(0, 0, 1)
	}
	return out
}

// fetchRange runs fetch over each window of [start, end] and joins the
// results in date order. It stops at the first error and returns what it had
// fetched until then along with the error: the earlier windows are complete,
// so a caller may use them.
func fetchRange[T any](ctx context.Context, start, end time.Time, maxDays int,
	fetch func(context.Context, time.Time, time.Time) ([]T, error)) ([]T, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var out []T
	for _, w := range rangeChunks(start, end, maxDays) {
		part, err := fetch(ctx, w[0], w[1])
		if err != nil {
			return out, err
		}
		out = append(out, part...)
	}
	return out, nil
}

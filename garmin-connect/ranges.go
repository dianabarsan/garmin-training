package garminconnect

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidRange is returned when a date range ends before it starts.
var ErrInvalidRange = errors.New("garmin: end date is before start date")

// dateParam formats t as the YYYY-MM-DD value Garmin expects, using t's own
// location (so pass a date in the user's local time zone).
func dateParam(t time.Time) string { return t.Format(time.DateOnly) }

func checkRange(start, end time.Time) error {
	if dateParam(end) < dateParam(start) {
		return fmt.Errorf("%w: %s < %s", ErrInvalidRange, dateParam(end), dateParam(start))
	}
	return nil
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
// cap is uncertain. HRV and VO2 max showed no cap up to 366 days and are sent
// as a single request.
const (
	maxStepsDays        = 28
	maxStressDays       = 28
	maxSleepRangeDays   = 31
	maxBodyBatteryDays  = 31
	maxDailySummaryDays = 300
)

// rangeChunks splits the inclusive date range [start, end] into consecutive,
// non-overlapping windows of at most maxDays calendar days each.
func rangeChunks(start, end time.Time, maxDays int) [][2]time.Time {
	if maxDays < 1 {
		maxDays = 1
	}
	var out [][2]time.Time
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
// results in date order. It stops at the first error.
func fetchRange[T any](ctx context.Context, start, end time.Time, maxDays int,
	fetch func(context.Context, time.Time, time.Time) ([]T, error)) ([]T, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var out []T
	for _, w := range rangeChunks(start, end, maxDays) {
		part, err := fetch(ctx, w[0], w[1])
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

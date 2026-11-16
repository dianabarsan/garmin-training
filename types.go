package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidRange is returned when a date range ends before it starts.
var ErrInvalidRange = errors.New("garmin: end date is before start date")

// rawJSON is embedded in response models so callers can reach fields this
// package does not model. Garmin objects carry dozens of fields and publish
// no schema, so the models cover the useful ones and keep the original.
type rawJSON struct{ raw json.RawMessage }

// Raw returns the response object exactly as Garmin sent it.
func (r rawJSON) Raw() json.RawMessage { return r.raw }

func (r *rawJSON) setRaw(b json.RawMessage) { r.raw = append(json.RawMessage(nil), b...) }

type rawSetter[T any] interface {
	*T
	setRaw(json.RawMessage)
}

// decodeOne unmarshals b into T and retains the raw bytes.
func decodeOne[T any, P rawSetter[T]](b json.RawMessage) (T, error) {
	var v T
	if len(b) == 0 { // HTTP 204: Garmin has no data
		return v, nil
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, err
	}
	P(&v).setRaw(b)
	return v, nil
}

// decodeList unmarshals a JSON array of objects, retaining each raw element.
// A JSON null decodes to a nil slice.
func decodeList[T any, P rawSetter[T]](b json.RawMessage) ([]T, error) {
	if len(b) == 0 { // HTTP 204: Garmin has no data
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, err
	}
	if raws == nil {
		return nil, nil
	}
	out := make([]T, 0, len(raws))
	for _, r := range raws {
		v, err := decodeOne[T, P](r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Timestamp is a UTC instant. Garmin encodes times either as strings such as
// "2026-11-15 06:30:00.0" (always GMT here) or as epoch milliseconds; both
// decode into this type. A null or empty value yields the zero time.
type Timestamp struct{ time.Time }

var timestampLayouts = []string{
	"2006-01-02 15:04:05", // fractional seconds are accepted when parsing
	"2006-01-02T15:04:05",
	time.RFC3339,
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null" || s == `""`:
		*t = Timestamp{}
		return nil
	case s[0] == '"':
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		for _, layout := range timestampLayouts {
			if tm, err := time.ParseInLocation(layout, str, time.UTC); err == nil {
				*t = Timestamp{tm.UTC()}
				return nil
			}
		}
		return fmt.Errorf("garmin: unrecognised timestamp %q", str)
	default:
		ms, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("garmin: unrecognised timestamp %s", s)
		}
		*t = Timestamp{time.UnixMilli(int64(ms)).UTC()}
		return nil
	}
}

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

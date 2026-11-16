package garmin

import (
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

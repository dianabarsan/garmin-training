package garminconnect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// rawJSON is embedded in response models so callers can reach fields this
// package does not model. Garmin objects carry dozens of fields and publish
// no schema, so the models cover the useful ones and keep the original.
type rawJSON struct{ raw json.RawMessage }

// Raw returns the response object exactly as Garmin sent it. The result is a
// copy: changing it does not affect the model.
func (r rawJSON) Raw() json.RawMessage { return bytes.Clone(r.raw) }

func (r *rawJSON) setRaw(b json.RawMessage) { r.raw = append(json.RawMessage(nil), b...) }

type rawSetter[T any] interface {
	*T
	setRaw(json.RawMessage)
}

// decodeOne unmarshals b into T and retains the raw bytes. An empty body
// (HTTP 204) or a JSON null means Garmin has no data and gives the zero value.
func decodeOne[T any, P rawSetter[T]](b json.RawMessage) (T, error) {
	var v T
	if t := bytes.TrimSpace(b); len(t) == 0 || string(t) == "null" {
		return v, nil
	}
	if err := unmarshalLenient(b, &v); err != nil {
		var zero T
		return zero, err
	}
	P(&v).setRaw(b)
	return v, nil
}

// unmarshalLenient is json.Unmarshal, except that a whole number written with
// a fraction or exponent ("1234.0", "1.2e3") still decodes into an integer
// field. Garmin is not consistent about this, and one such value would
// otherwise fail an entire response. A real fraction ("12.5") into an integer
// field is still an error.
func unmarshalLenient(b []byte, v any) error {
	err := json.Unmarshal(b, v)
	var te *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &te) || !strings.HasPrefix(te.Value, "number") {
		return err
	}
	normalized, ok := integralNumbers(b)
	if !ok {
		return err
	}
	if rerr := json.Unmarshal(normalized, v); rerr != nil {
		return err // report the original problem
	}
	return nil
}

// integralNumbers rewrites every whole number in b that has a fraction or
// exponent into plain integer form. ok is false when nothing changed.
func integralNumbers(b []byte) (out []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var tree any
	if dec.Decode(&tree) != nil {
		return nil, false
	}
	changed := false
	var walk func(any) any
	walk = func(x any) any {
		switch v := x.(type) {
		case map[string]any:
			for k, e := range v {
				v[k] = walk(e)
			}
		case []any:
			for i, e := range v {
				v[i] = walk(e)
			}
		case json.Number:
			s := string(v)
			if !strings.ContainsAny(s, ".eE") {
				return v
			}
			f, err := v.Float64()
			if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
				return v
			}
			changed = true
			return json.Number(strconv.FormatInt(int64(f), 10))
		}
		return x
	}
	tree = walk(tree)
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(tree)
	return out, err == nil
}

// decodeList unmarshals a JSON array of objects, retaining each raw element.
// A JSON null decodes to a nil slice, and a null element is skipped.
func decodeList[T any, P rawSetter[T]](b json.RawMessage) ([]T, error) {
	if len(bytes.TrimSpace(b)) == 0 { // HTTP 204: Garmin has no data
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
	for i, r := range raws {
		if string(bytes.TrimSpace(r)) == "null" {
			continue
		}
		v, err := decodeOne[T, P](r)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
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
		tm, ok := unixMilli(ms)
		if err != nil || !ok {
			return fmt.Errorf("garmin: unrecognised timestamp %s", s)
		}
		*t = Timestamp{tm}
		return nil
	}
}

// maxUnixMilli bounds epoch-millisecond values to years 0 to 9999, so an
// absurd number cannot overflow when converted.
const maxUnixMilli = 253402300799999

// unixMilli converts epoch milliseconds to a UTC time. ok is false for a
// value outside years 0 to 9999, or NaN.
func unixMilli(ms float64) (time.Time, bool) {
	if !(ms >= -62167219200000 && ms <= maxUnixMilli) {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

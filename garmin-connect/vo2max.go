package garminconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// VO2MaxReading is one VO2 max estimate in ml/kg/min.
type VO2MaxReading struct {
	CalendarDate string `json:"calendarDate"` // YYYY-MM-DD
	// Sport is the sport the estimate is for, e.g. "RUNNING"; SubSport
	// refines it, e.g. "GENERIC".
	Sport    string `json:"sport"`
	SubSport string `json:"subSport"`
	// Value is Garmin's rounded figure (a whole number) and Precise the same
	// estimate to one decimal place.
	Value   float64 `json:"vo2MaxValue"`
	Precise float64 `json:"vo2MaxPreciseValue"`
}

// VO2MaxDay is one dated entry of the VO2 max history. Garmin adds an entry
// when it recomputes the estimate after a qualifying activity, not every day,
// so gaps of several days are normal.
type VO2MaxDay struct {
	rawJSON
	// Generic is Garmin's general estimate (running-based on watches that
	// report one). Cycling is the cycling-specific estimate and is nil when
	// Garmin has none. The cycling object's shape has not been verified: the
	// account this was built against never had one, so read Raw if it matters.
	Generic *VO2MaxReading `json:"generic"`
	Cycling *VO2MaxReading `json:"cycling"`
}

// VO2Max returns the VO2 max history for [start, end], oldest first. Passing
// sports (such as "running" or "cycling", case-insensitive) keeps only entries
// that carry an estimate for one of them; an unknown sport is rejected by
// Garmin with HTTP 400. A range with no entries returns an empty slice.
func (c *Client) VO2Max(ctx context.Context, start, end time.Time, sports ...string) ([]VO2MaxDay, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var q url.Values
	for _, s := range sports {
		if q == nil {
			q = url.Values{}
		}
		q.Add("sport", s)
	}
	var raw json.RawMessage
	path := "/metrics-service/metrics/maxmet/daily/" + dateParam(start) + "/" + dateParam(end)
	if err := c.get(ctx, path, q, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[VO2MaxDay](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding VO2 max: %w", err)
	}
	return out, nil
}

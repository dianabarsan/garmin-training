package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

// Errors returned by Splits.
var (
	// ErrInvalidSplitSize is returned for a non-positive split distance.
	ErrInvalidSplitSize = errors.New("garmin: split distance must be positive")
	// ErrNoDistance is returned when a series has no distance data, e.g. a
	// strength workout or an indoor activity without a speed sensor.
	ErrNoDistance = errors.New("garmin: activity series has no distance data")
)

// elevationDeadband is the smallest altitude change, in metres, counted
// toward elevation gain or loss. Raw altimeter samples jitter, so summing every
// rise overstates gain badly (72 m against Garmin's 15 m on a flat run). A 1 m
// dead-band reproduced Garmin's totals on both real runs it was checked
// against (125/124 m reported vs 125/121 m computed, and 15/11 vs 17/15).
const elevationDeadband = 1.0

// seriesChartSize is the first-try resolution requested from Garmin. It covers
// activities up to about 5.5 hours at one sample per second; longer ones are
// re-requested using the total Garmin reports.
const seriesChartSize = 20000

// SeriesPoint is one sample of an activity. At full resolution Garmin records
// one per second. Optional measurements are nil when the sensor reported
// nothing at that instant. GPS coordinates are deliberately not decoded.
type SeriesPoint struct {
	Time Timestamp
	// Duration is timer seconds since the activity started (pauses excluded).
	Duration float64
	// Distance is cumulative metres since the start.
	Distance  float64
	HeartRate *float64 // bpm
	Speed     *float64 // m/s
	Elevation *float64 // metres
	// Cadence is total steps per minute, from Garmin's "directDoubleCadence".
	// The similarly named "directRunCadence" is per foot, half this value.
	Cadence *float64
	Power   *float64 // watts
}

// ActivitySeries is an activity's per-sample metrics.
type ActivitySeries struct {
	Points []SeriesPoint
	// Total is the number of samples Garmin holds for the activity.
	Total int
	// Downsampled reports whether Points holds fewer samples than Total.
	// ActivitySeries requests full resolution, so this is normally false.
	Downsampled bool
}

// UnmarshalJSON implements json.Unmarshaler. Columns are located by the
// response's own metric descriptors, never by position.
func (s *ActivitySeries) UnmarshalJSON(b []byte) error {
	var aux struct {
		Rows []struct {
			Metrics []*float64 `json:"metrics"`
		} `json:"activityDetailMetrics"`
		Descriptors []struct {
			Key   string `json:"key"`
			Index int    `json:"metricsIndex"`
		} `json:"metricDescriptors"`
		Count int `json:"metricsCount"`
		Total int `json:"totalMetricsCount"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	idx := make(map[string]int, len(aux.Descriptors))
	for _, d := range aux.Descriptors {
		idx[d.Key] = d.Index
	}
	col := func(row []*float64, key string) *float64 {
		i, ok := idx[key]
		if !ok || i < 0 || i >= len(row) {
			return nil
		}
		return row[i]
	}

	s.Points = make([]SeriesPoint, 0, len(aux.Rows))
	for _, r := range aux.Rows {
		dur := col(r.Metrics, "sumDuration")
		if dur == nil {
			continue // a row without a clock cannot be placed in time
		}
		p := SeriesPoint{
			Duration:  *dur,
			HeartRate: col(r.Metrics, "directHeartRate"),
			Speed:     col(r.Metrics, "directSpeed"),
			Elevation: col(r.Metrics, "directElevation"),
			Cadence:   col(r.Metrics, "directDoubleCadence"),
			Power:     col(r.Metrics, "directPower"),
		}
		if d := col(r.Metrics, "sumDistance"); d != nil {
			p.Distance = *d
		}
		if ts := col(r.Metrics, "directTimestamp"); ts != nil {
			p.Time = Timestamp{Time: time.UnixMilli(int64(*ts)).UTC()}
		}
		s.Points = append(s.Points, p)
	}
	s.Total = aux.Total
	s.Downsampled = aux.Total > len(s.Points) && aux.Count < aux.Total
	return nil
}

// ActivitySeries returns the activity's per-sample metrics at full
// resolution. It fetches the GPS track's endpoints too (Garmin offers no way
// to omit them) but does not decode any coordinates.
func (c *Client) ActivitySeries(ctx context.Context, id int64) (ActivitySeries, error) {
	path := "/activity-service/activity/" + strconv.FormatInt(id, 10) + "/details"
	fetch := func(size int) (ActivitySeries, error) {
		q := url.Values{"maxChartSize": {strconv.Itoa(size)}, "maxPolylineSize": {"2"}}
		var s ActivitySeries
		if err := c.get(ctx, path, q, &s); err != nil {
			return ActivitySeries{}, err
		}
		return s, nil
	}

	s, err := fetch(seriesChartSize)
	if err != nil {
		return ActivitySeries{}, fmt.Errorf("garmin: activity %d series: %w", id, err)
	}
	if s.Downsampled {
		// Longer than the first-try size: ask for everything Garmin holds.
		if s, err = fetch(s.Total); err != nil {
			return ActivitySeries{}, fmt.Errorf("garmin: activity %d series: %w", id, err)
		}
	}
	return s, nil
}

// Split is one fixed-distance segment of an activity. Averages are plain means
// of the samples inside the split. HeartRate and Cadence figures are zero when
// the activity recorded none.
type Split struct {
	Index    int     `json:"index"`    // 1-based
	Distance float64 `json:"distance"` // metres; less than the split size when Partial
	Duration float64 `json:"duration"` // timer seconds
	// AvgSpeed is Distance/Duration in m/s.
	AvgSpeed      float64 `json:"avgSpeed"`
	AvgHeartRate  float64 `json:"avgHeartRate"`
	MaxHeartRate  float64 `json:"maxHeartRate"`
	AvgCadence    float64 `json:"avgCadence"`
	ElevationGain float64 `json:"elevationGain"` // metres, 1 m dead-band
	ElevationLoss float64 `json:"elevationLoss"` // metres, 1 m dead-band
	// Partial marks the final, shorter segment left over after the last full one.
	Partial bool `json:"partial"`
}

// PaceSecondsPerKm returns the split's pace, or 0 when it has no distance.
func (s Split) PaceSecondsPerKm() float64 {
	if s.Distance <= 0 {
		return 0
	}
	return s.Duration / s.Distance * 1000
}

// Splits divides the activity into consecutive segments of splitMeters
// (1000 for kilometre splits, 1609.344 for miles). Split boundaries are placed
// by linear interpolation between samples, so durations are accurate even when
// the series is downsampled. Elevation gain and loss ignore changes smaller
// than 1 m (see elevationDeadband), which approximates Garmin's own totals.
//
// A final shorter segment is included with Partial set; callers that only want
// full splits can drop it.
func (s ActivitySeries) Splits(splitMeters float64) ([]Split, error) {
	if !(splitMeters > 0) || math.IsInf(splitMeters, 0) {
		return nil, ErrInvalidSplitSize
	}

	// Keep samples with non-decreasing distance; a backwards step is a glitch.
	pts := make([]SeriesPoint, 0, len(s.Points))
	for _, p := range s.Points {
		if len(pts) > 0 && p.Distance < pts[len(pts)-1].Distance {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) < 2 || pts[len(pts)-1].Distance <= 0 {
		return nil, ErrNoDistance
	}

	// Boundary clock times: bounds[k] is the timer at distance k*splitMeters.
	bounds := []float64{pts[0].Duration}
	next := splitMeters
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		if b.Distance <= a.Distance {
			continue // stationary: no distance covered, no boundary crossed
		}
		for next <= b.Distance {
			frac := (next - a.Distance) / (b.Distance - a.Distance)
			bounds = append(bounds, a.Duration+frac*(b.Duration-a.Duration))
			next = float64(len(bounds)) * splitMeters
		}
	}

	last := pts[len(pts)-1]
	full := len(bounds) - 1
	remainder := last.Distance - float64(full)*splitMeters
	count := full
	if remainder > 1e-6 {
		count++
	}
	if count == 0 {
		return nil, ErrNoDistance
	}

	type agg struct {
		hrSum, hrMax, cadSum float64
		hrN, cadN            int
		gain, loss           float64
	}
	aggs := make([]agg, count)
	// ref is the last altitude that counted; a change is only recorded once it
	// moves at least elevationDeadband away from it.
	var ref *float64
	for _, p := range pts {
		if p.Distance <= 0 {
			if p.Elevation != nil && ref == nil {
				ref = p.Elevation
			}
			continue
		}
		k := int(math.Ceil(p.Distance/splitMeters)) - 1
		k = min(max(k, 0), count-1)
		a := &aggs[k]
		if p.HeartRate != nil {
			a.hrSum += *p.HeartRate
			a.hrMax = max(a.hrMax, *p.HeartRate)
			a.hrN++
		}
		if p.Cadence != nil {
			a.cadSum += *p.Cadence
			a.cadN++
		}
		if p.Elevation != nil {
			switch {
			case ref == nil:
				ref = p.Elevation
			case *p.Elevation-*ref >= elevationDeadband:
				a.gain += *p.Elevation - *ref
				ref = p.Elevation
			case *ref-*p.Elevation >= elevationDeadband:
				a.loss += *ref - *p.Elevation
				ref = p.Elevation
			}
		}
	}

	out := make([]Split, count)
	for k := range out {
		startT := bounds[k]
		endT := last.Duration
		dist := remainder
		if k < full {
			endT = bounds[k+1]
			dist = splitMeters
		}
		sp := Split{
			Index:         k + 1,
			Distance:      dist,
			Duration:      endT - startT,
			ElevationGain: aggs[k].gain,
			ElevationLoss: aggs[k].loss,
			MaxHeartRate:  aggs[k].hrMax,
			Partial:       k >= full,
		}
		if sp.Duration > 0 {
			sp.AvgSpeed = dist / sp.Duration
		}
		if aggs[k].hrN > 0 {
			sp.AvgHeartRate = aggs[k].hrSum / float64(aggs[k].hrN)
		}
		if aggs[k].cadN > 0 {
			sp.AvgCadence = aggs[k].cadSum / float64(aggs[k].cadN)
		}
		out[k] = sp
	}
	return out, nil
}

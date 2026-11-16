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
	pts := monotonicPoints(s.Points)
	if len(pts) < 2 || pts[len(pts)-1].Distance <= 0 {
		return nil, ErrNoDistance
	}

	bounds := splitBoundaries(pts, splitMeters)
	last := pts[len(pts)-1]
	full := len(bounds) - 1 // splits that reach the full distance
	remainder := last.Distance - float64(full)*splitMeters
	count := full
	if remainder > partialEpsilon {
		count++ // a final, shorter split
	}
	if count == 0 {
		return nil, ErrNoDistance
	}

	aggs := aggregateSplits(pts, splitMeters, count)
	return buildSplits(aggs, bounds, last.Duration, splitMeters, full, remainder), nil
}

// partialEpsilon is the smallest leftover distance, in metres, that counts as
// a final partial split rather than rounding noise.
const partialEpsilon = 1e-6

// monotonicPoints keeps the samples whose distance never decreases; a step
// backwards is a GPS glitch.
func monotonicPoints(in []SeriesPoint) []SeriesPoint {
	pts := make([]SeriesPoint, 0, len(in))
	for _, p := range in {
		if len(pts) > 0 && p.Distance < pts[len(pts)-1].Distance {
			continue
		}
		pts = append(pts, p)
	}
	return pts
}

// splitBoundaries returns the timer reading at each whole multiple of
// splitMeters, found by linear interpolation between the samples either side:
// bounds[k] is the timer at distance k*splitMeters, and bounds[0] is the first
// sample's timer.
func splitBoundaries(pts []SeriesPoint, splitMeters float64) []float64 {
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
	return bounds
}

// splitAgg accumulates one split's sensor readings.
type splitAgg struct {
	hrSum, hrMax, cadSum float64
	hrN, cadN            int
	gain, loss           float64
}

// addSensors folds a sample's heart rate and cadence into the totals.
func (a *splitAgg) addSensors(p SeriesPoint) {
	if p.HeartRate != nil {
		a.hrSum += *p.HeartRate
		a.hrMax = max(a.hrMax, *p.HeartRate)
		a.hrN++
	}
	if p.Cadence != nil {
		a.cadSum += *p.Cadence
		a.cadN++
	}
}

// elevationTracker turns noisy altitude samples into gain and loss. It
// remembers the last altitude that counted, and records a change only once the
// altitude has moved at least elevationDeadband away from it.
type elevationTracker struct{ ref *float64 }

// seed sets the starting altitude if there is none yet.
func (t *elevationTracker) seed(e *float64) {
	if e != nil && t.ref == nil {
		t.ref = e
	}
}

// step feeds one altitude and returns the gain or loss it completes, if any.
func (t *elevationTracker) step(e *float64) (gain, loss float64) {
	switch {
	case t.ref == nil:
		t.ref = e
	case *e-*t.ref >= elevationDeadband:
		gain = *e - *t.ref
		t.ref = e
	case *t.ref-*e >= elevationDeadband:
		loss = *t.ref - *e
		t.ref = e
	}
	return gain, loss
}

// splitIndex is the zero-based split a distance falls in. A distance exactly
// on a boundary belongs to the split that ends there.
func splitIndex(distance, splitMeters float64, count int) int {
	k := int(math.Ceil(distance/splitMeters)) - 1
	return min(max(k, 0), count-1)
}

// aggregateSplits assigns every sample to its split and totals heart rate,
// cadence and elevation change per split.
func aggregateSplits(pts []SeriesPoint, splitMeters float64, count int) []splitAgg {
	aggs := make([]splitAgg, count)
	var track elevationTracker
	for _, p := range pts {
		if p.Distance <= 0 {
			track.seed(p.Elevation)
			continue
		}
		a := &aggs[splitIndex(p.Distance, splitMeters, count)]
		a.addSensors(p)
		if p.Elevation != nil {
			g, l := track.step(p.Elevation)
			a.gain += g
			a.loss += l
		}
	}
	return aggs
}

// buildSplits turns boundaries and totals into Split values. The first full
// splits span splitMeters each; a final split, when present, spans remainder.
func buildSplits(aggs []splitAgg, bounds []float64, endDuration, splitMeters float64, full int, remainder float64) []Split {
	out := make([]Split, len(aggs))
	for k, a := range aggs {
		startT, endT, dist := bounds[k], endDuration, remainder
		if k < full {
			endT, dist = bounds[k+1], splitMeters
		}
		sp := Split{
			Index:         k + 1,
			Distance:      dist,
			Duration:      endT - startT,
			ElevationGain: a.gain,
			ElevationLoss: a.loss,
			MaxHeartRate:  a.hrMax,
			Partial:       k >= full,
		}
		if sp.Duration > 0 {
			sp.AvgSpeed = dist / sp.Duration
		}
		if a.hrN > 0 {
			sp.AvgHeartRate = a.hrSum / float64(a.hrN)
		}
		if a.cadN > 0 {
			sp.AvgCadence = a.cadSum / float64(a.cadN)
		}
		out[k] = sp
	}
	return out
}

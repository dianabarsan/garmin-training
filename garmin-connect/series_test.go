package garminconnect_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func fp(v float64) *float64 { return &v }

// runner builds a series of a runner at constant speed, sampled every
// interval seconds until totalDistance metres are covered.
func runner(speed, totalDistance, interval float64) garminconnect.ActivitySeries {
	var s garminconnect.ActivitySeries
	for t := 0.0; ; t += interval {
		d := min(speed*t, totalDistance)
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: t, Distance: d})
		if d >= totalDistance {
			break
		}
	}
	return s
}

func TestSplits_ConstantSpeed(t *testing.T) {
	t.Parallel()
	got, err := runner(4, 2500, 1).Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d splits, want 3: %+v", len(got), got)
	}
	want := []struct {
		dist, dur float64
		partial   bool
	}{{1000, 250, false}, {1000, 250, false}, {500, 125, true}}
	for i, w := range want {
		g := got[i]
		if g.Index != i+1 || !near(g.Distance, w.dist, 1e-9) || !near(g.Duration, w.dur, 1e-9) || g.Partial != w.partial {
			t.Errorf("split %d = %+v, want dist=%v dur=%v partial=%v", i+1, g, w.dist, w.dur, w.partial)
		}
		if !near(g.AvgSpeed, 4, 1e-9) || !near(g.PaceSecondsPerKm(), 250, 1e-9) {
			t.Errorf("split %d speed/pace = %v / %v", i+1, g.AvgSpeed, g.PaceSecondsPerKm())
		}
	}
}

func TestSplits_InterpolatesBetweenSparseSamples(t *testing.T) {
	t.Parallel()
	// One sample every 7 s: the 1000 m boundary (t=250) falls between samples.
	// The series runs past 3 km so the first three splits are all interior;
	// the final sample's capped distance would otherwise skew the last one.
	got, err := runner(4, 3500, 7).Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("got %d splits", len(got))
	}
	for i := range 3 {
		if !near(got[i].Duration, 250, 1e-9) {
			t.Errorf("split %d duration = %v, want 250 despite sparse samples", i+1, got[i].Duration)
		}
	}
}

func TestSplits_ExactBoundaryHasNoPartial(t *testing.T) {
	t.Parallel()
	got, err := runner(4, 2000, 1).Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Partial {
		t.Errorf("splits = %+v, want exactly 2 full", got)
	}
}

func TestSplits_ShorterThanOneSplit(t *testing.T) {
	t.Parallel()
	got, err := runner(4, 400, 1).Splits(1000)
	if err != nil || len(got) != 1 || !got[0].Partial || !near(got[0].Distance, 400, 1e-9) || !near(got[0].Duration, 100, 1e-9) {
		t.Errorf("got %+v err %v", got, err)
	}
}

func TestSplits_Miles(t *testing.T) {
	t.Parallel()
	const mile = 1609.344
	got, err := runner(4, 5000, 1).Splits(mile)
	if err != nil || len(got) != 4 { // 3 full miles + partial
		t.Fatalf("got %d err %v", len(got), err)
	}
	if !near(got[0].Duration, mile/4, 1e-9) {
		t.Errorf("mile duration = %v", got[0].Duration)
	}
}

func TestSplits_StandingStillCountsTowardTime(t *testing.T) {
	t.Parallel()
	// 500 m at 4 m/s (125 s), stand 20 s, then 500 m more (125 s).
	var s garminconnect.ActivitySeries
	for t := 0; t <= 125; t++ {
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: float64(t), Distance: 4 * float64(t)})
	}
	for t := 126; t <= 145; t++ {
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: float64(t), Distance: 500})
	}
	for t := 146; t <= 270; t++ {
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: float64(t), Distance: 500 + 4*float64(t-145)})
	}
	got, err := s.Splits(1000)
	if err != nil || len(got) != 1 || !near(got[0].Duration, 270, 1e-9) {
		t.Errorf("got %+v err %v, want one 270 s split", got, err)
	}
}

func TestSplits_IgnoresBackwardsDistanceGlitch(t *testing.T) {
	t.Parallel()
	s := runner(4, 2000, 1)
	s.Points[100].Distance = 5 // glitch: jumps back near zero
	got, err := s.Splits(1000)
	if err != nil || len(got) != 2 || !near(got[0].Duration, 250, 1e-9) {
		t.Errorf("got %+v err %v", got, err)
	}
}

func TestSplits_Aggregates(t *testing.T) {
	t.Parallel()
	// 4 m/s for 500 s = 2000 m. Split 1: HR 100, cadence 170, climbing 0.1 m/s.
	// Split 2: HR 150 (peak 160 at one sample), cadence 180, descending 0.2 m/s.
	var s garminconnect.ActivitySeries
	for t := 0; t <= 500; t++ {
		ft := float64(t)
		p := garminconnect.SeriesPoint{Duration: ft, Distance: 4 * ft}
		if t <= 250 {
			p.HeartRate, p.Cadence, p.Elevation = fp(100), fp(170), fp(0.1*ft)
		} else {
			hr := 150.0
			if t == 400 {
				hr = 160
			}
			p.HeartRate, p.Cadence, p.Elevation = fp(hr), fp(180), fp(25-0.2*(ft-250))
		}
		s.Points = append(s.Points, p)
	}
	got, err := s.Splits(1000)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if !near(got[0].AvgHeartRate, 100, 1e-9) || !near(got[0].MaxHeartRate, 100, 1e-9) || !near(got[0].AvgCadence, 170, 1e-9) {
		t.Errorf("split 1 = %+v", got[0])
	}
	if !near(got[0].ElevationGain, 25, 1e-6) || !near(got[0].ElevationLoss, 0, 1e-6) {
		t.Errorf("split 1 elevation = +%v -%v", got[0].ElevationGain, got[0].ElevationLoss)
	}
	if got[1].MaxHeartRate != 160 || got[1].AvgHeartRate < 150 || got[1].AvgHeartRate > 151 || !near(got[1].AvgCadence, 180, 1e-9) {
		t.Errorf("split 2 = %+v", got[1])
	}
	if !near(got[1].ElevationLoss, 50, 1e-6) || !near(got[1].ElevationGain, 0, 1e-6) {
		t.Errorf("split 2 elevation = +%v -%v", got[1].ElevationGain, got[1].ElevationLoss)
	}
}

func TestSplits_MissingSensorsGiveZeros(t *testing.T) {
	t.Parallel()
	got, err := runner(4, 1500, 1).Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.AvgHeartRate != 0 || g.MaxHeartRate != 0 || g.AvgCadence != 0 || g.ElevationGain != 0 {
			t.Errorf("split %d should have zero sensor fields: %+v", g.Index, g)
		}
	}
}

func TestSplits_Errors(t *testing.T) {
	t.Parallel()
	good := runner(4, 2000, 1)
	for _, size := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := good.Splits(size); !errors.Is(err, garminconnect.ErrInvalidSplitSize) {
			t.Errorf("size %v: err = %v", size, err)
		}
	}
	noDist := garminconnect.ActivitySeries{Points: []garminconnect.SeriesPoint{{Duration: 0}, {Duration: 1}, {Duration: 2}}}
	for name, s := range map[string]garminconnect.ActivitySeries{
		"empty":       {},
		"one point":   {Points: []garminconnect.SeriesPoint{{Duration: 1, Distance: 5}}},
		"no distance": noDist,
	} {
		if _, err := s.Splits(1000); !errors.Is(err, garminconnect.ErrNoDistance) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSplit_PaceZeroWithoutDistance(t *testing.T) {
	t.Parallel()
	if got := (garminconnect.Split{Duration: 10}).PaceSecondsPerKm(); got != 0 {
		t.Errorf("pace = %v", got)
	}
}

// Whatever the sampling, split durations and distances must add up to the
// whole activity.
func TestSplits_TotalsAreConserved(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 200 {
		var s garminconnect.ActivitySeries
		tm, d := 0.0, 0.0
		s.Points = append(s.Points, garminconnect.SeriesPoint{})
		for d < 500+rng.Float64()*9000 {
			tm += 1 + rng.Float64()*20
			if rng.IntN(10) != 0 { // sometimes stand still
				d += rng.Float64() * 60
			}
			s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: tm, Distance: d})
		}
		size := []float64{400, 1000, 1609.344}[trial%3]
		got, err := s.Splits(size)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		var sumD, sumT float64
		for i, g := range got {
			if g.Duration < -1e-9 {
				t.Fatalf("trial %d split %d negative duration %v", trial, i, g.Duration)
			}
			if g.Partial != (i == len(got)-1 && g.Distance < size-1e-6) {
				t.Fatalf("trial %d: Partial flag wrong on split %d: %+v", trial, i, g)
			}
			sumD += g.Distance
			sumT += g.Duration
		}
		if !near(sumD, d, 1e-6) || !near(sumT, tm, 1e-6) {
			t.Fatalf("trial %d: totals %v m / %v s, want %v / %v", trial, sumD, sumT, d, tm)
		}
	}
}

// seriesBody builds a /details response with descriptors in a scrambled order.
func seriesBody(rows [][]any, count, total int) string {
	keys := []string{"directTimestamp", "sumDistance", "directHeartRate", "sumDuration", "directSpeed", "directElevation", "directDoubleCadence", "directPower", "directLatitude", "directLongitude"}
	var desc []map[string]any
	for i, k := range keys {
		desc = append(desc, map[string]any{"key": k, "metricsIndex": i})
	}
	var out []map[string]any
	for _, r := range rows {
		out = append(out, map[string]any{"metrics": r})
	}
	b, _ := json.Marshal(map[string]any{
		"activityDetailMetrics": out, "metricDescriptors": desc,
		"metricsCount": count, "totalMetricsCount": total,
		"geoPolylineDTO": map[string]any{"polyline": []any{}},
	})
	return string(b)
}

func TestActivitySeries_DecodesByDescriptor(t *testing.T) {
	t.Parallel()
	//          ts            dist  hr   dur  spd  elev cad  pow  lat  lon
	body := seriesBody([][]any{
		{1794639647000, 0.0, 120, 0.0, 0.0, 110.5, 80, 200, 46.0, 24.0},
		{1794639648000, 3.5, nil, 1.0, 3.5, nil, nil, nil, 46.0, 24.0},
		{nil, 7.0, 130, nil, 3.5, 111, 82, 210, 46.0, 24.0}, // no clock: skipped
	}, 3, 3)
	c, _ := serve(t, body)
	s, err := c.ActivitySeries(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Points) != 2 || s.Total != 3 || s.Downsampled {
		t.Fatalf("points=%d total=%d downsampled=%v", len(s.Points), s.Total, s.Downsampled)
	}
	p0, p1 := s.Points[0], s.Points[1]
	if p0.Time.UnixMilli() != 1794639647000 || p0.Distance != 0 || p0.Duration != 0 {
		t.Errorf("p0 = %+v", p0)
	}
	if p0.HeartRate == nil || *p0.HeartRate != 120 || *p0.Elevation != 110.5 || *p0.Cadence != 80 || *p0.Power != 200 {
		t.Errorf("p0 optionals wrong: %+v", p0)
	}
	if p1.Distance != 3.5 || p1.Duration != 1 || *p1.Speed != 3.5 {
		t.Errorf("p1 = %+v", p1)
	}
	if p1.HeartRate != nil || p1.Elevation != nil || p1.Cadence != nil || p1.Power != nil {
		t.Errorf("null cells must decode to nil: %+v", p1)
	}
}

func TestActivitySeries_RequestsFullResolution(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var gotQuery, gotPath atomic.Value
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotQuery.Store(r.URL.Query())
		gotPath.Store(r.URL.Path)
		_, _ = w.Write([]byte(seriesBody([][]any{{1, 0.0, 100, 0.0, 0.0, 1.0, 1, 1, 0.0, 0.0}}, 1, 1)))
	}))
	if _, err := c.ActivitySeries(context.Background(), 4242); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 when nothing is truncated", calls.Load())
	}
	q := gotQuery.Load().(url.Values)
	if q["maxChartSize"][0] != "20000" || q["maxPolylineSize"][0] != "2" {
		t.Errorf("query = %v", q)
	}
	if gotPath.Load() != "/activity-service/activity/4242/details" {
		t.Errorf("path = %v", gotPath.Load())
	}
}

func TestActivitySeries_RefetchesWhenTruncated(t *testing.T) {
	t.Parallel()
	var sizes []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := r.URL.Query().Get("maxChartSize")
		sizes = append(sizes, size)
		n := 2 // downsampled first answer
		if size == "30000" {
			n = 30000
		}
		rows := make([][]any, n)
		for i := range rows {
			rows[i] = []any{1, float64(i), 100, float64(i), 0.0, 1.0, 1, 1, 0.0, 0.0}
		}
		_, _ = w.Write([]byte(seriesBody(rows, n, 30000)))
	}))
	s, err := c.ActivitySeries(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sizes, ",") != "20000,30000" {
		t.Errorf("requested sizes = %v, want first 20000 then the reported total", sizes)
	}
	if len(s.Points) != 30000 || s.Downsampled {
		t.Errorf("points=%d downsampled=%v", len(s.Points), s.Downsampled)
	}
}

func TestActivitySeries_Errors(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.NotFoundHandler())
	if _, err := c.ActivitySeries(context.Background(), 1); !errors.Is(err, garminconnect.ErrNotFound) {
		t.Errorf("err = %v", err)
	}

	// A failing refetch is reported too.
	var n atomic.Int32
	c2 := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) > 1 {
			http.Error(w, "x", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(seriesBody([][]any{{1, 0.0, 1, 0.0, 0.0, 1.0, 1, 1, 0.0, 0.0}}, 1, 99999)))
	}))
	if _, err := c2.ActivitySeries(context.Background(), 1); !errors.Is(err, garminconnect.ErrForbidden) {
		t.Errorf("refetch err = %v", err)
	}
}

func TestActivitySeries_NoDetailsAvailable(t *testing.T) {
	t.Parallel()
	// Strength workouts and the like come back with no metrics at all.
	c, _ := serve(t, `{"activityId":1,"detailsAvailable":false,"activityDetailMetrics":null,"metricDescriptors":null,"metricsCount":0,"totalMetricsCount":0}`)
	s, err := c.ActivitySeries(context.Background(), 1)
	if err != nil || len(s.Points) != 0 {
		t.Fatalf("points=%d err=%v", len(s.Points), err)
	}
	if _, err := s.Splits(1000); !errors.Is(err, garminconnect.ErrNoDistance) {
		t.Errorf("Splits err = %v", err)
	}
}

// Uses the local, git-ignored capture of a real run when present (see
// TestLiveStoreActivity). It is skipped in CI and on fresh checkouts.
func TestSplits_LocalRealActivity(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "live")
	details, err := os.ReadFile(filepath.Join(dir, "activity_details.json"))
	if err != nil {
		t.Skip("no local capture; run TestLiveStoreActivity with -tags live")
	}
	summaryRaw, err := os.ReadFile(filepath.Join(dir, "activity_detail.json"))
	if err != nil {
		t.Skip("no local activity_detail capture")
	}
	var summary struct {
		S struct{ Distance, Duration, ElevationGain float64 } `json:"summaryDTO"`
	}
	if err := json.Unmarshal(summaryRaw, &summary); err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, string(details))
	series, err := c.ActivitySeries(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	splits, err := series.Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	var dist, dur, gain float64
	for _, s := range splits {
		dist += s.Distance
		dur += s.Duration
		gain += s.ElevationGain
	}
	if g := summary.S.ElevationGain; g > 0 && math.Abs(gain-g) > 0.15*g+3 {
		t.Errorf("split elevation gain total %.0f m vs Garmin's %.0f m", gain, g)
	}
	if !near(dist, summary.S.Distance, 1) || !near(dur, summary.S.Duration, 2) {
		t.Errorf("splits total %.1f m / %.1f s, summary says %.1f m / %.1f s", dist, dur, summary.S.Distance, summary.S.Duration)
	}
	if want := int(summary.S.Distance/1000) + 1; len(splits) != want {
		t.Errorf("got %d splits, want %d", len(splits), want)
	}
	t.Log("real-activity consistency: " + strconv.Itoa(len(splits)) + " splits; " + fmt.Sprintf("%.0f m, %.0f s", dist, dur))
}

// Garmin reports cadence twice: "directRunCadence" is per foot and
// "directDoubleCadence" is total steps per minute (what its own summary shows).
func TestActivitySeries_CadenceIsStepsPerMinuteNotPerFoot(t *testing.T) {
	t.Parallel()
	body := `{"metricsCount":1,"totalMetricsCount":1,
	  "metricDescriptors":[{"key":"sumDuration","metricsIndex":0},{"key":"directRunCadence","metricsIndex":1},{"key":"directDoubleCadence","metricsIndex":2}],
	  "activityDetailMetrics":[{"metrics":[0, 88, 176]}]}`
	c, _ := serve(t, body)
	s, err := c.ActivitySeries(context.Background(), 1)
	if err != nil || len(s.Points) != 1 || s.Points[0].Cadence == nil {
		t.Fatalf("series = %+v err %v", s, err)
	}
	if *s.Points[0].Cadence != 176 {
		t.Errorf("Cadence = %v, want 176 (steps per minute, not the per-foot 88)", *s.Points[0].Cadence)
	}
}

func TestSplits_ElevationIgnoresAltimeterJitter(t *testing.T) {
	t.Parallel()
	// A perfectly flat 2 km with the altimeter flickering +/-0.4 m.
	var s garminconnect.ActivitySeries
	for t := 0; t <= 500; t++ {
		jitter := 0.4
		if t%2 == 0 {
			jitter = -0.4
		}
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: float64(t), Distance: 4 * float64(t), Elevation: fp(100 + jitter)})
	}
	got, err := s.Splits(1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range got {
		if sp.ElevationGain != 0 || sp.ElevationLoss != 0 {
			t.Errorf("split %d invented elevation from noise: +%v -%v", sp.Index, sp.ElevationGain, sp.ElevationLoss)
		}
	}
}

func TestSplits_ElevationTracksRealClimbThroughJitter(t *testing.T) {
	t.Parallel()
	// Climb 10 m steadily over the first km, with +/-0.3 m jitter on top.
	var s garminconnect.ActivitySeries
	for t := 0; t <= 250; t++ {
		jitter := 0.3
		if t%2 == 0 {
			jitter = -0.3
		}
		s.Points = append(s.Points, garminconnect.SeriesPoint{Duration: float64(t), Distance: 4 * float64(t), Elevation: fp(100 + 10*float64(t)/250 + jitter)})
	}
	got, err := s.Splits(1000)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if g := got[0].ElevationGain; g < 9 || g > 11 {
		t.Errorf("gain = %v, want about 10 m", g)
	}
	if got[0].ElevationLoss > 1 {
		t.Errorf("loss = %v, want about 0", got[0].ElevationLoss)
	}
}

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

const (
	maxRangeDays        = 366
	defaultRangeDays    = 7
	defaultActivityPage = 20
	maxActivityPage     = 100
)

func ptr[T any](v T) *T { return &v }

func r1(x float64) float64 { return math.Round(x*10) / 10 }
func r2(x float64) float64 { return math.Round(x*100) / 100 }

func stamp(t garminconnect.Timestamp) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// addTool registers a read-only tool whose handler returns a typed result.
// Failures are reported as tool errors with guidance for the agent.
func addTool[In, Out any](s *Server, name, title, desc string, h func(context.Context, In) (Out, error)) {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: desc,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true), Title: title},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := h(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, s.userError(err)
		}
		return nil, out, nil
	})
}

// --- date handling -----------------------------------------------------------

func (s *Server) today() time.Time {
	n := s.now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func parseDay(field, v string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date like 2026-11-15, got %q", field, v)
	}
	return t, nil
}

// dateRange resolves optional start/end strings. End defaults to today and
// start to six days before end.
func (s *Server) dateRange(start, end string) (time.Time, time.Time, error) {
	e := s.today()
	if end != "" {
		var err error
		if e, err = parseDay("end", end); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	st := e.AddDate(0, 0, -(defaultRangeDays - 1))
	if start != "" {
		var err error
		if st, err = parseDay("start", start); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if st.After(e) {
		return time.Time{}, time.Time{}, fmt.Errorf("start (%s) is after end (%s)", st.Format(time.DateOnly), e.Format(time.DateOnly))
	}
	if days := int(e.Sub(st).Hours()/24) + 1; days > maxRangeDays {
		return time.Time{}, time.Time{}, fmt.Errorf("range is %d days; the limit is %d", days, maxRangeDays)
	}
	return st, e, nil
}

func (s *Server) oneDate(date string) (time.Time, error) {
	if date == "" {
		return s.today(), nil
	}
	return parseDay("date", date)
}

// rangeIn is the input shared by every date-range tool.
type rangeIn struct {
	Start          string `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD. Defaults to 6 days before end."`
	End            string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD. Defaults to today."`
	IncludeRawData *bool  `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

// --- activities ----------------------------------------------------------------

// ExerciseOut is one exercise of a strength session.
type ExerciseOut struct {
	Category    string  `json:"category"`
	SubCategory string  `json:"sub_category,omitempty"`
	Sets        int     `json:"sets"`
	Reps        int     `json:"reps"`
	MaxWeightKg float64 `json:"max_weight_kg"`
	VolumeKg    float64 `json:"volume_kg"`
	DurationS   float64 `json:"duration_s"`
}

// ActivityOut is an activity in compact, unit-labelled form.
type ActivityOut struct {
	ID                int64         `json:"id"`
	Name              string        `json:"name,omitempty"`
	Type              string        `json:"type"`
	StartTime         string        `json:"start_time,omitempty" jsonschema:"UTC, RFC 3339"`
	TimeZone          string        `json:"time_zone,omitempty"`
	DistanceM         float64       `json:"distance_m"`
	DurationS         float64       `json:"duration_s"`
	MovingDurationS   float64       `json:"moving_duration_s,omitempty"`
	Calories          float64       `json:"calories,omitempty"`
	AvgHR             float64       `json:"avg_hr,omitempty"`
	MaxHR             float64       `json:"max_hr,omitempty"`
	AvgSpeedMPS       float64       `json:"avg_speed_mps,omitempty"`
	ElevationGainM    float64       `json:"elevation_gain_m,omitempty"`
	ElevationLossM    float64       `json:"elevation_loss_m,omitempty"`
	AerobicEffect     float64       `json:"aerobic_training_effect,omitempty"`
	AnaerobicEffect   float64       `json:"anaerobic_training_effect,omitempty"`
	TrainingLoad      float64       `json:"training_load,omitempty"`
	TrainingLabel     string        `json:"training_effect_label,omitempty"`
	HRTimeInZoneS     []float64     `json:"hr_time_in_zone_s,omitempty" jsonschema:"seconds in heart-rate zones 1 to 5"`
	Steps             int           `json:"steps,omitempty"`
	BodyBatteryChange int           `json:"body_battery_change,omitempty"`
	TotalSets         int           `json:"total_sets,omitempty"`
	TotalReps         int           `json:"total_reps,omitempty"`
	Exercises         []ExerciseOut `json:"exercises,omitempty"`
	Sensors           []SensorOut   `json:"sensors,omitempty" jsonschema:"external sensors paired during the activity (get_activity only)"`
	Raw               any           `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified response for this activity; the fields above are a cleaned-up subset of it"`
}

// wantRaw reports whether a tool should include raw records: the
// include_raw_data input defaults to true when the client leaves it out.
func wantRaw(include *bool) bool { return include == nil || *include }

// rawData returns Garmin's unmodified JSON for an item when include is true,
// and nil otherwise or when there is none, so that the omitempty field is left
// out instead of becoming null.
func rawData(include bool, b json.RawMessage) any {
	if !include || len(b) == 0 {
		return nil
	}
	return b
}

// SensorOut is an external sensor recorded with an activity. The serial
// number is deliberately left out: it identifies the device and answers no
// training question.
type SensorOut struct {
	Type            string  `json:"type,omitempty" jsonschema:"what it measures, e.g. HEART_RATE, BIKE_POWER, FOOTPOD"`
	Manufacturer    string  `json:"manufacturer,omitempty"`
	Connection      string  `json:"connection,omitempty" jsonschema:"ANTPLUS, BLUETOOTH_LOW_ENERGY, ..."`
	SoftwareVersion float64 `json:"software_version,omitempty"`
	BatteryStatus   string  `json:"battery_status,omitempty"`
	BatteryPercent  *int    `json:"battery_percent,omitempty"`
}

func metricsOut(o *ActivityOut, m garminconnect.ActivityMetrics) {
	o.StartTime = stamp(m.StartTime)
	o.DistanceM = r1(m.Distance)
	o.DurationS = r1(m.Duration)
	o.MovingDurationS = r1(m.MovingDuration)
	o.Calories = r1(m.Calories)
	o.AvgHR = r1(m.AverageHR)
	o.MaxHR = r1(m.MaxHR)
	o.AvgSpeedMPS = r2(m.AverageSpeed)
	o.ElevationGainM = r1(m.ElevationGain)
	o.ElevationLossM = r1(m.ElevationLoss)
	o.AerobicEffect = r1(m.AerobicTrainingEffect)
	o.AnaerobicEffect = r1(m.AnaerobicTrainingEffect)
	o.TrainingLoad = r1(m.TrainingLoad)
	o.TrainingLabel = m.TrainingEffectLabel
	o.Steps = m.Steps
	o.BodyBatteryChange = m.BodyBatteryChange
	o.TotalSets = m.TotalSets
	o.TotalReps = m.TotalReps
}

func activityOut(a garminconnect.Activity, raw bool) ActivityOut {
	o := ActivityOut{ID: a.ID, Name: a.Name, Type: a.Type.Key, Raw: rawData(raw, a.Raw())}
	metricsOut(&o, a.ActivityMetrics)
	if a.HRTimeInZone != ([5]float64{}) {
		for _, z := range a.HRTimeInZone {
			o.HRTimeInZoneS = append(o.HRTimeInZoneS, r1(z))
		}
	}
	for _, e := range a.Exercises {
		o.Exercises = append(o.Exercises, ExerciseOut{
			Category: e.Category, SubCategory: e.SubCategory, Sets: e.Sets, Reps: e.Reps,
			MaxWeightKg: r2(e.MaxWeightKg), VolumeKg: r2(e.VolumeKg), DurationS: r1(e.DurationSeconds),
		})
	}
	return o
}

type listActivitiesIn struct {
	StartDate      string `json:"start_date,omitempty" jsonschema:"earliest local start date, YYYY-MM-DD"`
	EndDate        string `json:"end_date,omitempty" jsonschema:"latest local start date, YYYY-MM-DD"`
	Type           string `json:"type,omitempty" jsonschema:"parent activity type: running, cycling, walking, hiking, swimming, fitness_equipment, ..."`
	SubType        string `json:"sub_type,omitempty" jsonschema:"child activity type: strength_training, trail_running, treadmill_running, road_cycling, ..."`
	Search         string `json:"search,omitempty" jsonschema:"text to find in activity names and descriptions"`
	MinDistanceM   int64  `json:"min_distance_m,omitempty"`
	MaxDistanceM   int64  `json:"max_distance_m,omitempty"`
	MinDurationS   int64  `json:"min_duration_s,omitempty"`
	MaxDurationS   int64  `json:"max_duration_s,omitempty"`
	SortBy         string `json:"sort_by,omitempty" jsonschema:"name, distance or duration. Defaults to start time."`
	Ascending      bool   `json:"ascending,omitempty" jsonschema:"oldest or smallest first. Default is newest first."`
	Limit          int    `json:"limit,omitempty" jsonschema:"activities to return, 1 to 100. Default 20."`
	Offset         int    `json:"offset,omitempty" jsonschema:"activities to skip, for paging"`
	IncludeRawData *bool  `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

type listActivitiesOut struct {
	Count      int           `json:"count"`
	NextOffset *int          `json:"next_offset,omitempty" jsonschema:"pass as offset to fetch the next page; absent on the last page"`
	Activities []ActivityOut `json:"activities"`
}

type activityIDIn struct {
	ActivityID     int64 `json:"activity_id" jsonschema:"activity id from list_activities"`
	IncludeRawData *bool `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

type splitsIn struct {
	ActivityID int64   `json:"activity_id" jsonschema:"activity id from list_activities"`
	SplitM     float64 `json:"split_distance_m,omitempty" jsonschema:"split length in metres: 1000 for km splits, 1609.344 for miles. Default 1000."`
}

// SplitOut is one fixed-distance segment of an activity.
type SplitOut struct {
	Index          int     `json:"index"`
	DistanceM      float64 `json:"distance_m"`
	DurationS      float64 `json:"duration_s"`
	PaceSPerKm     float64 `json:"pace_s_per_km"`
	AvgSpeedMPS    float64 `json:"avg_speed_mps"`
	AvgHR          float64 `json:"avg_hr,omitempty"`
	MaxHR          float64 `json:"max_hr,omitempty"`
	AvgCadenceSPM  float64 `json:"avg_cadence_spm,omitempty"`
	ElevationGainM float64 `json:"elevation_gain_m"`
	ElevationLossM float64 `json:"elevation_loss_m"`
	Partial        bool    `json:"partial,omitempty" jsonschema:"the shorter final segment"`
}

type splitsOut struct {
	ActivityID int64      `json:"activity_id"`
	SplitM     float64    `json:"split_distance_m"`
	Splits     []SplitOut `json:"splits"`
}

type seriesIn struct {
	ActivityID int64 `json:"activity_id" jsonschema:"activity id from list_activities"`
	MaxPoints  int   `json:"max_points,omitempty" jsonschema:"most samples to return, 10 to 1000, evenly spaced. Default 200."`
}

// SeriesPointOut is one sample of an activity.
type SeriesPointOut struct {
	TimeS      float64  `json:"time_s" jsonschema:"timer seconds since the start"`
	DistanceM  float64  `json:"distance_m"`
	HR         *float64 `json:"hr,omitempty"`
	SpeedMPS   *float64 `json:"speed_mps,omitempty"`
	ElevationM *float64 `json:"elevation_m,omitempty"`
	CadenceSPM *float64 `json:"cadence_spm,omitempty"`
	PowerW     *float64 `json:"power_w,omitempty"`
}

type seriesOut struct {
	ActivityID  int64            `json:"activity_id"`
	TotalPoints int              `json:"total_points" jsonschema:"samples Garmin holds, before thinning"`
	Points      []SeriesPointOut `json:"points"`
}

func opt(p *float64) *float64 {
	if p == nil {
		return nil
	}
	return ptr(r2(*p))
}

func (s *Server) registerActivityTools() {
	addTool(s, "list_activities", "List activities",
		"Find the user's activities (runs, rides, strength sessions, ...), newest first, with distance, duration, heart rate, training effect, elevation and, for strength sessions, the per-exercise breakdown. Filter by date, type, text and size.",
		s.listActivities)

	addTool(s, "get_activity", "Get one activity",
		"Summary of a single activity by id: type, time zone, distance, duration, heart rate, elevation, training effect, and the external sensors used (heart-rate strap, power meter, foot pod; with battery state).",
		s.getActivity)

	addTool(s, "get_activity_splits", "Get activity splits",
		"Per-kilometre (or per-mile) splits of a run or ride, computed from the per-second data: time, pace, heart rate, cadence and elevation for each segment. Not available for activities without distance, such as strength training.",
		s.getActivitySplits)

	addTool(s, "get_activity_series", "Get activity time series",
		"Heart rate, speed, elevation, cadence and power over time for one activity (GPS coordinates are never included). Samples are thinned to max_points evenly spaced points.",
		s.getActivitySeries)
}

// listActivities handles the list_activities tool.
func (s *Server) listActivities(ctx context.Context, in listActivitiesIn) (listActivitiesOut, error) {
	raw := wantRaw(in.IncludeRawData)
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultActivityPage
	case limit < 0 || limit > maxActivityPage:
		return listActivitiesOut{}, fmt.Errorf("limit must be between 1 and %d", maxActivityPage)
	}
	opts := garminconnect.ActivitiesOptions{
		Start: in.Offset, Limit: limit, TypeKey: in.Type, SubTypeKey: in.SubType, Search: in.Search,
		MinDistance: in.MinDistanceM, MaxDistance: in.MaxDistanceM, MinDuration: in.MinDurationS, MaxDuration: in.MaxDurationS,
		SortBy: in.SortBy, Ascending: in.Ascending,
	}
	var err error
	if in.StartDate != "" {
		if opts.StartDate, err = parseDay("start_date", in.StartDate); err != nil {
			return listActivitiesOut{}, err
		}
	}
	if in.EndDate != "" {
		if opts.EndDate, err = parseDay("end_date", in.EndDate); err != nil {
			return listActivitiesOut{}, err
		}
	}
	acts, err := s.api.Activities(ctx, opts)
	if err != nil {
		return listActivitiesOut{}, err
	}
	out := listActivitiesOut{Count: len(acts), Activities: make([]ActivityOut, 0, len(acts))}
	for _, a := range acts {
		out.Activities = append(out.Activities, activityOut(a, raw))
	}
	if len(acts) == limit {
		out.NextOffset = ptr(in.Offset + len(acts))
	}
	return out, nil
}

// getActivity handles the get_activity tool.
func (s *Server) getActivity(ctx context.Context, in activityIDIn) (ActivityOut, error) {
	raw := wantRaw(in.IncludeRawData)
	d, err := s.api.ActivityDetail(ctx, in.ActivityID)
	if err != nil {
		return ActivityOut{}, err
	}
	o := ActivityOut{ID: d.ID, Name: d.Name, Type: d.Type.Key, TimeZone: d.TimeZone, Raw: rawData(raw, d.Raw())}
	metricsOut(&o, d.Summary)
	for _, sn := range d.Sensors {
		o.Sensors = append(o.Sensors, SensorOut{
			Type: sn.DeviceType, Manufacturer: sn.Manufacturer, Connection: sn.SourceType,
			SoftwareVersion: sn.SoftwareVersion, BatteryStatus: sn.BatteryStatus, BatteryPercent: sn.BatteryLevel,
		})
	}
	return o, nil
}

// getActivitySplits handles the get_activity_splits tool.
func (s *Server) getActivitySplits(ctx context.Context, in splitsIn) (splitsOut, error) {
	size := in.SplitM
	if size == 0 {
		size = 1000
	}
	if size < 10 {
		return splitsOut{}, fmt.Errorf("split_distance_m must be at least 10")
	}
	series, err := s.api.ActivitySeries(ctx, in.ActivityID)
	if err != nil {
		return splitsOut{}, err
	}
	splits, err := series.Splits(size)
	if err != nil {
		return splitsOut{}, err
	}
	out := splitsOut{ActivityID: in.ActivityID, SplitM: size, Splits: make([]SplitOut, 0, len(splits))}
	for _, sp := range splits {
		out.Splits = append(out.Splits, SplitOut{
			Index: sp.Index, DistanceM: r1(sp.Distance), DurationS: r1(sp.Duration), PaceSPerKm: r1(sp.PaceSecondsPerKm()),
			AvgSpeedMPS: r2(sp.AvgSpeed), AvgHR: r1(sp.AvgHeartRate), MaxHR: r1(sp.MaxHeartRate), AvgCadenceSPM: r1(sp.AvgCadence),
			ElevationGainM: r1(sp.ElevationGain), ElevationLossM: r1(sp.ElevationLoss), Partial: sp.Partial,
		})
	}
	return out, nil
}

// getActivitySeries handles the get_activity_series tool.
func (s *Server) getActivitySeries(ctx context.Context, in seriesIn) (seriesOut, error) {
	maxPts := in.MaxPoints
	switch {
	case maxPts == 0:
		maxPts = 200
	case maxPts < 10 || maxPts > 1000:
		return seriesOut{}, fmt.Errorf("max_points must be between 10 and 1000")
	}
	series, err := s.api.ActivitySeries(ctx, in.ActivityID)
	if err != nil {
		return seriesOut{}, err
	}
	out := seriesOut{ActivityID: in.ActivityID, TotalPoints: len(series.Points)}
	n := len(series.Points)
	for i := 0; i < maxPts && i < n; i++ {
		idx := i
		if n > maxPts { // evenly spaced, always including the first and last sample
			idx = int(math.Round(float64(i) * float64(n-1) / float64(maxPts-1)))
		}
		p := series.Points[idx]
		out.Points = append(out.Points, SeriesPointOut{
			TimeS: r1(p.Duration), DistanceM: r1(p.Distance),
			HR: opt(p.HeartRate), SpeedMPS: opt(p.Speed), ElevationM: opt(p.Elevation), CadenceSPM: opt(p.Cadence), PowerW: opt(p.Power),
		})
	}
	return out, nil
}

// registerTools adds every tool to the MCP server.
func (s *Server) registerTools() {
	s.registerActivityTools()
	s.registerHealthTools()
}

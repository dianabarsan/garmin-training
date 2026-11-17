package mcpserver

import (
	"context"
	"fmt"
	"time"

	garmin "github.com/dianabarsan/garmin-training"
)

// --- daily summaries -----------------------------------------------------------

// DaySummaryOut is one day's wellness rollup.
type DaySummaryOut struct {
	Date             string  `json:"date"`
	Steps            int     `json:"steps"`
	StepGoal         int     `json:"step_goal,omitempty"`
	DistanceM        float64 `json:"distance_m,omitempty"`
	FloorsAscended   int     `json:"floors_ascended,omitempty"`
	CaloriesTotal    int     `json:"calories_total,omitempty"`
	CaloriesActive   int     `json:"calories_active,omitempty"`
	RestingHR        int     `json:"resting_hr,omitempty"`
	MinHR            int     `json:"min_hr,omitempty"`
	MaxHR            int     `json:"max_hr,omitempty"`
	AvgStress        int     `json:"avg_stress,omitempty"`
	BodyBatteryHigh  int     `json:"body_battery_high,omitempty"`
	BodyBatteryLow   int     `json:"body_battery_low,omitempty"`
	BodyBatteryGain  int     `json:"body_battery_charged,omitempty"`
	BodyBatteryDrain int     `json:"body_battery_drained,omitempty"`
	ModerateMinutes  int     `json:"intensity_minutes_moderate,omitempty"`
	VigorousMinutes  int     `json:"intensity_minutes_vigorous,omitempty"`
	AvgRespiration   float64 `json:"avg_respiration,omitempty"`
	AvgSpO2          int     `json:"avg_spo2,omitempty"`
	HydrationMl      int     `json:"hydration_ml,omitempty"`
	Raw              any     `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified record for this day"`
}

type daysOut[T any] struct {
	Count int `json:"count"`
	Days  []T `json:"days"`
}

func daySummaryOut(d garmin.DailySummary, raw bool) DaySummaryOut {
	o := DaySummaryOut{Date: d.CalendarDate, Raw: rawData(raw, d.Raw())}
	if d.Steps != nil {
		o.Steps, o.StepGoal, o.DistanceM = d.Steps.Value, d.Steps.Goal, r1(d.Steps.DistanceMeters)
	}
	if d.FloorsAscended != nil {
		o.FloorsAscended = d.FloorsAscended.Value
	}
	if d.Calories != nil {
		o.CaloriesTotal, o.CaloriesActive = d.Calories.Total, d.Calories.Active
	}
	if d.HeartRate != nil {
		o.RestingHR, o.MinHR, o.MaxHR = d.HeartRate.Resting, d.HeartRate.Min, d.HeartRate.Max
	}
	if d.Stress != nil {
		o.AvgStress = d.Stress.AvgLevel
	}
	if b := d.BodyBattery; b != nil {
		o.BodyBatteryHigh, o.BodyBatteryLow, o.BodyBatteryGain, o.BodyBatteryDrain = b.Max, b.Min, b.Charged, b.Drained
	}
	if m := d.IntensityMinutes; m != nil {
		o.ModerateMinutes, o.VigorousMinutes = m.Moderate, m.Vigorous
	}
	if d.Respiration != nil {
		o.AvgRespiration = r1(d.Respiration.Avg)
	}
	if d.PulseOx != nil {
		o.AvgSpO2 = d.PulseOx.Avg
	}
	if d.Hydration != nil {
		o.HydrationMl = d.Hydration.ConsumedMl
	}
	return o
}

// --- sleep ---------------------------------------------------------------------

// NapOut is one nap.
type NapOut struct {
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	DurationS int    `json:"duration_s"`
	Feedback  string `json:"feedback,omitempty"`
}

// SleepOut is one day's sleep record.
type SleepOut struct {
	Date            string   `json:"date"`
	HasSleep        bool     `json:"has_sleep" jsonschema:"false for days that only have naps"`
	Start           string   `json:"start,omitempty" jsonschema:"UTC, RFC 3339"`
	End             string   `json:"end,omitempty" jsonschema:"UTC, RFC 3339"`
	TotalS          int      `json:"total_s"`
	DeepS           int      `json:"deep_s,omitempty"`
	LightS          int      `json:"light_s,omitempty"`
	REMS            int      `json:"rem_s,omitempty"`
	AwakeS          int      `json:"awake_s,omitempty"`
	NapS            int      `json:"nap_s,omitempty"`
	AwakeCount      int      `json:"awake_count,omitempty"`
	Score           int      `json:"score,omitempty" jsonschema:"overall sleep score, 0 to 100"`
	ScoreQuality    string   `json:"score_quality,omitempty"`
	Feedback        string   `json:"feedback,omitempty"`
	AvgHR           float64  `json:"avg_hr,omitempty"`
	AvgRespiration  float64  `json:"avg_respiration,omitempty"`
	AvgStress       float64  `json:"avg_stress,omitempty"`
	AvgSpO2         float64  `json:"avg_spo2,omitempty"`
	LowestSpO2      int      `json:"lowest_spo2,omitempty"`
	NeedBaselineMin int      `json:"sleep_need_baseline_min,omitempty"`
	NeedActualMin   int      `json:"sleep_need_actual_min,omitempty"`
	NeedFeedback    string   `json:"sleep_need_feedback,omitempty"`
	Naps            []NapOut `json:"naps,omitempty"`
	Raw             any      `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified sleep record for this day"`
}

func sleepOut(sl garmin.Sleep, raw bool) SleepOut {
	o := SleepOut{
		Date: sl.CalendarDate, HasSleep: sl.HasSleep(), Start: stamp(sl.Start), End: stamp(sl.End),
		TotalS: sl.TotalSeconds, DeepS: sl.DeepSeconds, LightS: sl.LightSeconds, REMS: sl.REMSeconds, AwakeS: sl.AwakeSeconds,
		NapS: sl.NapSeconds, AwakeCount: sl.AwakeCount, Score: sl.Scores.Overall.Value, ScoreQuality: sl.Scores.Overall.Qualifier,
		Feedback: sl.ScoreFeedback, AvgHR: r1(sl.AvgHeartRate), AvgRespiration: r1(sl.AvgRespiration), AvgStress: r1(sl.AvgSleepStress),
		AvgSpO2: r1(sl.AvgSpO2), LowestSpO2: sl.LowestSpO2, Raw: rawData(raw, sl.Raw()),
	}
	if n := sl.SleepNeed; n != nil {
		o.NeedBaselineMin, o.NeedActualMin, o.NeedFeedback = n.BaselineMins, n.ActualMins, n.Feedback
	}
	for _, n := range sl.Naps {
		o.Naps = append(o.Naps, NapOut{Start: stamp(n.Start), End: stamp(n.End), DurationS: n.Seconds, Feedback: n.Feedback})
	}
	return o
}

// --- heart rate / HRV / body battery / stress / steps / VO2 / zones ---------------

type heartRateIn struct {
	Date           string `json:"date,omitempty" jsonschema:"day, YYYY-MM-DD. Defaults to today."`
	IncludeSample  bool   `json:"include_samples,omitempty" jsonschema:"also return the heart-rate curve, averaged into buckets"`
	BucketMinutes  int    `json:"bucket_minutes,omitempty" jsonschema:"bucket size for samples, 1 to 240. Default 30."`
	IncludeRawData *bool  `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

// HRSampleOut is the average heart rate over one bucket of time.
type HRSampleOut struct {
	Time string `json:"time" jsonschema:"start of the bucket, UTC, RFC 3339"`
	BPM  int    `json:"bpm"`
}

type heartRateOut struct {
	Date           string        `json:"date"`
	RestingHR      int           `json:"resting_hr"`
	MinHR          int           `json:"min_hr"`
	MaxHR          int           `json:"max_hr"`
	AvgRestingHR7d int           `json:"avg_resting_hr_7d"`
	SampleCount    int           `json:"sample_count" jsonschema:"readings Garmin holds for the day"`
	Samples        []HRSampleOut `json:"samples,omitempty"`
	Raw            any           `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified response for the day, including every heart-rate reading"`
}

// HRVNightOut is one night's HRV summary.
type HRVNightOut struct {
	Date         string `json:"date"`
	LastNightAvg int    `json:"last_night_avg_ms"`
	WeeklyAvg    int    `json:"weekly_avg_ms"`
	Peak5Min     int    `json:"last_night_5min_high_ms"`
	Status       string `json:"status" jsonschema:"for example BALANCED or UNBALANCED"`
	Feedback     string `json:"feedback,omitempty"`
	BaselineLow  int    `json:"baseline_balanced_low_ms,omitempty"`
	BaselineHigh int    `json:"baseline_balanced_high_ms,omitempty"`
	Raw          any    `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified HRV summary for this night"`
}

// BodyBatteryOut is one day of Body Battery.
type BodyBatteryOut struct {
	Date        string `json:"date"`
	Charged     int    `json:"charged"`
	Drained     int    `json:"drained"`
	High        int    `json:"high"`
	Low         int    `json:"low"`
	First       int    `json:"first"`
	Last        int    `json:"last"`
	SampleCount int    `json:"sample_count"`
	Raw         any    `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified Body Battery record for this day, including every reading"`
}

// StressDayOut is one day of stress.
type StressDayOut struct {
	Date    string `json:"date"`
	Level   int    `json:"overall_level" jsonschema:"0 to 100"`
	RestS   int    `json:"rest_s"`
	LowS    int    `json:"low_s"`
	MediumS int    `json:"medium_s"`
	HighS   int    `json:"high_s"`
	Raw     any    `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified stress record for this day"`
}

// StepsDayOut is one day of steps.
type StepsDayOut struct {
	Date      string  `json:"date"`
	Steps     int     `json:"steps"`
	Goal      int     `json:"goal"`
	DistanceM float64 `json:"distance_m"`
	Raw       any     `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified steps record for this day"`
}

type vo2In struct {
	Start          string `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD. Defaults to 6 days before end."`
	End            string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD. Defaults to today."`
	Sport          string `json:"sport,omitempty" jsonschema:"running or cycling; omit for both"`
	IncludeRawData *bool  `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

// VO2Out is a VO2 max reading (ml/kg/min).
type VO2Out struct {
	Date    string  `json:"date"`
	Sport   string  `json:"sport"`
	Value   float64 `json:"vo2max" jsonschema:"to one decimal place"`
	Rounded float64 `json:"vo2max_rounded"`
}

// VO2DayOut is a dated VO2 max entry. Garmin adds one when it recomputes, so
// days can be missing.
type VO2DayOut struct {
	General *VO2Out `json:"general,omitempty"`
	Cycling *VO2Out `json:"cycling,omitempty"`
	Raw     any     `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified VO2 max entry"`
}

type zonesIn struct {
	Sport          string `json:"sport,omitempty" jsonschema:"running, cycling, ... Falls back to the default zones. Default running."`
	IncludeRawData *bool  `json:"include_raw_data,omitempty" jsonschema:"also return Garmin's complete, unmodified record for each item under raw. Default true. Set false for long ranges: the raw records make the output several times larger."`
}

// ZoneOut is a heart-rate zone as an inclusive bpm range.
type ZoneOut struct {
	Zone   int `json:"zone"`
	MinBPM int `json:"min_bpm"`
	MaxBPM int `json:"max_bpm"`
}

type zonesOut struct {
	Sport          string    `json:"sport"`
	TrainingMethod string    `json:"training_method"`
	Configured     bool      `json:"configured"`
	RestingHR      int       `json:"resting_hr,omitempty"`
	ThresholdHR    int       `json:"lactate_threshold_hr,omitempty"`
	MaxHR          int       `json:"max_hr,omitempty"`
	Zones          []ZoneOut `json:"zones"`
	Raw            any       `json:"raw,omitempty" jsonschema:"Garmin's complete, unmodified zone configuration"`
}

func vo2Out(r *garmin.VO2MaxReading) *VO2Out {
	if r == nil {
		return nil
	}
	return &VO2Out{Date: r.CalendarDate, Sport: r.Sport, Value: r1(r.Precise), Rounded: r.Value}
}

// thinHeartRate averages samples into buckets of the given length.
func thinHeartRate(samples []garmin.HeartRateSample, bucket time.Duration) []HRSampleOut {
	var out []HRSampleOut
	var sum, n int
	var cur time.Time
	flush := func() {
		if n > 0 {
			out = append(out, HRSampleOut{Time: cur.UTC().Format(time.RFC3339), BPM: int(float64(sum)/float64(n) + 0.5)})
		}
		sum, n = 0, 0
	}
	for _, p := range samples {
		start := p.Time.Truncate(bucket)
		if n > 0 && !start.Equal(cur) {
			flush()
		}
		cur = start
		sum += p.BPM
		n++
	}
	flush()
	return out
}

func (s *Server) registerHealthTools() {
	addTool(s, "get_daily_summaries", "Daily wellness summaries",
		"One row per day: steps, distance, calories, resting/min/max heart rate, average stress, Body Battery, intensity minutes, respiration, SpO2 and hydration.",
		s.getDailySummaries)

	addTool(s, "get_sleep", "Sleep",
		"Sleep per day: duration and stages, sleep score, heart rate, respiration, SpO2, Garmin's sleep-need estimate and any naps. Days with only naps have has_sleep false.",
		s.getSleep)

	addTool(s, "get_heart_rate", "Heart rate for a day",
		"Resting, minimum and maximum heart rate for one day, with the 7-day average resting rate and optionally the heart-rate curve averaged into buckets.",
		s.getHeartRate)

	addTool(s, "get_hrv", "Heart rate variability",
		"Nightly HRV per day: last-night average, weekly average, 5-minute peak, status (BALANCED, UNBALANCED, ...) and the personal baseline range, all in milliseconds.",
		s.getHrv)

	addTool(s, "get_body_battery", "Body Battery",
		"Body Battery (0 to 100 energy score) per day: amount charged and drained, the day's high and low, and first and last reading.",
		s.getBodyBattery)

	addTool(s, "get_stress", "Stress",
		"Daily stress: overall level (0 to 100) and seconds spent resting and in low, medium and high stress.",
		s.getStress)

	addTool(s, "get_steps", "Steps",
		"Daily step count, step goal and distance walked.",
		s.getSteps)

	addTool(s, "get_vo2max", "VO2 max",
		"VO2 max history (ml/kg/min). Garmin records an entry when it recomputes the estimate, so days can be missing; widen the range if the result is empty.",
		s.getVo2max)

	addTool(s, "get_heart_rate_zones", "Heart-rate zones",
		"The user's heart-rate zones for a sport as inclusive bpm ranges, with resting, threshold and maximum heart rate.",
		s.getHeartRateZones)
}

// getDailySummaries handles the get_daily_summaries tool.
func (s *Server) getDailySummaries(ctx context.Context, in rangeIn) (daysOut[DaySummaryOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[DaySummaryOut]{}, err
	}
	ds, err := s.api.DailySummaries(ctx, st, en)
	if err != nil {
		return daysOut[DaySummaryOut]{}, err
	}
	out := daysOut[DaySummaryOut]{Count: len(ds), Days: make([]DaySummaryOut, 0, len(ds))}
	for _, d := range ds {
		out.Days = append(out.Days, daySummaryOut(d, raw))
	}
	return out, nil
}

// getSleep handles the get_sleep tool.
func (s *Server) getSleep(ctx context.Context, in rangeIn) (daysOut[SleepOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[SleepOut]{}, err
	}
	sl, err := s.api.SleepRange(ctx, st, en)
	if err != nil {
		return daysOut[SleepOut]{}, err
	}
	out := daysOut[SleepOut]{Count: len(sl), Days: make([]SleepOut, 0, len(sl))}
	for _, x := range sl {
		out.Days = append(out.Days, sleepOut(x, raw))
	}
	return out, nil
}

// getHeartRate handles the get_heart_rate tool.
func (s *Server) getHeartRate(ctx context.Context, in heartRateIn) (heartRateOut, error) {
	raw := wantRaw(in.IncludeRawData)
	day, err := s.oneDate(in.Date)
	if err != nil {
		return heartRateOut{}, err
	}
	bucket := in.BucketMinutes
	switch {
	case bucket == 0:
		bucket = 30
	case bucket < 1 || bucket > 240:
		return heartRateOut{}, fmt.Errorf("bucket_minutes must be between 1 and 240")
	}
	h, err := s.api.HeartRate(ctx, day)
	if err != nil {
		return heartRateOut{}, err
	}
	out := heartRateOut{
		Date: h.CalendarDate, RestingHR: h.Resting, MinHR: h.Min, MaxHR: h.Max,
		AvgRestingHR7d: h.LastSevenDaysAvgResting, SampleCount: len(h.Samples), Raw: rawData(raw, h.Raw()),
	}
	if out.Date == "" {
		out.Date = day.Format(time.DateOnly)
	}
	if in.IncludeSample {
		out.Samples = thinHeartRate(h.Samples, time.Duration(bucket)*time.Minute)
	}
	return out, nil
}

// getHrv handles the get_hrv tool.
func (s *Server) getHrv(ctx context.Context, in rangeIn) (daysOut[HRVNightOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[HRVNightOut]{}, err
	}
	ns, err := s.api.HRVRange(ctx, st, en)
	if err != nil {
		return daysOut[HRVNightOut]{}, err
	}
	out := daysOut[HRVNightOut]{Count: len(ns), Days: make([]HRVNightOut, 0, len(ns))}
	for _, n := range ns {
		o := HRVNightOut{Date: n.CalendarDate, LastNightAvg: n.LastNightAvg, WeeklyAvg: n.WeeklyAvg, Peak5Min: n.LastNight5MinHigh, Status: n.Status, Feedback: n.FeedbackPhrase, Raw: rawData(raw, n.Raw())}
		if n.Baseline != nil {
			o.BaselineLow, o.BaselineHigh = n.Baseline.BalancedLow, n.Baseline.BalancedUpper
		}
		out.Days = append(out.Days, o)
	}
	return out, nil
}

// getBodyBattery handles the get_body_battery tool.
func (s *Server) getBodyBattery(ctx context.Context, in rangeIn) (daysOut[BodyBatteryOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[BodyBatteryOut]{}, err
	}
	bb, err := s.api.BodyBattery(ctx, st, en)
	if err != nil {
		return daysOut[BodyBatteryOut]{}, err
	}
	out := daysOut[BodyBatteryOut]{Count: len(bb), Days: make([]BodyBatteryOut, 0, len(bb))}
	for _, d := range bb {
		o := BodyBatteryOut{Date: d.Date, Charged: d.Charged, Drained: d.Drained, SampleCount: len(d.Samples), Raw: rawData(raw, d.Raw())}
		if len(d.Samples) > 0 {
			o.High, o.Low = d.Samples[0].Level, d.Samples[0].Level
			o.First, o.Last = d.Samples[0].Level, d.Samples[len(d.Samples)-1].Level
			for _, p := range d.Samples {
				o.High, o.Low = max(o.High, p.Level), min(o.Low, p.Level)
			}
		}
		out.Days = append(out.Days, o)
	}
	return out, nil
}

// getStress handles the get_stress tool.
func (s *Server) getStress(ctx context.Context, in rangeIn) (daysOut[StressDayOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[StressDayOut]{}, err
	}
	ds, err := s.api.Stress(ctx, st, en)
	if err != nil {
		return daysOut[StressDayOut]{}, err
	}
	out := daysOut[StressDayOut]{Count: len(ds), Days: make([]StressDayOut, 0, len(ds))}
	for _, d := range ds {
		v := d.Values
		out.Days = append(out.Days, StressDayOut{Date: d.CalendarDate, Level: v.OverallLevel, RestS: v.RestSeconds, LowS: v.LowSeconds, MediumS: v.MediumSeconds, HighS: v.HighSeconds, Raw: rawData(raw, d.Raw())})
	}
	return out, nil
}

// getSteps handles the get_steps tool.
func (s *Server) getSteps(ctx context.Context, in rangeIn) (daysOut[StepsDayOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[StepsDayOut]{}, err
	}
	ds, err := s.api.Steps(ctx, st, en)
	if err != nil {
		return daysOut[StepsDayOut]{}, err
	}
	out := daysOut[StepsDayOut]{Count: len(ds), Days: make([]StepsDayOut, 0, len(ds))}
	for _, d := range ds {
		out.Days = append(out.Days, StepsDayOut{Date: d.CalendarDate, Steps: d.TotalSteps, Goal: d.StepGoal, DistanceM: r1(d.TotalDistance), Raw: rawData(raw, d.Raw())})
	}
	return out, nil
}

// getVo2max handles the get_vo2max tool.
func (s *Server) getVo2max(ctx context.Context, in vo2In) (daysOut[VO2DayOut], error) {
	raw := wantRaw(in.IncludeRawData)
	st, en, err := s.dateRange(in.Start, in.End)
	if err != nil {
		return daysOut[VO2DayOut]{}, err
	}
	var sports []string
	if in.Sport != "" {
		sports = []string{in.Sport}
	}
	vs, err := s.api.VO2Max(ctx, st, en, sports...)
	if err != nil {
		return daysOut[VO2DayOut]{}, err
	}
	out := daysOut[VO2DayOut]{Count: len(vs), Days: make([]VO2DayOut, 0, len(vs))}
	for _, v := range vs {
		out.Days = append(out.Days, VO2DayOut{General: vo2Out(v.Generic), Cycling: vo2Out(v.Cycling), Raw: rawData(raw, v.Raw())})
	}
	return out, nil
}

// getHeartRateZones handles the get_heart_rate_zones tool.
func (s *Server) getHeartRateZones(ctx context.Context, in zonesIn) (zonesOut, error) {
	raw := wantRaw(in.IncludeRawData)
	sport := in.Sport
	if sport == "" {
		sport = "running"
	}
	z, err := s.api.HeartRateZonesFor(ctx, sport)
	if err != nil {
		return zonesOut{}, err
	}
	out := zonesOut{Sport: z.Sport, TrainingMethod: z.TrainingMethod, Configured: z.Configured(), RestingHR: z.RestingHR, ThresholdHR: z.LactateThresholdHR, MaxHR: z.MaxHR, Zones: make([]ZoneOut, 0, len(z.Zones)), Raw: rawData(raw, z.Raw())}
	for _, hz := range z.Zones {
		out.Zones = append(out.Zones, ZoneOut{Zone: hz.Number, MinBPM: hz.MinBPM, MaxBPM: hz.MaxBPM})
	}
	return out, nil
}

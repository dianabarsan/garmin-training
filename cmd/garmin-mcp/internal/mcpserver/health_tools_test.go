package mcpserver

import (
	"strings"
	"testing"
)

type dayList[T any] struct {
	Count int `json:"count"`
	Days  []T `json:"days"`
}

func TestTools_DefaultRangeIsTheLastSevenDaysEndingToday(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[]`)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-01/2026-11-05", `[]`)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-14/2026-11-16", `[]`)

	for name, args := range map[string]map[string]any{
		"no arguments":   nil,
		"explicit range": {"start": "2026-11-01", "end": "2026-11-05"},
		"only a start":   {"start": "2026-11-14"},
		"only an end":    {"end": "2026-11-16"},
	} {
		if res := callTool(t, cs, "get_steps", args); res.IsError {
			t.Errorf("%s: %s", name, resultText(res))
		}
	}
	// "only an end" must start 6 days earlier; the route below proves it.
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-04/2026-11-10", `[{"calendarDate":"2026-11-10","totalSteps":7}]`)
	got := decodeResult[dayList[struct {
		Steps int `json:"steps"`
	}]](t, callTool(t, cs, "get_steps", map[string]any{"end": "2026-11-10"}))
	if got.Count != 1 || got.Days[0].Steps != 7 {
		t.Errorf("end-only default start: %+v", got)
	}
}

func TestTools_DateValidation(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"bad start":      {"get_sleep", map[string]any{"start": "yesterday"}, "start must be a date"},
		"bad end":        {"get_steps", map[string]any{"end": "2026-02-30"}, "end must be a date"},
		"start>end":      {"get_stress", map[string]any{"start": "2026-11-10", "end": "2026-11-01"}, "after end"},
		"367 days":       {"get_hrv", map[string]any{"start": "2025-11-14", "end": "2026-11-15"}, "limit is 366"},
		"bad single day": {"get_heart_rate", map[string]any{"date": "11/15/2026"}, "date must be a date"},
		"bad bucket":     {"get_heart_rate", map[string]any{"bucket_minutes": 500}, "bucket_minutes"},
	} {
		res := callTool(t, cs, tc.tool, tc.args)
		if !res.IsError || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s: IsError=%v text=%q, want it to contain %q", name, res.IsError, resultText(res), tc.want)
		}
	}
	// Exactly 366 days is allowed.
	h.fg.route("/hrv-service/hrv/daily/2025-11-16/2026-11-16", `{"hrvSummaries":[]}`)
	if res := callTool(t, cs, "get_hrv", map[string]any{"start": "2025-11-16", "end": "2026-11-16"}); res.IsError {
		t.Errorf("366 days must be accepted: %s", resultText(res))
	}
}

func TestTools_DailySummaries(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/usersummary-service/v2/dailysummaries", `{"data":[
	 {"calendarDate":"2026-11-15","movement":{"steps":{"value":8123,"goal":10000,"distanceInMeters":6100.55},"floorsAscended":{"value":12}},
	  "calories":{"burnedTotal":2400,"burnedActive":600},"heartRate":{"minValue":44,"maxValue":160,"restingValue":50},
	  "stress":{"avgLevel":31},"bodyBattery":{"chargedValue":60,"drainedValue":55,"minValue":10,"maxValue":95},
	  "intensityMinutes":{"moderate":30,"vigorous":15},"respiration":{"avgValue":14.46},"pulseOx":{"avgValue":96},"hydration":{"consumedInMl":1500}},
	 {"calendarDate":"2026-11-16"}]}`)
	type day struct {
		Steps   int     `json:"steps"`
		Goal    int     `json:"step_goal"`
		Dist    float64 `json:"distance_m"`
		Floors  int     `json:"floors_ascended"`
		Cal     int     `json:"calories_total"`
		Rest    int     `json:"resting_hr"`
		Stress  int     `json:"avg_stress"`
		BBHigh  int     `json:"body_battery_high"`
		BBDrain int     `json:"body_battery_drained"`
		Vig     int     `json:"intensity_minutes_vigorous"`
		Resp    float64 `json:"avg_respiration"`
		SpO2    int     `json:"avg_spo2"`
		Water   int     `json:"hydration_ml"`
	}
	type dayWithDate struct {
		D string `json:"date"`
		day
	}
	got := decodeResult[dayList[dayWithDate]](t, callTool(t, cs, "get_daily_summaries", nil))
	if got.Count != 2 {
		t.Fatalf("count = %d", got.Count)
	}
	d := got.Days[0]
	if d.D != "2026-11-15" || d.Steps != 8123 || d.Goal != 10000 || d.Dist != 6100.6 || d.Floors != 12 || d.Cal != 2400 || d.Rest != 50 ||
		d.Stress != 31 || d.BBHigh != 95 || d.BBDrain != 55 || d.Vig != 15 || d.Resp != 14.5 || d.SpO2 != 96 || d.Water != 1500 {
		t.Errorf("day = %+v", d)
	}
	if e := got.Days[1]; e.D != "2026-11-16" || e.Steps != 0 {
		t.Errorf("a day with no groups must still decode: %+v", e)
	}
}

func TestTools_SleepIncludingNapOnlyDays(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/sleep-service/sleep/dailySleepSummariesByDate", `[
	 {"id":7,"calendarDate":"2026-11-15","sleepTimeSeconds":27000,"deepSleepSeconds":5400,"lightSleepSeconds":15000,"remSleepSeconds":6600,
	  "awakeSleepSeconds":900,"awakeCount":3,"sleepStartTimestampGMT":1794698754000,"sleepEndTimestampGMT":1794725754000,
	  "avgHeartRate":52.46,"averageSpO2Value":95.5,"lowestSpO2Value":90,"sleepScoreFeedback":"POSITIVE_LONG_AND_DEEP",
	  "sleepScores":{"overall":{"value":81,"qualifierKey":"GOOD"}},"sleepNeed":{"baseline":420,"actual":480,"feedback":"HIGHLY_INCREASED"}},
	 {"id":0,"calendarDate":"","napTimeSeconds":2640,"sleepNeed":{"calendarDate":"2026-11-16","baseline":420,"actual":440},
	  "dailyNapDTOS":[{"calendarDate":"2026-11-16","napTimeSec":2640,"napStartTimestampGMT":"2026-11-16T10:51:08","napEndTimestampGMT":"2026-11-16T11:35:08","napFeedback":"LONG_DURATION_LOW_NEED"}]}]`)
	type nap struct {
		Start string `json:"start"`
		Dur   int    `json:"duration_s"`
	}
	type night struct {
		Date    string  `json:"date"`
		Has     bool    `json:"has_sleep"`
		Start   string  `json:"start"`
		Total   int     `json:"total_s"`
		Deep    int     `json:"deep_s"`
		Score   int     `json:"score"`
		Quality string  `json:"score_quality"`
		HR      float64 `json:"avg_hr"`
		Need    int     `json:"sleep_need_actual_min"`
		NapS    int     `json:"nap_s"`
		Naps    []nap   `json:"naps"`
	}
	got := decodeResult[dayList[night]](t, callTool(t, cs, "get_sleep", map[string]any{"start": "2026-11-15", "end": "2026-11-16"}))
	if got.Count != 2 {
		t.Fatalf("count = %d", got.Count)
	}
	n := got.Days[0]
	if n.Date != "2026-11-15" || !n.Has || n.Total != 27000 || n.Deep != 5400 || n.Score != 81 || n.Quality != "GOOD" || n.HR != 52.5 || n.Need != 480 || n.Start == "" {
		t.Errorf("night = %+v", n)
	}
	napDay := got.Days[1]
	if napDay.Date != "2026-11-16" || napDay.Has || napDay.NapS != 2640 || len(napDay.Naps) != 1 || napDay.Naps[0].Dur != 2640 || napDay.Naps[0].Start != "2026-11-16T10:51:08Z" {
		t.Errorf("nap-only day = %+v", napDay)
	}
}

func TestTools_HeartRate(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	// 10:00, 10:10, 10:20 fall in the 10:00 half-hour bucket (avg 70); 10:40 in 10:30 (90).
	h.fg.route("/wellness-service/wellness/dailyHeartRate", `{"calendarDate":"2026-11-15","restingHeartRate":50,"minHeartRate":44,"maxHeartRate":170,
	  "lastSevenDaysAvgRestingHeartRate":51,"heartRateValueDescriptors":[{"key":"timestamp","index":0},{"key":"heartrate","index":1}],
	  "heartRateValues":[[1794650400000,60],[1794651000000,70],[1794651600000,80],[1794652800000,90],[1794653400000,null]]}`)
	type sample struct {
		Time string `json:"time"`
		BPM  int    `json:"bpm"`
	}
	type out struct {
		Date    string   `json:"date"`
		Resting int      `json:"resting_hr"`
		Min     int      `json:"min_hr"`
		Max     int      `json:"max_hr"`
		Avg7    int      `json:"avg_resting_hr_7d"`
		Count   int      `json:"sample_count"`
		Samples []sample `json:"samples"`
	}
	plain := decodeResult[out](t, callTool(t, cs, "get_heart_rate", map[string]any{"date": "2026-11-15"}))
	if plain.Date != "2026-11-15" || plain.Resting != 50 || plain.Min != 44 || plain.Max != 170 || plain.Avg7 != 51 || plain.Count != 4 || len(plain.Samples) != 0 {
		t.Errorf("without samples = %+v", plain)
	}
	if q := h.fg.query("/wellness-service/wellness/dailyHeartRate"); q != "date=2026-11-15" {
		t.Errorf("query = %q", q)
	}
	withSamples := decodeResult[out](t, callTool(t, cs, "get_heart_rate", map[string]any{"date": "2026-11-15", "include_samples": true, "bucket_minutes": 30}))
	if len(withSamples.Samples) != 2 || withSamples.Samples[0].BPM != 70 || withSamples.Samples[1].BPM != 90 {
		t.Fatalf("buckets = %+v, want two averaged buckets 70 and 90", withSamples.Samples)
	}
	if !strings.HasSuffix(withSamples.Samples[0].Time, ":00:00Z") || !strings.HasSuffix(withSamples.Samples[1].Time, ":30:00Z") {
		t.Errorf("bucket starts = %v", withSamples.Samples)
	}
}

func TestTools_HeartRateDefaultsToToday(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/wellness-service/wellness/dailyHeartRate", `{"restingHeartRate":50}`)
	out := decodeResult[struct {
		Date string `json:"date"`
	}](t, callTool(t, cs, "get_heart_rate", nil))
	if out.Date != "2026-11-16" || h.fg.query("/wellness-service/wellness/dailyHeartRate") != "date=2026-11-16" {
		t.Errorf("date = %q, query = %q", out.Date, h.fg.query("/wellness-service/wellness/dailyHeartRate"))
	}
}

func TestTools_HRVBodyBatteryStressSteps(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/hrv-service/hrv/daily/2026-11-10/2026-11-16", `{"hrvSummaries":[
	 {"calendarDate":"2026-11-15","weeklyAvg":40,"lastNightAvg":43,"lastNight5MinHigh":69,"status":"BALANCED","feedbackPhrase":"HRV_BALANCED_2",
	  "baseline":{"lowUpper":36,"balancedLow":37,"balancedUpper":50,"markerValue":0.55}}]}`)
	hrv := decodeResult[dayList[struct {
		Date   string `json:"date"`
		Last   int    `json:"last_night_avg_ms"`
		Weekly int    `json:"weekly_avg_ms"`
		Peak   int    `json:"last_night_5min_high_ms"`
		Status string `json:"status"`
		Low    int    `json:"baseline_balanced_low_ms"`
		High   int    `json:"baseline_balanced_high_ms"`
	}]](t, callTool(t, cs, "get_hrv", nil))
	if hrv.Count != 1 || hrv.Days[0].Last != 43 || hrv.Days[0].Weekly != 40 || hrv.Days[0].Peak != 69 || hrv.Days[0].Status != "BALANCED" || hrv.Days[0].Low != 37 || hrv.Days[0].High != 50 {
		t.Errorf("hrv = %+v", hrv)
	}

	h.fg.route("/wellness-service/wellness/bodyBattery/reports/daily", `[{"date":"2026-11-15","charged":60,"drained":45,
	  "bodyBatteryValueDescriptorDTOList":[{"bodyBatteryValueDescriptorIndex":0,"bodyBatteryValueDescriptorKey":"timestamp"},{"bodyBatteryValueDescriptorIndex":1,"bodyBatteryValueDescriptorKey":"bodyBatteryLevel"}],
	  "bodyBatteryValuesArray":[[1794650400000,80],[1794654000000,55],[1794657600000,95],[1794661200000,70]]},
	  {"date":"2026-11-16","charged":0,"drained":0}]`)
	bb := decodeResult[dayList[struct {
		Date    string `json:"date"`
		Charged int    `json:"charged"`
		High    int    `json:"high"`
		Low     int    `json:"low"`
		First   int    `json:"first"`
		Last    int    `json:"last"`
		Count   int    `json:"sample_count"`
	}]](t, callTool(t, cs, "get_body_battery", nil))
	if bb.Count != 2 {
		t.Fatalf("bb = %+v", bb)
	}
	if d := bb.Days[0]; d.Charged != 60 || d.High != 95 || d.Low != 55 || d.First != 80 || d.Last != 70 || d.Count != 4 {
		t.Errorf("body battery day = %+v", d)
	}
	if d := bb.Days[1]; d.Count != 0 || d.High != 0 {
		t.Errorf("a day without samples = %+v", d)
	}

	h.fg.route("/usersummary-service/stats/stress/daily/2026-11-10/2026-11-16", `[{"calendarDate":"2026-11-15","values":{"overallStressLevel":33,"restStressDuration":30000,"lowStressDuration":20000,"mediumStressDuration":9000,"highStressDuration":1200}}]`)
	st := decodeResult[dayList[struct {
		Date  string `json:"date"`
		Level int    `json:"overall_level"`
		Rest  int    `json:"rest_s"`
		High  int    `json:"high_s"`
	}]](t, callTool(t, cs, "get_stress", nil))
	if st.Count != 1 || st.Days[0].Level != 33 || st.Days[0].Rest != 30000 || st.Days[0].High != 1200 {
		t.Errorf("stress = %+v", st)
	}

	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[{"calendarDate":"2026-11-15","totalSteps":5572,"stepGoal":8000,"totalDistance":4067.04}]`)
	sp := decodeResult[dayList[struct {
		Date  string  `json:"date"`
		Steps int     `json:"steps"`
		Goal  int     `json:"goal"`
		Dist  float64 `json:"distance_m"`
	}]](t, callTool(t, cs, "get_steps", nil))
	if sp.Count != 1 || sp.Days[0].Steps != 5572 || sp.Days[0].Goal != 8000 || sp.Days[0].Dist != 4067 {
		t.Errorf("steps = %+v", sp)
	}
}

func TestTools_VO2MaxAndZones(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/metrics-service/metrics/maxmet/daily/2026-11-01/2026-11-16", `[
	 {"generic":{"calendarDate":"2026-11-10","sport":"RUNNING","subSport":"GENERIC","vo2MaxValue":47.0,"vo2MaxPreciseValue":46.94},"cycling":null},
	 {"generic":{"calendarDate":"2026-11-14","sport":"RUNNING","subSport":"GENERIC","vo2MaxValue":48.0,"vo2MaxPreciseValue":47.6},
	  "cycling":{"calendarDate":"2026-11-14","sport":"CYCLING","subSport":"ROAD","vo2MaxValue":52.0,"vo2MaxPreciseValue":51.8}}]`)
	type reading struct {
		Date    string  `json:"date"`
		Sport   string  `json:"sport"`
		Value   float64 `json:"vo2max"`
		Rounded float64 `json:"vo2max_rounded"`
	}
	type vo2 struct {
		General *reading `json:"general"`
		Cycling *reading `json:"cycling"`
	}
	got := decodeResult[dayList[vo2]](t, callTool(t, cs, "get_vo2max", map[string]any{"start": "2026-11-01", "end": "2026-11-16", "sport": "running"}))
	if got.Count != 2 || got.Days[0].General.Value != 46.9 || got.Days[0].General.Rounded != 47 || got.Days[0].Cycling != nil {
		t.Fatalf("vo2 = %+v", got)
	}
	if c := got.Days[1].Cycling; c == nil || c.Sport != "CYCLING" || c.Value != 51.8 {
		t.Errorf("cycling reading = %+v", c)
	}
	if q := h.fg.query("/metrics-service/metrics/maxmet/daily/2026-11-01/2026-11-16"); q != "sport=running" {
		t.Errorf("sport filter not forwarded: %q", q)
	}

	h.fg.route("/biometric-service/heartRateZones/v2", `[
	 {"trainingMethod":"LACTATE_THRESHOLD","restingHeartRateUsed":50,"lactateThresholdHeartRateUsed":170,"zone1Floor":120,"zone2Floor":140,"zone3Floor":155,"zone4Floor":165,"zone5Floor":175,"maxHeartRateUsed":190,"sport":"DEFAULT"},
	 {"trainingMethod":"LACTATE_THRESHOLD","restingHeartRateUsed":50,"lactateThresholdHeartRateUsed":170,"zone1Floor":110,"zone2Floor":135,"zone3Floor":150,"zone4Floor":160,"zone5Floor":170,"maxHeartRateUsed":190,"sport":"RUNNING"}]`)
	type zone struct {
		Zone int `json:"zone"`
		Min  int `json:"min_bpm"`
		Max  int `json:"max_bpm"`
	}
	type zones struct {
		Sport      string `json:"sport"`
		Configured bool   `json:"configured"`
		Threshold  int    `json:"lactate_threshold_hr"`
		Max        int    `json:"max_hr"`
		Zones      []zone `json:"zones"`
	}
	run := decodeResult[zones](t, callTool(t, cs, "get_heart_rate_zones", nil)) // defaults to running
	if run.Sport != "RUNNING" || !run.Configured || run.Threshold != 170 || run.Max != 190 || len(run.Zones) != 5 || run.Zones[0].Min != 110 || run.Zones[4].Max != 190 {
		t.Errorf("running zones = %+v", run)
	}
	cyc := decodeResult[zones](t, callTool(t, cs, "get_heart_rate_zones", map[string]any{"sport": "cycling"}))
	if cyc.Sport != "DEFAULT" || cyc.Zones[0].Min != 120 {
		t.Errorf("cycling must fall back to the default zones: %+v", cyc)
	}
}

func TestTools_GarminErrorsBecomeToolErrorsNotProtocolErrors(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.status["/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16"] = 429
	if res := callTool(t, cs, "get_steps", nil); !res.IsError || !strings.Contains(resultText(res), "rate limiting") {
		t.Errorf("429: %q", resultText(res))
	}
	// An unrouted path is a 404 from the fake: surfaced as "not found" guidance.
	if res := callTool(t, cs, "get_stress", nil); !res.IsError || !strings.Contains(resultText(res), "not found") {
		t.Errorf("404: %q", resultText(res))
	}
}

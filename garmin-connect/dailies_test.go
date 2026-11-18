package garminconnect_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

func TestDailySummaries(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `{"data":[
	 {"calendarDate":"2026-11-14","movement":{"steps":{"value":8123,"goal":10000,"distanceInMeters":6100.5},
	   "floorsAscended":{"value":12,"goal":10}},
	  "heartRate":{"minValue":44,"maxValue":160,"restingValue":50},
	  "calories":{"burnedActive":600,"burnedResting":1800,"burnedTotal":2400},
	  "stress":{"avgLevel":31,"maxLevel":88,"qualifier":"balanced","highStressDurationInMillis":120000},
	  "bodyBattery":{"chargedValue":60,"drainedValue":55,"latestValue":40,"minValue":10,"maxValue":95},
	  "respiration":{"avgValue":14.5}, "pulseOx":{"avgValue":96}, "hydration":{"consumedInMl":1500,"goalInMl":2500},
	  "intensityMinutes":{"goal":150,"moderate":30,"vigorous":15}},
	 {"calendarDate":"2026-11-15"}]}`)
	got, err := c.DailySummaries(context.Background(), d1, d2)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if q := last.URL.Query(); q.Get("startDate") != "2026-11-09" || q.Get("endDate") != "2026-11-15" {
		t.Errorf("query = %v", q)
	}
	s := got[0]
	if s.CalendarDate != "2026-11-14" || s.Steps == nil || s.Steps.Value != 8123 || s.Steps.Goal != 10000 || s.Steps.DistanceMeters != 6100.5 {
		t.Errorf("steps = %+v", s.Steps)
	}
	if s.FloorsAscended == nil || s.FloorsAscended.Value != 12 {
		t.Errorf("floors = %+v", s.FloorsAscended)
	}
	if s.HeartRate.Resting != 50 || s.Calories.Total != 2400 || s.Stress.AvgLevel != 31 || s.Stress.HighMillis != 120000 {
		t.Errorf("groups wrong: %+v %+v %+v", s.HeartRate, s.Calories, s.Stress)
	}
	if s.BodyBattery.Max != 95 || s.Respiration.Avg != 14.5 || s.PulseOx.Avg != 96 || s.Hydration.GoalMl != 2500 || s.IntensityMinutes.Vigorous != 15 {
		t.Errorf("more groups wrong")
	}
	empty := got[1]
	if empty.Steps != nil || empty.HeartRate != nil || empty.Stress != nil {
		t.Errorf("absent groups must be nil: %+v", empty)
	}
	if len(empty.Raw()) == 0 {
		t.Error("raw missing")
	}
}

func TestDailySummaries_EmptyAndRange(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{}`)
	if got, err := c.DailySummaries(context.Background(), d1, d2); err != nil || len(got) != 0 {
		t.Errorf("empty envelope: %v %v", got, err)
	}
	if _, err := c.DailySummaries(context.Background(), d2, d1); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.DailySummaries(context.Background(), d1, d1); err != nil {
		t.Errorf("single-day range rejected: %v", err)
	}
}

func TestStepsAndStress(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[{"calendarDate":"2026-11-14","totalSteps":9000,"stepGoal":8000,"totalDistance":6500.5,"unmapped":1}]`)
	steps, err := c.Steps(context.Background(), d1, d2)
	if err != nil || len(steps) != 1 || steps[0].TotalSteps != 9000 || steps[0].TotalDistance != 6500.5 {
		t.Fatalf("steps = %+v err %v", steps, err)
	}
	if !strings.Contains(string(steps[0].Raw()), `"unmapped":1`) {
		t.Errorf("steps raw = %s", steps[0].Raw())
	}
	if last.URL.Path != "/usersummary-service/stats/steps/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last.URL.Path)
	}

	c2, last2 := serve(t, `[{"calendarDate":"2026-11-14","values":{"overallStressLevel":33,"restStressDuration":100,"highStressDuration":50},"unmapped":2}]`)
	st, err := c2.Stress(context.Background(), d1, d2)
	if err != nil || len(st) != 1 || st[0].Values.OverallLevel != 33 || st[0].Values.HighSeconds != 50 {
		t.Fatalf("stress = %+v err %v", st, err)
	}
	if !strings.Contains(string(st[0].Raw()), `"unmapped":2`) {
		t.Errorf("stress raw = %s", st[0].Raw())
	}
	if last2.URL.Path != "/usersummary-service/stats/stress/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last2.URL.Path)
	}
	for _, f := range []func() error{
		func() error { _, err := c.Steps(context.Background(), d2, d1); return err },
		func() error { _, err := c.Stress(context.Background(), d2, d1); return err },
	} {
		if err := f(); !errors.Is(err, garminconnect.ErrInvalidRange) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestStepsAndStress_SplitAt28Days(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(c *garminconnect.Client) (int, error){
		"steps": func(c *garminconnect.Client) (int, error) {
			r, err := c.Steps(context.Background(), day("2026-09-01"), day("2026-10-30"))
			return len(r), err
		},
		"stress": func(c *garminconnect.Client) (int, error) {
			r, err := c.Stress(context.Background(), day("2026-09-01"), day("2026-10-30"))
			return len(r), err
		},
	} {
		rec := &windowRecorder{}
		c := newTestClient(t, rec.handler("", "", func(s string) string { return `[{"calendarDate":"` + s + `"}]` }))
		n, err := call(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "2026-09-01..2026-09-28 2026-09-29..2026-10-26 2026-10-27..2026-10-30" // 28 + 28 + 4
		if rec.summary() != want || n != 3 {
			t.Errorf("%s: requests = %s (n=%d)\nwant %s", name, rec.summary(), n, want)
		}
	}
}

func TestDailySummaries_SplitAt300Days(t *testing.T) {
	t.Parallel()
	rec := &windowRecorder{}
	c := newTestClient(t, rec.handler("startDate", "endDate", func(s string) string {
		return `{"data":[{"calendarDate":"` + s + `"}]}`
	}))
	got, err := c.DailySummaries(context.Background(), day("2025-01-01"), day("2026-11-15")) // 684 days
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.windows) != 3 || rec.windows[0] != [2]string{"2025-01-01", "2025-10-27"} || rec.windows[2][1] != "2026-11-15" || len(got) != 3 {
		t.Errorf("requests = %s (n=%d)", rec.summary(), len(got))
	}
}

func TestStress_KnownKeysAndUnitsDecode(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[{"calendarDate":"2026-11-14","values":{"overallStressLevel":33,"restStressDuration":30000,
	  "lowStressDuration":20000,"mediumStressDuration":9000,"highStressDuration":1200}}]`)
	got, err := c.Stress(context.Background(), day("2026-11-14"), day("2026-11-14"))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if last.URL.Path != "/usersummary-service/stats/stress/daily/2026-11-14/2026-11-14" {
		t.Errorf("path = %s", last.URL.Path)
	}
	v := got[0].Values
	if got[0].CalendarDate != "2026-11-14" || v.OverallLevel != 33 || v.RestSeconds != 30000 || v.LowSeconds != 20000 || v.MediumSeconds != 9000 || v.HighSeconds != 1200 {
		t.Errorf("values = %+v", v)
	}
	if v.Other != nil {
		t.Errorf("Other must be nil when there are no extra keys: %v", v.Other)
	}
}

func TestStress_UnknownKeysAreKeptNotDropped(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"calendarDate":"2026-11-14","values":{"overallStressLevel":40,"activityStressDuration":600,
	  "uncategorizedStressDuration":120.5,"note":"text","missing":null,"flag":true}}]`)
	got, err := c.Stress(context.Background(), day("2026-11-14"), day("2026-11-14"))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	v := got[0].Values
	if v.OverallLevel != 40 {
		t.Errorf("known key lost: %+v", v)
	}
	if len(v.Other) != 2 || v.Other["activityStressDuration"] != 600 || v.Other["uncategorizedStressDuration"] != 120.5 {
		t.Errorf("Other = %v, want the two numeric unknown keys only (text, null and bool ignored)", v.Other)
	}
}

func TestStress_EmptyOrNullValuesDecode(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`[{"calendarDate":"2026-11-14","values":{}}]`,
		`[{"calendarDate":"2026-11-14","values":null}]`,
		`[{"calendarDate":"2026-11-14"}]`,
	} {
		c, _ := serve(t, body)
		got, err := c.Stress(context.Background(), day("2026-11-14"), day("2026-11-14"))
		if err != nil || len(got) != 1 || got[0].Values.OverallLevel != 0 || got[0].Values.Other != nil {
			t.Errorf("body %s: %+v err %v", body, got, err)
		}
	}
	bad, _ := serve(t, `[{"calendarDate":"2026-11-14","values":"oops"}]`)
	if _, err := bad.Stress(context.Background(), day("2026-11-14"), day("2026-11-14")); err == nil {
		t.Error("values of the wrong JSON type must be an error")
	}
}

func TestStressValues_MarshalIncludesOtherOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	plain, _ := json.Marshal(garminconnect.StressValues{OverallLevel: 30})
	if strings.Contains(string(plain), "other") {
		t.Errorf("empty Other must be omitted: %s", plain)
	}
	extra, _ := json.Marshal(garminconnect.StressValues{Other: map[string]float64{"x": 1}})
	if !strings.Contains(string(extra), `"other":{"x":1}`) {
		t.Errorf("json = %s", extra)
	}
}

// --- Heart rate: abnormal-HR fields from Garmin's schema -----------------------

func TestStressValues_OtherSurvivesEncodingAndDecoding(t *testing.T) {
	t.Parallel()
	in := garminconnect.StressValues{OverallLevel: 30, RestSeconds: 100, Other: map[string]float64{"newMetric": 7}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out garminconnect.StressValues
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Other["newMetric"] != 7 || out.RestSeconds != 100 || out.OverallLevel != 30 {
		t.Errorf("round trip: %+v from %s", out, b)
	}
}

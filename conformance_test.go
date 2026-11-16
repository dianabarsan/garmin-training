package garmin_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	garmin "github.com/dianabarsan/garmin-training"
)

// TestShapesDecode serves every response shape captured from the live API
// (testdata/shapes, values blanked) through the real client and requires the
// typed models to decode them. A type mismatch between a model and what
// Garmin really sends fails here.
func TestShapesDecode(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	week := day.AddDate(0, 0, -6)

	// Maps URL path -> shape file. Steps and stress embed dates in the path.
	routes := map[string]string{
		"/activitylist-service/activities/search/activities":            "activities_search",
		"/activity-service/activity/42":                                 "activity_detail",
		"/activity-service/activity/activityTypes/simple":               "activity_types",
		"/usersummary-service/v2/dailysummaries":                        "daily_summaries",
		"/usersummary-service/stats/steps/daily/2026-11-09/2026-11-15":  "steps_daily",
		"/usersummary-service/stats/stress/daily/2026-11-09/2026-11-15": "stress_daily",
		"/sleep-service/sleep/dailySleep":                               "sleep_daily",
		"/sleep-service/sleep/dailySleepSummariesByDate":                "sleep_summaries",
		"/wellness-service/wellness/dailyHeartRate":                     "heart_rate_daily",
		"/hrv-service/hrv/2026-11-15":                                   "hrv_day",
		"/wellness-service/wellness/bodyBattery/reports/daily":          "body_battery",
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unrouted path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", "shapes", name+".json"))
		if err != nil {
			t.Errorf("missing shape %s: %v", name, err)
			http.Error(w, "x", 500)
			return
		}
		_, _ = w.Write(b)
	}))
	ctx := context.Background()

	check := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	acts, err := c.Activities(ctx, garmin.ActivitiesOptions{})
	check("Activities", err)
	if len(acts) == 0 || len(acts[0].Raw()) == 0 {
		t.Errorf("Activities returned %d items / no raw", len(acts))
	}
	_, err = c.ActivityDetail(ctx, 42)
	check("ActivityDetail", err)
	_, err = c.ActivityTypeKeys(ctx)
	check("ActivityTypeKeys", err)
	_, err = c.DailySummaries(ctx, week, day)
	check("DailySummaries", err)
	_, err = c.Steps(ctx, week, day)
	check("Steps", err)
	_, err = c.Stress(ctx, week, day)
	check("Stress", err)
	_, err = c.Sleep(ctx, day)
	check("Sleep", err)
	_, err = c.SleepRange(ctx, week, day)
	check("SleepRange", err)
	_, err = c.HeartRate(ctx, day)
	check("HeartRate", err)
	_, err = c.HRV(ctx, day)
	check("HRV", err)
	_, err = c.BodyBattery(ctx, week, day)
	check("BodyBattery", err)
}

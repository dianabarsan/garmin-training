package garmin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	garmin "github.com/dianabarsan/garmin-training"
)

var (
	d1 = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	d2 = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
)

// serve returns a client whose server replies with body to every request and
// records the last request.
func serve(t *testing.T, body string) (*garmin.Client, *http.Request) {
	t.Helper()
	last := new(http.Request)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r.Clone(context.Background())
		_, _ = w.Write([]byte(body))
	}))
	return c, last
}

func TestTimestamp_Unmarshal(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 15, 6, 30, 5, 0, time.UTC)
	tests := []struct {
		name, in string
		want     time.Time
		wantErr  bool
	}{
		{"space layout", `"2026-11-15 06:30:05"`, want, false},
		{"fractional", `"2026-11-15 06:30:05.0"`, want, false},
		{"T layout", `"2026-11-15T06:30:05.0"`, want, false},
		{"rfc3339 with offset", `"2026-11-15T07:30:05+01:00"`, want, false},
		{"epoch ms", strconv.FormatInt(want.UnixMilli(), 10), want, false},
		{"epoch ms float", strconv.FormatInt(want.UnixMilli(), 10) + ".0", want, false},
		{"null", `null`, time.Time{}, false},
		{"empty string", `""`, time.Time{}, false},
		{"garbage string", `"yesterday"`, time.Time{}, true},
		{"bool", `true`, time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ts garmin.Timestamp
			err := json.Unmarshal([]byte(tc.in), &ts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !ts.Equal(tc.want) {
				t.Errorf("got %v, want %v", ts.Time, tc.want)
			}
			if !tc.wantErr && ts.Location() != time.UTC && !ts.IsZero() {
				t.Errorf("location = %v, want UTC", ts.Location())
			}
		})
	}
}

func TestTimestamp_RoundTrip(t *testing.T) {
	t.Parallel()
	in := garmin.Timestamp{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out garmin.Timestamp
	if err := json.Unmarshal(b, &out); err != nil || !out.Equal(in.Time) {
		t.Errorf("round trip: %s -> %v (%v)", b, out.Time, err)
	}
}

const activityJSON = `{
  "activityId": 9001, "activityName": "Morning Run",
  "activityType": {"typeId": 1, "typeKey": "running", "parentTypeId": 17},
  "startTimeGMT": "2026-11-15 06:30:05", "distance": 10234.5, "duration": 3000.25,
  "averageHR": 150.5, "maxHR": 178, "steps": 9000, "calories": 640,
  "aerobicTrainingEffect": 3.4, "anaerobicTrainingEffect": 1.2, "activityTrainingLoad": 120.5,
  "hrTimeInZone_1": 100, "hrTimeInZone_2": 200.5, "hrTimeInZone_3": 300, "hrTimeInZone_4": 400, "hrTimeInZone_5": 50,
  "someFutureField": {"x": 1}
}`

func TestActivities_DecodesAndKeepsRaw(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "["+activityJSON+"]")
	got, err := c.Activities(context.Background(), garmin.ActivitiesOptions{})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d, err %v", len(got), err)
	}
	a := got[0]
	if a.ID != 9001 || a.Name != "Morning Run" || a.Type.Key != "running" || a.Type.ParentID != 17 {
		t.Errorf("identity fields wrong: %+v", a)
	}
	if a.Distance != 10234.5 || a.Duration != 3000.25 || a.AverageHR != 150.5 || a.MaxHR != 178 || a.Steps != 9000 {
		t.Errorf("metrics wrong: %+v", a.ActivityMetrics)
	}
	if a.AerobicTrainingEffect != 3.4 || a.AnaerobicTrainingEffect != 1.2 || a.TrainingLoad != 120.5 {
		t.Errorf("training scores wrong: %+v", a.ActivityMetrics)
	}
	if want := [5]float64{100, 200.5, 300, 400, 50}; a.HRTimeInZone != want {
		t.Errorf("HRTimeInZone = %v, want %v", a.HRTimeInZone, want)
	}
	if want := time.Date(2026, 11, 15, 6, 30, 5, 0, time.UTC); !a.StartTime.Equal(want) {
		t.Errorf("StartTime = %v", a.StartTime.Time)
	}
	if !strings.Contains(string(a.Raw()), "someFutureField") {
		t.Errorf("Raw lost unmodelled fields: %s", a.Raw())
	}
}

func TestActivities_Query(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts garmin.ActivitiesOptions
		want url.Values
	}{
		{"defaults", garmin.ActivitiesOptions{}, url.Values{"start": {"0"}, "limit": {"99"}}},
		{"paging", garmin.ActivitiesOptions{Start: 40, Limit: 10}, url.Values{"start": {"40"}, "limit": {"10"}}},
		{"child type only", garmin.ActivitiesOptions{SubTypeKey: "strength_training"},
			url.Values{"start": {"0"}, "limit": {"99"}, "activitySubType": {"strength_training"}}},
		{"parent and child", garmin.ActivitiesOptions{TypeKey: "running", SubTypeKey: "trail_running"},
			url.Values{"start": {"0"}, "limit": {"99"}, "activityType": {"running"}, "activitySubType": {"trail_running"}}},
		{"filters", garmin.ActivitiesOptions{StartDate: d1, EndDate: d2, TypeKey: "running"},
			url.Values{"start": {"0"}, "limit": {"99"}, "startDate": {"2026-11-09"}, "endDate": {"2026-11-15"}, "activityType": {"running"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, last := serve(t, "[]")
			if _, err := c.Activities(context.Background(), tc.opts); err != nil {
				t.Fatal(err)
			}
			if got := last.URL.Query(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("query = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActivities_InvalidOptions(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "[]")
	if _, err := c.Activities(context.Background(), garmin.ActivitiesOptions{StartDate: d2, EndDate: d1}); !errors.Is(err, garmin.ErrInvalidRange) {
		t.Errorf("reversed range err = %v", err)
	}
	if _, err := c.Activities(context.Background(), garmin.ActivitiesOptions{Start: -1}); err == nil {
		t.Error("negative start accepted")
	}
}

func TestActivities_BadJSON(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"not":"a list"}`)
	if _, err := c.Activities(context.Background(), garmin.ActivitiesOptions{}); err == nil {
		t.Error("expected decode error")
	}
}

// pagedServer serves total activities, honouring start/limit but capping each
// page at maxPage like a server with its own limit would.
func pagedServer(t *testing.T, total, maxPage int, pages *int) *garmin.Client {
	t.Helper()
	return newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*pages++
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		limit = min(limit, maxPage)
		var items []string
		for i := start; i < min(start+limit, total); i++ {
			items = append(items, fmt.Sprintf(`{"activityId":%d}`, i))
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	}))
}

func TestAllActivities(t *testing.T) {
	t.Parallel()
	t.Run("walks every page even when server caps page size", func(t *testing.T) {
		t.Parallel()
		var pages int
		c := pagedServer(t, 25, 10, &pages) // asks for 20, gets 10
		var ids []int64
		for a, err := range c.AllActivities(context.Background(), garmin.ActivitiesOptions{}) {
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, a.ID)
		}
		if len(ids) != 25 {
			t.Fatalf("got %d activities, want 25", len(ids))
		}
		for i, id := range ids {
			if id != int64(i) {
				t.Fatalf("ids[%d] = %d (gap or duplicate)", i, id)
			}
		}
		if pages != 4 { // 10 + 10 + 5 + empty
			t.Errorf("pages = %d, want 4", pages)
		}
	})
	t.Run("stops fetching when the consumer breaks", func(t *testing.T) {
		t.Parallel()
		var pages int
		c := pagedServer(t, 100, 10, &pages)
		n := 0
		for _, err := range c.AllActivities(context.Background(), garmin.ActivitiesOptions{}) {
			if err != nil {
				t.Fatal(err)
			}
			if n++; n == 3 {
				break
			}
		}
		if pages != 1 {
			t.Errorf("pages = %d, want 1", pages)
		}
	})
	t.Run("respects starting offset", func(t *testing.T) {
		t.Parallel()
		var pages int
		c := pagedServer(t, 12, 50, &pages)
		n := 0
		for a, err := range c.AllActivities(context.Background(), garmin.ActivitiesOptions{Start: 10}) {
			if err != nil {
				t.Fatal(err)
			}
			if a.ID < 10 {
				t.Errorf("saw id %d before offset", a.ID)
			}
			n++
		}
		if n != 2 {
			t.Errorf("n = %d, want 2", n)
		}
	})
	t.Run("surfaces errors once then stops", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusForbidden)
		}))
		var errs int
		for _, err := range c.AllActivities(context.Background(), garmin.ActivitiesOptions{}) {
			if !errors.Is(err, garmin.ErrForbidden) {
				t.Errorf("err = %v", err)
			}
			errs++
		}
		if errs != 1 {
			t.Errorf("errs = %d, want 1", errs)
		}
	})
}

func TestActivityDetail(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `{
	  "activityId": 9001, "activityName": "Run",
	  "activityTypeDTO": {"typeId": 1, "typeKey": "running", "parentTypeId": 17},
	  "summaryDTO": {"startTimeGMT": "2026-11-15 06:30:05.0", "distance": 5000, "minHR": 90, "trainingEffect": 2.7,
	                 "averageMovingSpeed": 3.1},
	  "timeZoneUnitDTO": {"timeZone": "Europe/Bucharest"}, "extra": true}`)
	d, err := c.ActivityDetail(context.Background(), 9001)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/activity-service/activity/9001" {
		t.Errorf("path = %s", last.URL.Path)
	}
	if d.ID != 9001 || d.Type.Key != "running" || d.TimeZone != "Europe/Bucharest" {
		t.Errorf("detail = %+v", d)
	}
	if d.Summary.Distance != 5000 || d.Summary.MinHR != 90 || d.Summary.AverageMovingSpeed != 3.1 {
		t.Errorf("summary = %+v", d.Summary)
	}
	if d.Summary.AerobicTrainingEffect != 2.7 {
		t.Errorf("trainingEffect not mapped to aerobic: %v", d.Summary.AerobicTrainingEffect)
	}
	if !strings.Contains(string(d.Raw()), `"extra"`) {
		t.Error("raw not retained")
	}
}

func TestActivityDetail_NotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.NotFoundHandler())
	if _, err := c.ActivityDetail(context.Background(), 1); !errors.Is(err, garmin.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestActivityTypeKeys(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"key":"running"},{"key":"cycling"}]`)
	got, err := c.ActivityTypeKeys(context.Background())
	if err != nil || strings.Join(got, ",") != "running,cycling" {
		t.Errorf("got %v err %v", got, err)
	}
}

func TestDownloadActivity(t *testing.T) {
	t.Parallel()
	payload := []byte("PK\x03\x04binary\x00data")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download-service/files/activity/77" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/zip") {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write(payload)
	}))
	var buf bytes.Buffer
	if err := c.DownloadActivity(context.Background(), 77, &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Errorf("body = %q", buf.Bytes())
	}
}

func TestDownloadActivity_Error(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.NotFoundHandler())
	var buf bytes.Buffer
	if err := c.DownloadActivity(context.Background(), 1, &buf); !errors.Is(err, garmin.ErrNotFound) || buf.Len() != 0 {
		t.Errorf("err = %v, wrote %d bytes", err, buf.Len())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestDownloadActivity_WriterError(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "data")
	if err := c.DownloadActivity(context.Background(), 1, failWriter{}); err == nil {
		t.Error("expected writer error")
	}
}

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
	if _, err := c.DailySummaries(context.Background(), d2, d1); !errors.Is(err, garmin.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.DailySummaries(context.Background(), d1, d1); err != nil {
		t.Errorf("single-day range rejected: %v", err)
	}
}

func TestStepsAndStress(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[{"calendarDate":"2026-11-14","totalSteps":9000,"stepGoal":8000,"totalDistance":6500.5}]`)
	steps, err := c.Steps(context.Background(), d1, d2)
	if err != nil || len(steps) != 1 || steps[0].TotalSteps != 9000 || steps[0].TotalDistance != 6500.5 {
		t.Fatalf("steps = %+v err %v", steps, err)
	}
	if last.URL.Path != "/usersummary-service/stats/steps/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last.URL.Path)
	}

	c2, last2 := serve(t, `[{"calendarDate":"2026-11-14","values":{"overallStressLevel":33,"restStressDuration":100,"highStressDuration":50}}]`)
	st, err := c2.Stress(context.Background(), d1, d2)
	if err != nil || len(st) != 1 || st[0].Values.OverallLevel != 33 || st[0].Values.HighSeconds != 50 {
		t.Fatalf("stress = %+v err %v", st, err)
	}
	if last2.URL.Path != "/usersummary-service/stats/stress/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last2.URL.Path)
	}
	for _, f := range []func() error{
		func() error { _, err := c.Steps(context.Background(), d2, d1); return err },
		func() error { _, err := c.Stress(context.Background(), d2, d1); return err },
	} {
		if err := f(); !errors.Is(err, garmin.ErrInvalidRange) {
			t.Errorf("err = %v", err)
		}
	}
}

const sleepJSON = `{"id":5,"calendarDate":"2026-11-15",
 "sleepStartTimestampGMT":1789000000000,"sleepEndTimestampGMT":1789028800000,
 "sleepTimeSeconds":28800,"deepSleepSeconds":5000,"lightSleepSeconds":15000,"remSleepSeconds":6000,"awakeSleepSeconds":2800,
 "awakeCount":3,"avgHeartRate":52.5,"averageSpO2Value":95.5,"lowestSpO2Value":90,"breathingDisruptionSeverity":"MILD",
 "sleepScores":{"overall":{"value":82,"qualifierKey":"GOOD"},"deepPercentage":{"value":18,"qualifierKey":"EXCELLENT"}},
 "sleepScoreFeedback":"POSITIVE_LONG_AND_DEEP","autoSleepStartTimestampGMT":null,"sleepQualityTypePK":null}`

func TestSleep(t *testing.T) {
	t.Parallel()
	c, last := serve(t, sleepJSON)
	s, err := c.Sleep(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Query().Get("date") != "2026-11-15" {
		t.Errorf("query = %v", last.URL.Query())
	}
	if s.TotalSeconds != 28800 || s.DeepSeconds != 5000 || s.AwakeCount != 3 || s.AvgHeartRate != 52.5 || s.LowestSpO2 != 90 {
		t.Errorf("sleep = %+v", s)
	}
	if got := s.End.Sub(s.Start.Time); got != 28800*time.Second {
		t.Errorf("end-start = %v", got)
	}
	if s.Scores.Overall.Value != 82 || s.Scores.Overall.Qualifier != "GOOD" || s.Scores.Deep.Qualifier != "EXCELLENT" {
		t.Errorf("scores = %+v", s.Scores)
	}
	if s.BreathingDisruptionSeverity != "MILD" || s.ScoreFeedback == "" {
		t.Errorf("text fields = %+v", s)
	}
}

func TestSleepRange(t *testing.T) {
	t.Parallel()
	c, last := serve(t, "["+sleepJSON+","+sleepJSON+"]")
	got, err := c.SleepRange(context.Background(), d1, d2)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if q := last.URL.Query(); q.Get("startDate") != "2026-11-09" || q.Get("endDate") != "2026-11-15" {
		t.Errorf("query = %v", q)
	}
	if _, err := c.SleepRange(context.Background(), d2, d1); !errors.Is(err, garmin.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
}

func TestHeartRate(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"calendarDate":"2026-11-15","restingHeartRate":50,"minHeartRate":44,"maxHeartRate":170,
	  "lastSevenDaysAvgRestingHeartRate":51,"startTimestampGMT":"2026-11-14T22:00:00.0",
	  "heartRateValueDescriptors":[{"key":"timestamp","index":0},{"key":"heartrate","index":1}],
	  "heartRateValues":[[1789000000000,60],[1789000120000,null],[1789000240000,62],[1789000360000]]}`)
	h, err := c.HeartRate(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if h.Resting != 50 || h.Min != 44 || h.Max != 170 || h.LastSevenDaysAvgResting != 51 {
		t.Errorf("summary = %+v", h)
	}
	if len(h.Samples) != 2 || h.Samples[0].BPM != 60 || h.Samples[1].BPM != 62 {
		t.Fatalf("samples = %+v (null and short rows must be skipped)", h.Samples)
	}
	if h.Samples[0].Time.UnixMilli() != 1789000000000 {
		t.Errorf("sample time = %v", h.Samples[0].Time.Time)
	}
}

func TestHeartRate_DescriptorOrderRespected(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"heartRateValueDescriptors":[{"key":"heartrate","index":0},{"key":"timestamp","index":1}],
	  "heartRateValues":[[75,1789000000000]]}`)
	h, err := c.HeartRate(context.Background(), d2)
	if err != nil || len(h.Samples) != 1 || h.Samples[0].BPM != 75 || h.Samples[0].Time.UnixMilli() != 1789000000000 {
		t.Errorf("samples = %+v err %v", h.Samples, err)
	}
}

func TestHRV(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `{"hrvSummary":{"calendarDate":"2026-11-15","weeklyAvg":55,"lastNightAvg":58,"lastNight5MinHigh":80,
	  "status":"BALANCED","feedbackPhrase":"HRV_BALANCED_1","baseline":{"lowUpper":40,"balancedLow":45,"balancedUpper":65}},
	 "hrvReadings":[{"hrvValue":60,"readingTimeGMT":"2026-11-15T01:05:00.0"},{"hrvValue":52,"readingTimeGMT":"2026-11-15T01:10:00.0"}],
	 "sleepStartTimestampGMT":"2026-11-14T22:00:00.0"}`)
	h, err := c.HRV(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/hrv-service/hrv/2026-11-15" {
		t.Errorf("path = %s", last.URL.Path)
	}
	if h.Summary == nil || h.Summary.LastNightAvg != 58 || h.Summary.Status != "BALANCED" || h.Summary.Baseline.BalancedUpper != 65 {
		t.Fatalf("summary = %+v", h.Summary)
	}
	if len(h.Readings) != 2 || h.Readings[1].Value != 52 || h.Readings[1].Time.Minute() != 10 {
		t.Errorf("readings = %+v", h.Readings)
	}
}

func TestHRV_NoData(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{}`, `{"hrvSummary":null,"hrvReadings":null}`} {
		c, _ := serve(t, body)
		h, err := c.HRV(context.Background(), d2)
		if err != nil || h.Summary != nil || len(h.Readings) != 0 {
			t.Errorf("body %s: %+v err %v", body, h, err)
		}
	}
}

func TestBodyBattery(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[{"date":"2026-11-15","charged":60,"drained":45,"startTimestampGMT":"2026-11-14T22:00:00.0",
	  "bodyBatteryValueDescriptorDTOList":[
	    {"bodyBatteryValueDescriptorIndex":0,"bodyBatteryValueDescriptorKey":"timestamp"},
	    {"bodyBatteryValueDescriptorIndex":1,"bodyBatteryValueDescriptorKey":"bodyBatteryStatus"},
	    {"bodyBatteryValueDescriptorIndex":2,"bodyBatteryValueDescriptorKey":"bodyBatteryLevel"}],
	  "bodyBatteryValuesArray":[[1789000000000,"MEASURED",80],[1789000180000,"MEASURED",null],[1789000360000,"MEASURED",78]]}]`)
	got, err := c.BodyBattery(context.Background(), d1, d2)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if q := last.URL.Query(); q.Get("startDate") != "2026-11-09" || q.Get("endDate") != "2026-11-15" {
		t.Errorf("query = %v", q)
	}
	d := got[0]
	if d.Charged != 60 || d.Drained != 45 {
		t.Errorf("day = %+v", d)
	}
	if len(d.Samples) != 2 || d.Samples[0].Level != 80 || d.Samples[1].Level != 78 {
		t.Errorf("samples = %+v (string column and null level must not break parsing)", d.Samples)
	}
	if _, err := c.BodyBattery(context.Background(), d2, d1); !errors.Is(err, garmin.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
}

func TestUserSettingsStillWorks(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"a":1}`)
	if got, err := c.UserSettings(context.Background()); err != nil || string(got) != `{"a":1}` {
		t.Errorf("got %s err %v", got, err)
	}
}

// Garmin's real Body Battery rows have two columns, not four. Parsing must
// follow the response's own descriptors rather than assume a layout.
func TestBodyBattery_TwoColumnLayoutFromLiveAPI(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"date":"2026-11-15",
	  "bodyBatteryValueDescriptorDTOList":[
	    {"bodyBatteryValueDescriptorIndex":0,"bodyBatteryValueDescriptorKey":"timestamp"},
	    {"bodyBatteryValueDescriptorIndex":1,"bodyBatteryValueDescriptorKey":"bodyBatteryLevel"}],
	  "bodyBatteryValuesArray":[[1789000000000,80],[1789000900000,76]]}]`)
	got, err := c.BodyBattery(context.Background(), d2, d2)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if s := got[0].Samples; len(s) != 2 || s[0].Level != 80 || s[1].Level != 76 {
		t.Errorf("samples = %+v", s)
	}
}

const strengthJSON = `{
  "activityId": 5, "activityName": "Strength",
  "activityType": {"typeId": 13, "typeKey": "strength_training", "parentTypeId": 29},
  "duration": 4003.8, "movingDuration": 2714.0, "calories": 347,
  "activeSets": 7, "totalSets": 7, "totalReps": 80,
  "summarizedExerciseSets": [
    {"category": "SQUAT", "subCategory": "DUMBBELL_FRONT_SQUAT", "reps": 60, "volume": 1800000, "duration": 290926.5, "sets": 4, "maxWeight": 30000},
    {"category": "WARM_UP", "reps": 4, "volume": 0, "duration": 512278, "sets": 1, "maxWeight": 0},
    {"category": "CRUNCH", "subCategory": "CROSSED_ARMS_CRUNCH", "reps": 16, "volume": 0, "duration": 250000, "sets": 2, "maxWeight": 0}
  ]}`

func TestActivities_StrengthExercises(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "["+strengthJSON+"]")
	got, err := c.Activities(context.Background(), garmin.ActivitiesOptions{})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	a := got[0]
	if a.ActiveSets != 7 || a.TotalSets != 7 || a.TotalReps != 80 {
		t.Errorf("totals = %d/%d/%d", a.ActiveSets, a.TotalSets, a.TotalReps)
	}
	if len(a.Exercises) != 3 {
		t.Fatalf("exercises = %+v", a.Exercises)
	}
	sq := a.Exercises[0]
	if sq.Category != "SQUAT" || sq.SubCategory != "DUMBBELL_FRONT_SQUAT" || sq.Sets != 4 || sq.Reps != 60 {
		t.Errorf("squat = %+v", sq)
	}
	// Garmin sends grams and milliseconds.
	if sq.MaxWeightKg != 30 || sq.VolumeKg != 1800 || sq.DurationSeconds != 290.9265 {
		t.Errorf("squat units = %v kg max, %v kg volume, %v s", sq.MaxWeightKg, sq.VolumeKg, sq.DurationSeconds)
	}
	if w := a.Exercises[1]; w.SubCategory != "" || w.MaxWeightKg != 0 || w.VolumeKg != 0 || w.Reps != 4 {
		t.Errorf("warm-up (no subcategory, no weight) = %+v", w)
	}
	var sets, reps int
	var dur float64
	for _, e := range a.Exercises {
		sets += e.Sets
		reps += e.Reps
		dur += e.DurationSeconds
	}
	if sets != a.TotalSets || reps != a.TotalReps {
		t.Errorf("exercise sums %d sets / %d reps != totals %d / %d", sets, reps, a.TotalSets, a.TotalReps)
	}
	if !near(dur, 290.9265+512.278+250, 1e-9) {
		t.Errorf("duration sum = %v", dur)
	}
	if !strings.Contains(string(a.Raw()), "summarizedExerciseSets") {
		t.Error("raw must keep the original exercise data")
	}
}

func TestActivities_NoExercisesForOtherTypes(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"activityId":1}`, `{"activityId":1,"summarizedExerciseSets":null}`, `{"activityId":1,"summarizedExerciseSets":[]}`} {
		c, _ := serve(t, "["+body+"]")
		got, err := c.Activities(context.Background(), garmin.ActivitiesOptions{})
		if err != nil || len(got) != 1 || len(got[0].Exercises) != 0 {
			t.Errorf("body %s: %+v err %v", body, got, err)
		}
	}
}

func TestActivities_ExerciseWithBadTypesFails(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"activityId":1,"summarizedExerciseSets":[{"reps":"many"}]}]`)
	if _, err := c.Activities(context.Background(), garmin.ActivitiesOptions{}); err == nil {
		t.Error("expected decode error")
	}
}

func TestExerciseSummary_MarshalsFriendlyUnits(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(garmin.ExerciseSummary{Category: "SQUAT", Sets: 4, Reps: 60, MaxWeightKg: 30, VolumeKg: 1800, DurationSeconds: 291})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"category":"SQUAT","sets":4,"reps":60,"maxWeightKg":30,"volumeKg":1800,"durationSeconds":291}`
	if string(b) != want {
		t.Errorf("json = %s\nwant   %s", b, want)
	}
}

func TestActivityDetail_StrengthTotals(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"activityId":5,"activityTypeDTO":{"typeKey":"strength_training"},
	  "summaryDTO":{"activeSets":26,"totalSets":26,"totalExerciseReps":341,"duration":4003.8}}`)
	d, err := c.ActivityDetail(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.ActiveSets != 26 || d.Summary.TotalSets != 26 || d.Summary.TotalReps != 341 {
		t.Errorf("summary = %+v", d.Summary)
	}
}

// Invariants against the local, git-ignored capture of a real strength
// session (see TestLiveStoreActivity); skipped when absent.
func TestActivities_LocalRealStrengthInvariants(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("testdata", "live", "strength_list_entry.json"))
	if err != nil {
		t.Skip("no local strength capture")
	}
	c, _ := serve(t, "["+string(b)+"]")
	got, err := c.Activities(context.Background(), garmin.ActivitiesOptions{})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	a := got[0]
	var sets, reps int
	var dur float64
	for _, e := range a.Exercises {
		sets += e.Sets
		reps += e.Reps
		dur += e.DurationSeconds
		if e.Reps > 0 && e.MaxWeightKg > 0 && e.VolumeKg > float64(e.Reps)*e.MaxWeightKg+1e-6 {
			t.Errorf("%s: volume %v kg exceeds reps*maxWeight %v", e.Category, e.VolumeKg, float64(e.Reps)*e.MaxWeightKg)
		}
	}
	if len(a.Exercises) == 0 || sets != a.TotalSets || reps != a.TotalReps {
		t.Errorf("%d exercises: sums %d sets / %d reps vs totals %d / %d", len(a.Exercises), sets, reps, a.TotalSets, a.TotalReps)
	}
	if !near(dur, a.MovingDuration, 1) {
		t.Errorf("exercise time %v s != moving duration %v s", dur, a.MovingDuration)
	}
}

func TestActivities_QueryFromSwaggerOptions(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	tests := []struct {
		name string
		opts garmin.ActivitiesOptions
		want url.Values
	}{
		{"sorting", garmin.ActivitiesOptions{SortBy: "distance", Ascending: true},
			url.Values{"start": {"0"}, "limit": {"99"}, "sortBy": {"distance"}, "sortOrder": {"asc"}}},
		{"descending is Garmin's default so nothing is sent", garmin.ActivitiesOptions{SortBy: "duration"},
			url.Values{"start": {"0"}, "limit": {"99"}, "sortBy": {"duration"}}},
		{"search and event type", garmin.ActivitiesOptions{Search: "long run", EventType: "race"},
			url.Values{"start": {"0"}, "limit": {"99"}, "search": {"long run"}, "eventType": {"race"}}},
		{"favorites only", garmin.ActivitiesOptions{Favorite: &yes},
			url.Values{"start": {"0"}, "limit": {"99"}, "favorite": {"1"}}},
		{"non-favorites only", garmin.ActivitiesOptions{Favorite: &no},
			url.Values{"start": {"0"}, "limit": {"99"}, "favorite": {"0"}}},
		{"include child activities", garmin.ActivitiesOptions{IncludeChildren: true},
			url.Values{"start": {"0"}, "limit": {"99"}, "excludeChildren": {"false"}}},
		{"repeated array params", garmin.ActivitiesOptions{ExcludedSubTypes: []string{"treadmill_running", "indoor_cardio"}, RequiredFields: []string{"averageHR", "calories"}},
			url.Values{"start": {"0"}, "limit": {"99"}, "excludedActivitySubType": {"treadmill_running", "indoor_cardio"}, "requiredField": {"averageHR", "calories"}}},
		{"ranges", garmin.ActivitiesOptions{MinDistance: 10000, MaxDistance: 21097, MinDuration: 600, MaxDuration: 7200,
			MinElapsedDuration: 700, MaxElapsedDuration: 8000, MinElevationGain: 50, MaxElevationGain: 900, MinAvgPower: 150},
			url.Values{"start": {"0"}, "limit": {"99"}, "minDistance": {"10000"}, "maxDistance": {"21097"}, "minDuration": {"600"}, "maxDuration": {"7200"},
				"minElapsedDuration": {"700"}, "maxElapsedDuration": {"8000"}, "minElevation": {"50"}, "maxElevation": {"900"}, "minAvgPower": {"150"}}},
		{"one-sided range", garmin.ActivitiesOptions{MinDistance: 5000},
			url.Values{"start": {"0"}, "limit": {"99"}, "minDistance": {"5000"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, last := serve(t, "[]")
			if _, err := c.Activities(context.Background(), tc.opts); err != nil {
				t.Fatal(err)
			}
			if got := last.URL.Query(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("query = %v\nwant    %v", got, tc.want)
			}
		})
	}
}

func TestActivities_InvalidRangeOptions(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "[]")
	for name, o := range map[string]garmin.ActivitiesOptions{
		"distance min>max":  {MinDistance: 10, MaxDistance: 5},
		"duration min>max":  {MinDuration: 10, MaxDuration: 5},
		"elapsed min>max":   {MinElapsedDuration: 10, MaxElapsedDuration: 5},
		"elevation min>max": {MinElevationGain: 10, MaxElevationGain: 5},
		"negative distance": {MinDistance: -1},
		"negative duration": {MaxDuration: -1},
		"negative power":    {MinAvgPower: -1},
	} {
		if _, err := c.Activities(context.Background(), o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestActivities_ElevationFieldsAndNoDataSentinel(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[
	  {"activityId":1,"activityType":{"typeKey":"running"},"avgElevation":110.2,"minElevation":100.6,"maxElevation":122.8,"elevationGain":125,"elevationLoss":124.5},
	  {"activityId":2,"activityType":{"typeKey":"strength_training"},"avgElevation":-500.0},
	  {"activityId":3,"activityType":{"typeKey":"cycling"},"avgElevation":-20.5,"minElevation":-30,"maxElevation":-10}]`)
	got, err := c.Activities(context.Background(), garmin.ActivitiesOptions{})
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	run := got[0]
	if run.AvgElevation != 110.2 || run.MinElevation != 100.6 || run.MaxElevation != 122.8 || run.ElevationGain != 125 || run.ElevationLoss != 124.5 {
		t.Errorf("outdoor run elevation = %+v", run.ActivityMetrics)
	}
	if st := got[1]; st.AvgElevation != 0 || st.ElevationGain != 0 || st.MinElevation != 0 {
		t.Errorf("-500 sentinel not mapped to zero: %+v", st.ActivityMetrics)
	}
	if !strings.Contains(string(got[1].Raw()), "-500") {
		t.Error("raw must keep Garmin's original value")
	}
	if below := got[2]; below.AvgElevation != -20.5 || below.MinElevation != -30 {
		t.Errorf("genuine below-sea-level elevations must survive: %+v", below.ActivityMetrics)
	}
}

func TestActivityDetail_ElevationFields(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"activityId":5,"summaryDTO":{"elevationGain":125,"elevationLoss":124,"minElevation":100.6,"maxElevation":122.8,"avgElevation":110.2}}`)
	d, err := c.ActivityDetail(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.ElevationGain != 125 || d.Summary.ElevationLoss != 124 || d.Summary.MinElevation != 100.6 || d.Summary.MaxElevation != 122.8 {
		t.Errorf("summary = %+v", d.Summary)
	}
	c2, _ := serve(t, `{"activityId":6,"summaryDTO":{"avgElevation":-500}}`)
	d2, err := c2.ActivityDetail(context.Background(), 6)
	if err != nil || d2.Summary.AvgElevation != 0 {
		t.Errorf("detail sentinel: %+v err %v", d2.Summary, err)
	}
}

package garminconnect_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

const sleepJSON = `{"id":5,"calendarDate":"2026-11-15",
 "sleepStartTimestampGMT":1789000000000,"sleepEndTimestampGMT":1789028800000,
 "sleepTimeSeconds":28800,"deepSleepSeconds":5000,"lightSleepSeconds":15000,"remSleepSeconds":6000,"awakeSleepSeconds":2800,
 "awakeCount":3,"avgHeartRate":52.5,"averageSpO2Value":95.5,"lowestSpO2Value":90,"breathingDisruptionSeverity":"MILD",
 "sleepScores":{"overall":{"value":82,"qualifierKey":"GOOD"},"deepPercentage":{"value":18,"qualifierKey":"EXCELLENT"}},
 "sleepScoreFeedback":"POSITIVE_LONG_AND_DEEP","autoSleepStartTimestampGMT":null,"sleepQualityTypePK":null}`

// Shaped like the Swagger example: ISO timestamps, naps, every optional block.
const sleepSchemaJSON = `{
  "id": 7, "userProfilePK": 99, "calendarDate": "2026-11-16",
  "sleepTimeSeconds": 27000, "napTimeSeconds": 1200,
  "sleepWindowConfirmed": true, "sleepWindowConfirmationType": "enhanced_confirmed_final",
  "sleepStartTimestampGMT": "2026-11-15T22:30:00.000Z", "sleepEndTimestampGMT": "2026-11-16T06:00:00.000Z",
  "sleepStartTimestampLocal": "2026-11-16T00:30:00.000Z",
  "deepSleepSeconds": 5400, "lightSleepSeconds": 15000, "remSleepSeconds": 6600, "awakeSleepSeconds": 900,
  "averageSpO2Value": 95.5, "lowestSpO2Value": 90, "highestSpO2Value": 99,
  "numberOfEventsBelowThreshold": 3, "averageSpO2HRSleep": 54.5, "spo2Threshold": 90,
  "ageGroup": "ADULT", "avgHeartRate": 52.5, "sleepScoreFeedback": "POSITIVE_LONG_AND_DEEP",
  "userNote": "slept badly", "sleepVersion": 2,
  "sleepScores": {
    "overall": {"value": 81, "qualifierKey": "GOOD"},
    "awakeCount": {"qualifierKey": "FAIR", "optimalStart": 0.0, "optimalEnd": 1.0},
    "restlessness": {"qualifierKey": "GOOD", "optimalStart": 5.0, "optimalEnd": 30.0},
    "stress": {"qualifierKey": "FAIR", "optimalStart": 0.0, "optimalEnd": 15.0},
    "totalDuration": {"qualifierKey": "EXCELLENT", "optimalStart": 28800.0, "optimalEnd": 28800.0},
    "deepPercentage": {"value": 20, "qualifierKey": "EXCELLENT", "optimalStart": 16.0, "optimalEnd": 33.0, "idealStartInSeconds": 4320.0, "idealEndInSeconds": 8910.0}
  },
  "sleepNeed": {"calendarDate": "2026-11-16", "timestampGmt": "2026-11-15T18:40:12", "baseline": 420, "actual": 480,
    "feedback": "HIGHLY_INCREASED", "trainingFeedback": "TODAYS_LOAD_AND_CHRONIC", "sleepHistoryAdjustment": "INCREASING",
    "hrvAdjustment": "NO_CHANGE", "napAdjustment": "DECREASING", "displayedForTheDay": true,
    "recommendedBedtimeStartMins": 1360, "recommendedBedtimeEndMins": 0},
  "nextSleepNeed": {"baseline": 420, "actual": 450, "feedback": "INCREASED"},
  "sleepAlignment": {"calendarDate": "2026-11-16", "status": "BEHIND", "sleepNeedBaselineMins": 420,
    "optimalSleepWindowStartMins": -20, "optimalSleepWindowEndMins": 400, "optimalSleepWindowMidpointMins": 190, "lastSleepMidpointMins": 286},
  "dailyNapDTOS": [{"napTimeSec": 1200, "napStartTimestampGMT": "2026-11-16T13:00:00.000Z",
    "napEndTimestampGMT": "2026-11-16T13:20:00.000Z", "napFeedback": "IDEAL_TIMING_LONG_DURATION"}],
  "breathingDisruptionSeverity": "MILD"}`

// The shape Garmin returns for a day with a nap but no main sleep: id 0, an
// empty calendarDate, and the real date only inside the nested blocks.
const napOnlyDayJSON = `{"id":0,"calendarDate":"","napTimeSeconds":2640,
  "dailyNapDTOS":[{"calendarDate":"2026-06-11","napTimeSec":2640,"napStartTimestampGMT":"2026-06-11T10:51:08",
    "napEndTimestampGMT":"2026-06-11T11:35:08","napFeedback":"LONG_DURATION_LOW_NEED","napSource":0,"createTimestampGMT":null}],
  "sleepNeed":{"calendarDate":"2026-06-11","baseline":420,"actual":440,"feedback":"INCREASED"},
  "nextSleepNeed":{"calendarDate":"2026-06-12","baseline":420,"actual":420,"feedback":"NO_CHANGE_BALANCED"}}`

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
	if _, err := c.SleepRange(context.Background(), d2, d1); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
}

func TestSleep_FullDocumentedSchema(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, sleepSchemaJSON)
	s, err := c.Sleep(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if !s.WindowConfirmed || s.WindowConfirmationType != "enhanced_confirmed_final" || s.AgeGroup != "ADULT" || s.SleepVersion != 2 || s.UserNote != "slept badly" {
		t.Errorf("flags/text = %+v", s)
	}
	if want := time.Date(2026, 11, 15, 22, 30, 0, 0, time.UTC); !s.Start.Equal(want) {
		t.Errorf("ISO start = %v, want %v", s.Start.Time, want)
	}
	if s.SpO2EventsBelowThreshold != 3 || s.SpO2Threshold != 90 || s.AvgSpO2SleepHeartRate != 54.5 || s.NapSeconds != 1200 {
		t.Errorf("spo2/nap = %+v", s)
	}
	sc := s.Scores
	if sc.Overall.Value != 81 || sc.Deep.Value != 20 || sc.Deep.IdealEndSeconds != 8910 || sc.Deep.OptimalStart != 16 {
		t.Errorf("scores overall/deep = %+v / %+v", sc.Overall, sc.Deep)
	}
	if sc.AwakeCount.Qualifier != "FAIR" || sc.Restlessness.OptimalEnd != 30 || sc.Stress.Qualifier != "FAIR" || sc.TotalDuration.Qualifier != "EXCELLENT" || sc.TotalDuration.OptimalStart != 28800 {
		t.Errorf("component scores = %+v", sc)
	}
	n := s.SleepNeed
	if n == nil || n.BaselineMins != 420 || n.ActualMins != 480 || n.Feedback != "HIGHLY_INCREASED" || n.HRVAdjustment != "NO_CHANGE" ||
		n.NapAdjustment != "DECREASING" || n.SleepHistoryAdjustment != "INCREASING" || !n.DisplayedForTheDay ||
		n.RecommendedBedtimeStartMins != 1360 || n.RecommendedBedtimeEndMins != 0 {
		t.Errorf("sleepNeed = %+v", n)
	}
	if want := time.Date(2026, 11, 15, 18, 40, 12, 0, time.UTC); !n.Timestamp.Equal(want) {
		t.Errorf("need timestamp = %v", n.Timestamp.Time)
	}
	if s.NextSleepNeed == nil || s.NextSleepNeed.ActualMins != 450 {
		t.Errorf("nextSleepNeed = %+v", s.NextSleepNeed)
	}
	a := s.Alignment
	if a == nil || a.Status != "BEHIND" || a.OptimalWindowStartMins != -20 || a.OptimalWindowEndMins != 400 || a.OptimalMidpointMins != 190 || a.LastSleepMidpointMins != 286 || a.NeedBaselineMins != 420 {
		t.Errorf("alignment = %+v", a)
	}
	if len(s.Naps) != 1 || s.Naps[0].Seconds != 1200 || s.Naps[0].End.Sub(s.Naps[0].Start.Time) != 20*time.Minute || s.Naps[0].Feedback != "IDEAL_TIMING_LONG_DURATION" {
		t.Errorf("naps = %+v", s.Naps)
	}
}

func TestSleep_AbsentBlocksAreNil(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"id":1,"calendarDate":"2026-11-16","sleepNeed":null,"nextSleepNeed":null,"sleepAlignment":null,"dailyNapDTOS":null}`)
	s, err := c.Sleep(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if s.SleepNeed != nil || s.NextSleepNeed != nil || s.Alignment != nil || len(s.Naps) != 0 {
		t.Errorf("absent blocks must be nil: %+v", s)
	}
	c2, _ := serve(t, `{"id":1}`)
	if s, err := c2.Sleep(context.Background(), d2); err != nil || s.SleepNeed != nil || s.Alignment != nil {
		t.Errorf("missing keys: %+v err %v", s, err)
	}
}

func TestSleepRange_SharesTheFullModel(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, "["+sleepSchemaJSON+"]")
	got, err := c.SleepRange(context.Background(), d1, d2)
	if err != nil || len(got) != 1 || got[0].SleepNeed == nil || got[0].Alignment == nil || got[0].Scores.TotalDuration.Qualifier != "EXCELLENT" {
		t.Errorf("range = %+v err %v", got, err)
	}
}

// --- Long ranges are split to fit Garmin's per-endpoint caps -----------------

func TestSleepRange_SplitsAt31Days(t *testing.T) {
	t.Parallel()
	rec := &windowRecorder{}
	c := newTestClient(t, rec.handler("startDate", "endDate", func(s string) string {
		return `[{"id":1,"calendarDate":"` + s + `"}]`
	}))
	got, err := c.SleepRange(context.Background(), day("2026-09-01"), day("2026-11-09")) // 70 days
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-09-01..2026-10-01 2026-10-02..2026-11-01 2026-11-02..2026-11-09"
	if rec.summary() != want {
		t.Errorf("requests = %s\nwant       %s", rec.summary(), want)
	}
	if len(got) != 3 || got[0].CalendarDate != "2026-09-01" || got[1].CalendarDate != "2026-10-02" || got[2].CalendarDate != "2026-11-02" {
		t.Errorf("results not joined in order: %+v", got)
	}
}

// A one-day SleepRange is how to get a day's individual naps; this pins the
// documented workaround.
func TestSleepRange_OneDayRangeReturnsNaps(t *testing.T) {
	t.Parallel()
	c, last := serve(t, "["+napOnlyDayJSON+"]")
	got, err := c.SleepRange(context.Background(), day("2026-06-11"), day("2026-06-11"))
	if err != nil || len(got) != 1 || len(got[0].Naps) != 1 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if q := last.URL.Query(); q.Get("startDate") != "2026-06-11" || q.Get("endDate") != "2026-06-11" {
		t.Errorf("query = %v", q)
	}
}

func TestSleep_NapOnlyDayIsKeptAndDated(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, napOnlyDayJSON)
	s, err := c.Sleep(context.Background(), day("2026-06-11"))
	if err != nil {
		t.Fatal(err)
	}
	if s.HasSleep() {
		t.Error("a nap-only day has no main sleep")
	}
	if s.CalendarDate != "2026-06-11" {
		t.Errorf("CalendarDate = %q, want the date from sleepNeed (not nextSleepNeed's 2026-06-12)", s.CalendarDate)
	}
	if s.NapSeconds != 2640 || len(s.Naps) != 1 {
		t.Fatalf("naps = %+v (NapSeconds %d)", s.Naps, s.NapSeconds)
	}
	n := s.Naps[0]
	if n.CalendarDate != "2026-06-11" || n.Seconds != 2640 || n.Feedback != "LONG_DURATION_LOW_NEED" {
		t.Errorf("nap = %+v", n)
	}
	if want := time.Date(2026, 6, 11, 10, 51, 8, 0, time.UTC); !n.Start.Equal(want) || n.End.Sub(n.Start.Time) != 44*time.Minute {
		t.Errorf("nap times = %v .. %v", n.Start.Time, n.End.Time)
	}
	if s.SleepNeed == nil || s.SleepNeed.ActualMins != 440 || s.NextSleepNeed == nil || s.NextSleepNeed.ActualMins != 420 {
		t.Errorf("need blocks lost: %+v / %+v", s.SleepNeed, s.NextSleepNeed)
	}
	if !strings.Contains(string(s.Raw()), "dailyNapDTOS") {
		t.Error("raw not retained")
	}
}

func TestSleep_DateFallbackOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, want string
	}{
		{"keeps its own date", `{"id":1,"calendarDate":"2026-03-03","sleepNeed":{"calendarDate":"2026-03-04"}}`, "2026-03-03"},
		{"falls back to sleepNeed", `{"id":0,"calendarDate":"","sleepNeed":{"calendarDate":"2026-03-04"}}`, "2026-03-04"},
		{"then to the first nap", `{"id":0,"dailyNapDTOS":[{"calendarDate":"2026-03-05"}]}`, "2026-03-05"},
		{"stays empty with nothing to go on", `{"id":0}`, ""},
	}
	for _, tc := range tests {
		c, _ := serve(t, tc.body)
		s, err := c.Sleep(context.Background(), d2)
		if err != nil || s.CalendarDate != tc.want {
			t.Errorf("%s: CalendarDate = %q (err %v), want %q", tc.name, s.CalendarDate, err, tc.want)
		}
	}
}

func TestSleep_HasSleep(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, sleepJSON)
	s, err := c.Sleep(context.Background(), d2)
	if err != nil || !s.HasSleep() {
		t.Errorf("a recorded night must report HasSleep: %+v err %v", s, err)
	}
	if (garminconnect.Sleep{}).HasSleep() {
		t.Error("the zero Sleep (no data / HTTP 204) must not claim sleep")
	}
}

func TestSleepRange_NapOnlyDaysSortInPlace(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"id":1,"calendarDate":"2026-06-10"},`+napOnlyDayJSON+`,{"id":3,"calendarDate":"2026-06-12"}]`)
	got, err := c.SleepRange(context.Background(), day("2026-06-10"), day("2026-06-12"))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	dates := []string{got[0].CalendarDate, got[1].CalendarDate, got[2].CalendarDate}
	if strings.Join(dates, ",") != "2026-06-10,2026-06-11,2026-06-12" {
		t.Errorf("dates = %v, want ascending with the nap-only day dated", dates)
	}
	if got[1].HasSleep() || !got[0].HasSleep() || !got[2].HasSleep() {
		t.Error("HasSleep wrong across the range")
	}
}

// --- Stress values are an open map in Garmin's schema -------------------------

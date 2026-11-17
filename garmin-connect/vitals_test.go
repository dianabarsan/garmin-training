package garminconnect_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

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
	if _, err := c.BodyBattery(context.Background(), d2, d1); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("err = %v", err)
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

func TestHRVRange(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `{"userProfilePk":1,"hrvSummaries":[
	  {"calendarDate":"2026-11-14","weeklyAvg":40,"lastNightAvg":43,"lastNight5MinHigh":69,"status":"BALANCED","feedbackPhrase":"HRV_BALANCED_2",
	   "createTimeStamp":"2026-11-14T06:41:09.123","baseline":{"lowUpper":36,"balancedLow":37,"balancedUpper":50,"markerValue":0.55}},
	  {"calendarDate":"2026-11-15","weeklyAvg":41,"lastNightAvg":39,"lastNight5MinHigh":99,"status":"UNBALANCED","createTimeStamp":"2026-11-15T06:30:00.0"}]}`)
	got, err := c.HRVRange(context.Background(), d1, d2)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if last.URL.Path != "/hrv-service/hrv/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last.URL.Path)
	}
	a, b := got[0], got[1]
	if a.CalendarDate != "2026-11-14" || a.LastNightAvg != 43 || a.WeeklyAvg != 40 || a.LastNight5MinHigh != 69 || a.Status != "BALANCED" {
		t.Errorf("first = %+v", a)
	}
	if a.Baseline == nil || a.Baseline.BalancedLow != 37 || a.Baseline.BalancedUpper != 50 || a.Baseline.MarkerValue != 0.55 {
		t.Errorf("baseline = %+v", a.Baseline)
	}
	if want := time.Date(2026, 11, 14, 6, 41, 9, 123000000, time.UTC); !a.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt.Time, want)
	}
	if b.Status != "UNBALANCED" || b.Baseline != nil || b.LastNightAvg != 39 {
		t.Errorf("second = %+v", b)
	}
	if r := string(a.Raw()); !strings.Contains(r, `"markerValue":0.55`) || strings.Contains(r, "2026-11-15") {
		t.Errorf("each summary must keep exactly its own raw object, got %s", r)
	}
	if _, err := c.HRVRange(context.Background(), d2, d1); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("err = %v", err)
	}
}

func TestHRV_StartEndTimestamps(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"startTimestampGMT":"2026-11-14T22:00:00.0","endTimestampGMT":"2026-11-15T06:00:00.0",
	  "hrvSummary":{"calendarDate":"2026-11-15","lastNightAvg":50,"baseline":{"markerValue":0.5}}}`)
	h, err := c.HRV(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if h.End.Sub(h.Start.Time) != 8*time.Hour || h.Summary == nil || h.Summary.Baseline.MarkerValue != 0.5 {
		t.Errorf("hrv = %+v", h)
	}
}

// --- VO2 max ----------------------------------------------------------------

func TestBodyBattery_SplitsAt31Days(t *testing.T) {
	t.Parallel()
	rec := &windowRecorder{}
	c := newTestClient(t, rec.handler("startDate", "endDate", func(s string) string { return `[{"date":"` + s + `"}]` }))
	got, err := c.BodyBattery(context.Background(), day("2026-09-01"), day("2026-11-01")) // 62 days
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-09-01..2026-10-01 2026-10-02..2026-11-01"; rec.summary() != want || len(got) != 2 {
		t.Errorf("requests = %s (n=%d), want %s", rec.summary(), len(got), want)
	}
}

func TestHeartRate_RealStringTimestampsAndNoAbnormalFields(t *testing.T) {
	t.Parallel()
	// The real response: plain strings for dates (the Swagger schema shows
	// {date, localDate} objects), and no abnormal-HR keys at all.
	c, last := serve(t, `{"userProfilePK":1,"calendarDate":"2026-11-15",
	  "startTimestampGMT":"2026-11-14T22:00:00.0","endTimestampGMT":"2026-11-15T22:00:00.0",
	  "startTimestampLocal":"2026-11-15T00:00:00.0","endTimestampLocal":"2026-11-16T00:00:00.0",
	  "maxHeartRate":170,"minHeartRate":44,"restingHeartRate":50,"lastSevenDaysAvgRestingHeartRate":51,
	  "heartRateValueDescriptors":[{"key":"timestamp","index":0},{"key":"heartrate","index":1}],
	  "heartRateValues":[[1789000000000,60]]}`)
	h, err := c.HeartRate(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/wellness-service/wellness/dailyHeartRate" || last.URL.Query().Get("date") != "2026-11-15" {
		t.Errorf("request = %s?%s", last.URL.Path, last.URL.RawQuery)
	}
	if h.CalendarDate != "2026-11-15" || h.End.Sub(h.Start.Time) != 24*time.Hour || len(h.Samples) != 1 {
		t.Errorf("day = %+v", h)
	}
	if h.AbnormalThreshold != 0 || h.LowAbnormalThreshold != 0 || len(h.Abnormal) != 0 {
		t.Errorf("absent abnormal fields must be zero/empty: %+v", h)
	}
}

func TestHeartRate_AbnormalFieldsFromSchema(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"calendarDate":"2026-11-15","abnormalHrThresholdValue":120,"lowAbnormalHrThresholdValue":40,
	  "heartRateValueDescriptors":[{"key":"timestamp","index":0},{"key":"heartrate","index":1}],
	  "heartRateValues":[[1789000000000,60]],
	  "abnormalHRValuesArray":[[1789000060000,125],[1789000120000,null],[1789000180000,38]]}`)
	h, err := c.HeartRate(context.Background(), d2)
	if err != nil {
		t.Fatal(err)
	}
	if h.AbnormalThreshold != 120 || h.LowAbnormalThreshold != 40 {
		t.Errorf("thresholds = %d / %d", h.AbnormalThreshold, h.LowAbnormalThreshold)
	}
	if len(h.Abnormal) != 2 || h.Abnormal[0].BPM != 125 || h.Abnormal[1].BPM != 38 || h.Abnormal[0].Time.UnixMilli() != 1789000060000 {
		t.Errorf("abnormal = %+v (null row must be skipped)", h.Abnormal)
	}
	if len(h.Samples) != 1 || h.Samples[0].BPM != 60 {
		t.Errorf("normal samples disturbed: %+v", h.Samples)
	}
}

// The abnormal array's real shape is unverified, so no shape of it may be
// able to break decoding of the rest of the day.
func TestHeartRate_UnexpectedAbnormalShapesDoNotBreakTheDay(t *testing.T) {
	t.Parallel()
	for name, abnormal := range map[string]string{
		"null":             `null`,
		"empty":            `[]`,
		"objects not rows": `[{"time":1,"bpm":2}]`,
		"rows of objects":  `[[{"a":1}]]`,
		"a string":         `"none"`,
		"a number":         `7`,
		"short rows":       `[[1789000000000]]`,
		"text cells":       `[["x","y"]]`,
	} {
		c, _ := serve(t, `{"calendarDate":"2026-11-15","restingHeartRate":50,
		  "heartRateValues":[[1789000000000,61]],"abnormalHRValuesArray":`+abnormal+`}`)
		h, err := c.HeartRate(context.Background(), d2)
		if err != nil {
			t.Errorf("%s: decode failed: %v", name, err)
			continue
		}
		if h.Resting != 50 || len(h.Samples) != 1 || h.Samples[0].BPM != 61 {
			t.Errorf("%s: rest of the day damaged: %+v", name, h)
		}
		if len(h.Abnormal) != 0 {
			t.Errorf("%s: expected no abnormal samples, got %+v", name, h.Abnormal)
		}
	}
}

func TestHeartRate_ErrorsMatchTheDocumentedStatuses(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]error{
		http.StatusUnauthorized: garminconnect.ErrUnauthorized,
		http.StatusForbidden:    garminconnect.ErrForbidden,
	} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		if _, err := c.HeartRate(context.Background(), d2); !errors.Is(err, want) {
			t.Errorf("HTTP %d: err = %v, want %v", status, err, want)
		}
	}
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls++; calls == 1 {
			w.WriteHeader(http.StatusInternalServerError) // documented 500, transient
			return
		}
		_, _ = w.Write([]byte(`{"restingHeartRate":50}`))
	}))
	if h, err := c.HeartRate(context.Background(), d2); err != nil || h.Resting != 50 || calls != 2 {
		t.Errorf("500 should be retried once: %+v err %v calls %d", h, err, calls)
	}
	c400 := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	var apiErr *garminconnect.APIError
	if _, err := c400.HeartRate(context.Background(), d2); !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Errorf("400 err = %v", err)
	}
}

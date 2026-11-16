package garmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// HeartRateSample is one heart-rate reading.
type HeartRateSample struct {
	Time Timestamp
	BPM  int
}

// HeartRateDay is a day of heart-rate data. Samples omit readings Garmin
// reports as null.
type HeartRateDay struct {
	rawJSON
	CalendarDate            string            `json:"calendarDate"`
	Start                   Timestamp         `json:"startTimestampGMT"`
	End                     Timestamp         `json:"endTimestampGMT"`
	Resting                 int               `json:"restingHeartRate"`
	Min                     int               `json:"minHeartRate"`
	Max                     int               `json:"maxHeartRate"`
	LastSevenDaysAvgResting int               `json:"lastSevenDaysAvgRestingHeartRate"`
	Samples                 []HeartRateSample `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (h *HeartRateDay) UnmarshalJSON(b []byte) error {
	type plain HeartRateDay
	aux := struct {
		*plain
		Descriptors []valueDescriptor   `json:"heartRateValueDescriptors"`
		Values      [][]json.RawMessage `json:"heartRateValues"`
	}{plain: (*plain)(h)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	tsIdx := descriptorIndex(aux.Descriptors, "timestamp", 0)
	hrIdx := descriptorIndex(aux.Descriptors, "heartrate", 1)
	for _, row := range aux.Values {
		ts, ok1 := numberAt(row, tsIdx)
		hr, ok2 := numberAt(row, hrIdx)
		if !ok1 || !ok2 {
			continue
		}
		h.Samples = append(h.Samples, HeartRateSample{
			Time: Timestamp{time.UnixMilli(int64(ts)).UTC()},
			BPM:  int(hr),
		})
	}
	return nil
}

// HeartRate returns heart-rate data for the given day.
func (c *Client) HeartRate(ctx context.Context, date time.Time) (HeartRateDay, error) {
	var raw json.RawMessage
	q := url.Values{"date": {dateParam(date)}}
	if err := c.get(ctx, "/wellness-service/wellness/dailyHeartRate", q, &raw); err != nil {
		return HeartRateDay{}, err
	}
	h, err := decodeOne[HeartRateDay](raw)
	if err != nil {
		return HeartRateDay{}, fmt.Errorf("garmin: decoding heart rate: %w", err)
	}
	return h, nil
}

// HRVBaseline is the user's personal HRV range in milliseconds.
type HRVBaseline struct {
	LowUpper      int     `json:"lowUpper"`
	BalancedLow   int     `json:"balancedLow"`
	BalancedUpper int     `json:"balancedUpper"`
	MarkerValue   float64 `json:"markerValue"`
}

// HRVSummary is Garmin's overnight heart-rate-variability rollup, in
// milliseconds.
type HRVSummary struct {
	CalendarDate      string       `json:"calendarDate"`
	WeeklyAvg         int          `json:"weeklyAvg"`
	LastNightAvg      int          `json:"lastNightAvg"`
	LastNight5MinHigh int          `json:"lastNight5MinHigh"`
	Status            string       `json:"status"` // e.g. "BALANCED"
	FeedbackPhrase    string       `json:"feedbackPhrase"`
	Baseline          *HRVBaseline `json:"baseline"`
	// CreatedAt is when Garmin computed the summary.
	CreatedAt Timestamp `json:"createTimeStamp"`
}

// HRVReading is one overnight HRV measurement in milliseconds.
type HRVReading struct {
	Time  Timestamp `json:"readingTimeGMT"`
	Value int       `json:"hrvValue"`
}

// HRV is a night's heart-rate-variability data. Summary is nil when Garmin
// has no HRV for the day.
type HRV struct {
	rawJSON
	Summary    *HRVSummary  `json:"hrvSummary"`
	Readings   []HRVReading `json:"hrvReadings"`
	Start      Timestamp    `json:"startTimestampGMT"`
	End        Timestamp    `json:"endTimestampGMT"`
	SleepStart Timestamp    `json:"sleepStartTimestampGMT"`
	SleepEnd   Timestamp    `json:"sleepEndTimestampGMT"`
}

// HRV returns heart-rate-variability data for the night ending on date. When
// Garmin has no data for the date (HTTP 204) it returns the zero HRV, whose
// Summary is nil.
func (c *Client) HRV(ctx context.Context, date time.Time) (HRV, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/hrv-service/hrv/"+dateParam(date), nil, &raw); err != nil {
		return HRV{}, err
	}
	h, err := decodeOne[HRV](raw)
	if err != nil {
		return HRV{}, fmt.Errorf("garmin: decoding HRV: %w", err)
	}
	return h, nil
}

// BodyBatterySample is one 0-100 Body Battery reading.
type BodyBatterySample struct {
	Time  Timestamp
	Level int
}

// BodyBatteryDay is a day of Body Battery data.
type BodyBatteryDay struct {
	rawJSON
	Date    string              `json:"date"`
	Start   Timestamp           `json:"startTimestampGMT"`
	End     Timestamp           `json:"endTimestampGMT"`
	Charged int                 `json:"charged"`
	Drained int                 `json:"drained"`
	Samples []BodyBatterySample `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *BodyBatteryDay) UnmarshalJSON(b []byte) error {
	type plain BodyBatteryDay
	aux := struct {
		*plain
		Descriptors []struct {
			Index int    `json:"bodyBatteryValueDescriptorIndex"`
			Key   string `json:"bodyBatteryValueDescriptorKey"`
		} `json:"bodyBatteryValueDescriptorDTOList"`
		Values [][]json.RawMessage `json:"bodyBatteryValuesArray"`
	}{plain: (*plain)(d)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	descs := make([]valueDescriptor, len(aux.Descriptors))
	for i, x := range aux.Descriptors {
		descs[i] = valueDescriptor{Index: x.Index, Key: x.Key}
	}
	tsIdx := descriptorIndex(descs, "timestamp", 0)
	lvlIdx := descriptorIndex(descs, "bodybatterylevel", 2)
	for _, row := range aux.Values {
		ts, ok1 := numberAt(row, tsIdx)
		lvl, ok2 := numberAt(row, lvlIdx)
		if !ok1 || !ok2 {
			continue
		}
		d.Samples = append(d.Samples, BodyBatterySample{
			Time:  Timestamp{time.UnixMilli(int64(ts)).UTC()},
			Level: int(lvl),
		})
	}
	return nil
}

// BodyBattery returns Body Battery data for each day in [start, end].
func (c *Client) BodyBattery(ctx context.Context, start, end time.Time) ([]BodyBatteryDay, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	q := url.Values{"startDate": {dateParam(start)}, "endDate": {dateParam(end)}}
	if err := c.get(ctx, "/wellness-service/wellness/bodyBattery/reports/daily", q, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[BodyBatteryDay](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding body battery: %w", err)
	}
	return out, nil
}

// valueDescriptor names a column in Garmin's array-of-arrays time series.
type valueDescriptor struct {
	Index int    `json:"index"`
	Key   string `json:"key"`
}

// descriptorIndex finds the column named key (case-insensitive), falling back
// to def when the response carries no descriptors.
func descriptorIndex(descs []valueDescriptor, key string, def int) int {
	for _, d := range descs {
		if strings.EqualFold(d.Key, key) {
			return d.Index
		}
	}
	return def
}

// numberAt returns row[i] as a number; ok is false for a missing, null or
// non-numeric cell (series rows mix numbers, strings and nulls).
func numberAt(row []json.RawMessage, i int) (float64, bool) {
	if i < 0 || i >= len(row) {
		return 0, false
	}
	var f *float64
	if err := json.Unmarshal(row[i], &f); err != nil || f == nil {
		return 0, false
	}
	return *f, true
}

// HRVRange returns the nightly HRV summary for each day in [start, end], oldest
// first. Days Garmin has no HRV for are absent, and a range with no data at all
// (HTTP 204) returns an empty slice. It carries the summaries only; use HRV for
// a night's individual readings.
func (c *Client) HRVRange(ctx context.Context, start, end time.Time) ([]HRVSummary, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var env struct {
		Summaries []HRVSummary `json:"hrvSummaries"`
	}
	path := "/hrv-service/hrv/daily/" + dateParam(start) + "/" + dateParam(end)
	if err := c.get(ctx, path, nil, &env); err != nil {
		return nil, err
	}
	return env.Summaries, nil
}

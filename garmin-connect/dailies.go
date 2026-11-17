package garminconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// DailySummary is one day's wellness rollup. Nested groups are nil when
// Garmin reports nothing for them (for example no watch worn that day).
type DailySummary struct {
	rawJSON
	// CalendarDate is the day as YYYY-MM-DD in the user's local time.
	CalendarDate string `json:"calendarDate"`

	Steps            *Movement         `json:"-"`
	FloorsAscended   *Movement         `json:"-"`
	Calories         *DailyCalories    `json:"calories"`
	HeartRate        *DailyHeartRate   `json:"heartRate"`
	Stress           *DailyStress      `json:"stress"`
	BodyBattery      *DailyBodyBattery `json:"bodyBattery"`
	IntensityMinutes *IntensityMinutes `json:"intensityMinutes"`
	Hydration        *DailyHydration   `json:"hydration"`
	PulseOx          *DailyPulseOx     `json:"pulseOx"`
	Respiration      *DailyRespiration `json:"respiration"`
}

// Movement is a counter with an optional daily goal.
type Movement struct {
	Value          int     `json:"value"`
	Goal           int     `json:"goal"`
	DistanceMeters float64 `json:"distanceInMeters"`
}

type dailyMovementJSON struct {
	Steps          *Movement `json:"steps"`
	FloorsAscended *Movement `json:"floorsAscended"`
}

// DailyCalories are kilocalories.
type DailyCalories struct {
	Active  int `json:"burnedActive"`
	Resting int `json:"burnedResting"`
	Total   int `json:"burnedTotal"`
}

// DailyHeartRate is in beats per minute.
type DailyHeartRate struct {
	Min     int `json:"minValue"`
	Max     int `json:"maxValue"`
	Resting int `json:"restingValue"`
}

// DailyStress holds Garmin's 0-100 stress levels and the milliseconds spent
// in each band.
type DailyStress struct {
	AvgLevel            int    `json:"avgLevel"`
	MaxLevel            int    `json:"maxLevel"`
	Qualifier           string `json:"qualifier"`
	RestMillis          int64  `json:"restDurationInMillis"`
	LowMillis           int64  `json:"lowStressDurationInMillis"`
	MediumMillis        int64  `json:"mediumStressDurationInMillis"`
	HighMillis          int64  `json:"highStressDurationInMillis"`
	ActivityMillis      int64  `json:"activityDurationInMillis"`
	UncategorizedMillis int64  `json:"uncategorizedDurationInMillis"`
}

// DailyBodyBattery is Garmin's 0-100 energy score.
type DailyBodyBattery struct {
	Charged int `json:"chargedValue"`
	Drained int `json:"drainedValue"`
	Latest  int `json:"latestValue"`
	Min     int `json:"minValue"`
	Max     int `json:"maxValue"`
}

// IntensityMinutes counts weekly-goal exercise minutes.
type IntensityMinutes struct {
	Goal     int `json:"goal"`
	Moderate int `json:"moderate"`
	Vigorous int `json:"vigorous"`
}

// DailyHydration is in millilitres.
type DailyHydration struct {
	ConsumedMl int `json:"consumedInMl"`
	GoalMl     int `json:"goalInMl"`
}

// DailyPulseOx is blood-oxygen saturation in percent.
type DailyPulseOx struct {
	Avg    int `json:"avgValue"`
	Min    int `json:"minValue"`
	Latest int `json:"latestValue"`
}

// DailyRespiration is in breaths per minute.
type DailyRespiration struct {
	Avg    float64 `json:"avgValue"`
	Min    float64 `json:"minValue"`
	Max    float64 `json:"maxValue"`
	Latest float64 `json:"latestValue"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *DailySummary) UnmarshalJSON(b []byte) error {
	type plain DailySummary
	aux := struct {
		*plain
		Movement *dailyMovementJSON `json:"movement"`
	}{plain: (*plain)(d)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if m := aux.Movement; m != nil {
		d.Steps, d.FloorsAscended = m.Steps, m.FloorsAscended
	}
	return nil
}

// DailySummaries returns one summary per day in [start, end], inclusive.
// Days are interpreted by their calendar date in the location of the times
// you pass.
// Ranges longer than 300 days are fetched in several requests and joined.
func (c *Client) DailySummaries(ctx context.Context, start, end time.Time) ([]DailySummary, error) {
	return fetchRange(ctx, start, end, maxDailySummaryDays, c.dailySummaries)
}

// dailySummaries fetches a single window that fits within Garmin's range cap.
func (c *Client) dailySummaries(ctx context.Context, start, end time.Time) ([]DailySummary, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	q := url.Values{"startDate": {dateParam(start)}, "endDate": {dateParam(end)}}
	if err := c.get(ctx, "/usersummary-service/v2/dailysummaries", q, &env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return nil, nil
	}
	out, err := decodeList[DailySummary](env.Data)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding daily summaries: %w", err)
	}
	return out, nil
}

// StepsDay is one day of the step statistics endpoint.
type StepsDay struct {
	rawJSON
	CalendarDate  string  `json:"calendarDate"`
	TotalSteps    int     `json:"totalSteps"`
	StepGoal      int     `json:"stepGoal"`
	TotalDistance float64 `json:"totalDistance"` // metres
}

// Steps returns daily step totals in [start, end].
// Ranges longer than 28 days are fetched in several requests and joined.
func (c *Client) Steps(ctx context.Context, start, end time.Time) ([]StepsDay, error) {
	return fetchRange(ctx, start, end, maxStepsDays, c.steps)
}

// steps fetches a single window that fits within Garmin's range cap.
func (c *Client) steps(ctx context.Context, start, end time.Time) ([]StepsDay, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := "/usersummary-service/stats/steps/daily/" + dateParam(start) + "/" + dateParam(end)
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[StepsDay](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding steps: %w", err)
	}
	return out, nil
}

// StressDay is one day of the stress statistics endpoint.
type StressDay struct {
	rawJSON
	CalendarDate string       `json:"calendarDate"`
	Values       StressValues `json:"values"`
}

// StressValues are a day's stress figures. Durations are seconds (verified
// against the daily summary's millisecond figures) and OverallLevel is Garmin's
// 0-100 score. Garmin documents the underlying object as an open map, so any
// numeric key beyond the five known ones is kept in Other instead of dropped.
type StressValues struct {
	OverallLevel  int `json:"overallStressLevel"`
	RestSeconds   int `json:"restStressDuration"`
	LowSeconds    int `json:"lowStressDuration"`
	MediumSeconds int `json:"mediumStressDuration"`
	HighSeconds   int `json:"highStressDuration"`
	// Other holds additional numeric keys by their Garmin name; nil when none.
	Other map[string]float64 `json:"other,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler. Non-numeric values are ignored.
func (v *StressValues) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*v = StressValues{}
	for k, raw := range m {
		var f float64
		// Unmarshalling null into a float succeeds and yields 0, so check for it
		// explicitly or a null would be recorded as a real zero.
		if string(raw) == "null" || json.Unmarshal(raw, &f) != nil {
			continue
		}
		switch k {
		case "overallStressLevel":
			v.OverallLevel = int(f)
		case "restStressDuration":
			v.RestSeconds = int(f)
		case "lowStressDuration":
			v.LowSeconds = int(f)
		case "mediumStressDuration":
			v.MediumSeconds = int(f)
		case "highStressDuration":
			v.HighSeconds = int(f)
		default:
			if v.Other == nil {
				v.Other = map[string]float64{}
			}
			v.Other[k] = f
		}
	}
	return nil
}

// Stress returns daily stress statistics in [start, end].
// Ranges longer than 28 days are fetched in several requests and joined.
func (c *Client) Stress(ctx context.Context, start, end time.Time) ([]StressDay, error) {
	return fetchRange(ctx, start, end, maxStressDays, c.stress)
}

// stress fetches a single window that fits within Garmin's range cap.
func (c *Client) stress(ctx context.Context, start, end time.Time) ([]StressDay, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	path := "/usersummary-service/stats/stress/daily/" + dateParam(start) + "/" + dateParam(end)
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[StressDay](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding stress: %w", err)
	}
	return out, nil
}

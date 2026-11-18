package garminconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// Sleep is one night's sleep summary. Durations are seconds. Scores and
// insights are Garmin's own and may be empty when the night was not scored.
type Sleep struct {
	rawJSON
	ID           int64     `json:"id"`
	CalendarDate string    `json:"calendarDate"`
	Start        Timestamp `json:"sleepStartTimestampGMT"`
	End          Timestamp `json:"sleepEndTimestampGMT"`
	// WindowConfirmed reports whether Garmin confirmed the sleep window, and
	// WindowConfirmationType how, e.g. "enhanced_confirmed_final".
	WindowConfirmed        bool   `json:"sleepWindowConfirmed"`
	WindowConfirmationType string `json:"sleepWindowConfirmationType"`

	TotalSeconds        int `json:"sleepTimeSeconds"`
	DeepSeconds         int `json:"deepSleepSeconds"`
	LightSeconds        int `json:"lightSleepSeconds"`
	REMSeconds          int `json:"remSleepSeconds"`
	AwakeSeconds        int `json:"awakeSleepSeconds"`
	NapSeconds          int `json:"napTimeSeconds"`
	UnmeasurableSeconds int `json:"unmeasurableSleepSeconds"`
	AwakeCount          int `json:"awakeCount"`

	AvgHeartRate   float64 `json:"avgHeartRate"`
	AvgSleepStress float64 `json:"avgSleepStress"`
	AvgRespiration float64 `json:"averageRespirationValue"`
	LowestResp     float64 `json:"lowestRespirationValue"`
	HighestResp    float64 `json:"highestRespirationValue"`
	AvgSpO2        float64 `json:"averageSpO2Value"`
	LowestSpO2     int     `json:"lowestSpO2Value"`
	HighestSpO2    int     `json:"highestSpO2Value"`
	// AvgSpO2SleepHeartRate is the average heart rate during the SpO2 readings.
	AvgSpO2SleepHeartRate float64 `json:"averageSpO2HRSleep"`
	// SpO2EventsBelowThreshold counts dips under SpO2Threshold (percent); both
	// are zero when the watch did not measure them.
	SpO2EventsBelowThreshold int `json:"numberOfEventsBelowThreshold"`
	SpO2Threshold            int `json:"spo2Threshold"`

	Scores                      SleepScores `json:"sleepScores"`
	ScoreFeedback               string      `json:"sleepScoreFeedback"`
	ScoreInsight                string      `json:"sleepScoreInsight"`
	BreathingDisruptionSeverity string      `json:"breathingDisruptionSeverity"`

	// SleepNeed is the need for this night and NextSleepNeed the recommendation
	// for the next one. Both are nil when Garmin reports none.
	SleepNeed     *SleepNeed `json:"sleepNeed"`
	NextSleepNeed *SleepNeed `json:"nextSleepNeed"`
	// Alignment compares your sleep timing with Garmin's recommended window.
	Alignment *SleepAlignment `json:"sleepAlignment"`
	// Naps lists individual naps. Only SleepRange fills it: the single-day Sleep
	// call returns the NapSeconds total but not the list (checked on a day with
	// a 44 minute nap), so use SleepRange with a one-day range to get naps.
	// AgeGroup is e.g. "ADULT".
	Naps     []SleepNap `json:"dailyNapDTOS"`
	AgeGroup string     `json:"ageGroup"`
	// UserNote is the free-text note you attached to the night, if any.
	UserNote     string `json:"userNote"`
	SleepVersion int    `json:"sleepVersion"`
}

// UnmarshalJSON implements json.Unmarshaler. Garmin returns a record for days
// that have naps or sleep-need guidance but no main sleep, with id 0 and an
// empty calendarDate. Those carry real data, so they are kept, and the date is
// taken from the nested sleep-need block (or the first nap) so entries still
// sort and join by day.
func (s *Sleep) UnmarshalJSON(b []byte) error {
	type plain Sleep // drops methods, avoiding recursion
	if err := json.Unmarshal(b, (*plain)(s)); err != nil {
		return err
	}
	if s.CalendarDate == "" {
		switch {
		case s.SleepNeed != nil && s.SleepNeed.CalendarDate != "":
			s.CalendarDate = s.SleepNeed.CalendarDate
		case len(s.Naps) > 0 && s.Naps[0].CalendarDate != "":
			s.CalendarDate = s.Naps[0].CalendarDate
		}
	}
	return nil
}

// HasSleep reports whether Garmin recorded a main sleep. It is false for days
// that only carry naps or sleep-need guidance; check NapSeconds and Naps there.
func (s Sleep) HasSleep() bool { return s.ID != 0 || !s.Start.IsZero() }

// SleepScore is one component of the sleep score: a 0-100 value where Garmin
// scores one, its qualifier (e.g. "GOOD"), and the optimal range for it.
type SleepScore struct {
	Value             int     `json:"value"`
	Qualifier         string  `json:"qualifierKey"`
	OptimalStart      float64 `json:"optimalStart"`
	OptimalEnd        float64 `json:"optimalEnd"`
	IdealStartSeconds float64 `json:"idealStartInSeconds"`
	IdealEndSeconds   float64 `json:"idealEndInSeconds"`
}

// SleepScores holds the overall score and every component Garmin reports.
type SleepScores struct {
	Overall       SleepScore `json:"overall"`
	Deep          SleepScore `json:"deepPercentage"`
	Light         SleepScore `json:"lightPercentage"`
	REM           SleepScore `json:"remPercentage"`
	AwakeCount    SleepScore `json:"awakeCount"`
	Restlessness  SleepScore `json:"restlessness"`
	Stress        SleepScore `json:"stress"`
	TotalDuration SleepScore `json:"totalDuration"`
}

// SleepNeed is Garmin's sleep-need estimate. Baseline and Actual are minutes;
// the recommended bedtime window is minutes after midnight (1360 is 22:40).
type SleepNeed struct {
	CalendarDate                string    `json:"calendarDate"`
	Time                        Timestamp `json:"timestampGmt"`
	BaselineMins                int       `json:"baseline"`
	ActualMins                  int       `json:"actual"`
	Feedback                    string    `json:"feedback"`
	TrainingFeedback            string    `json:"trainingFeedback"`
	SleepHistoryAdjustment      string    `json:"sleepHistoryAdjustment"`
	HRVAdjustment               string    `json:"hrvAdjustment"`
	NapAdjustment               string    `json:"napAdjustment"`
	DisplayedForTheDay          bool      `json:"displayedForTheDay"`
	RecommendedBedtimeStartMins int       `json:"recommendedBedtimeStartMins"`
	RecommendedBedtimeEndMins   int       `json:"recommendedBedtimeEndMins"`
}

// SleepAlignment relates when you slept to Garmin's recommended sleep window.
// All figures are minutes relative to midnight, so a negative window start is
// before midnight (-20 is 23:40). Status is e.g. "BEHIND".
type SleepAlignment struct {
	CalendarDate           string `json:"calendarDate"`
	Status                 string `json:"status"`
	NeedBaselineMins       int    `json:"sleepNeedBaselineMins"`
	OptimalWindowStartMins int    `json:"optimalSleepWindowStartMins"`
	OptimalWindowEndMins   int    `json:"optimalSleepWindowEndMins"`
	OptimalMidpointMins    int    `json:"optimalSleepWindowMidpointMins"`
	LastSleepMidpointMins  int    `json:"lastSleepMidpointMins"`
}

// SleepNap is one nap.
type SleepNap struct {
	CalendarDate string    `json:"calendarDate"`
	Start        Timestamp `json:"napStartTimestampGMT"`
	End          Timestamp `json:"napEndTimestampGMT"`
	Seconds      int       `json:"napTimeSec"`
	Feedback     string    `json:"napFeedback"`
}

// Sleep returns the sleep summary for the night ending on date. When Garmin
// has no sleep for the date it returns the zero Sleep. It does not include the
// list of naps, only their NapSeconds total; SleepRange does.
func (c *Client) Sleep(ctx context.Context, date time.Time) (Sleep, error) {
	var raw json.RawMessage
	q := url.Values{"date": {dateParam(date)}}
	if err := c.get(ctx, "/sleep-service/sleep/dailySleep", q, &raw); err != nil {
		return Sleep{}, err
	}
	s, err := decodeOne[Sleep](raw)
	if err != nil {
		return Sleep{}, fmt.Errorf("garmin: decoding sleep: %w", err)
	}
	return s, nil
}

// SleepRange returns sleep summaries for each day in [start, end]. Days with
// naps or sleep-need guidance but no main sleep are included (see
// Sleep.HasSleep); days with nothing at all are absent.
// Ranges longer than 31 days are fetched in several requests and joined;
// if one fails, the days fetched before it are returned with the error.
func (c *Client) SleepRange(ctx context.Context, start, end time.Time) ([]Sleep, error) {
	return fetchRange(ctx, start, end, maxSleepRangeDays, c.sleepRange)
}

// sleepRange fetches a single window that fits within Garmin's range cap.
func (c *Client) sleepRange(ctx context.Context, start, end time.Time) ([]Sleep, error) {
	var raw json.RawMessage
	q := url.Values{"startDate": {dateParam(start)}, "endDate": {dateParam(end)}}
	if err := c.get(ctx, "/sleep-service/sleep/dailySleepSummariesByDate", q, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[Sleep](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding sleep summaries: %w", err)
	}
	return out, nil
}

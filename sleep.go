package garmin

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
	// Naps lists individual naps. Garmin's schema documents it but it was empty
	// on every night checked, even ones with NapSeconds > 0, so treat it as
	// unverified. AgeGroup is e.g. "ADULT".
	Naps     []SleepNap `json:"dailyNapDTOS"`
	AgeGroup string     `json:"ageGroup"`
	// UserNote is the free-text note you attached to the night, if any.
	UserNote     string `json:"userNote"`
	SleepVersion int    `json:"sleepVersion"`
}

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
	Timestamp                   Timestamp `json:"timestampGmt"`
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

// SleepNap is one nap, as documented by Garmin's schema.
type SleepNap struct {
	Start    Timestamp `json:"napStartTimestampGMT"`
	End      Timestamp `json:"napEndTimestampGMT"`
	Seconds  int       `json:"napTimeSec"`
	Feedback string    `json:"napFeedback"`
}

// Sleep returns the sleep summary for the night ending on date. When Garmin
// has no sleep for the date it returns the zero Sleep.
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

// SleepRange returns sleep summaries for each night in [start, end].
func (c *Client) SleepRange(ctx context.Context, start, end time.Time) ([]Sleep, error) {
	if err := checkRange(start, end); err != nil {
		return nil, err
	}
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

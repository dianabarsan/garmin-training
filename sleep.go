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

	Scores                      SleepScores `json:"sleepScores"`
	ScoreFeedback               string      `json:"sleepScoreFeedback"`
	ScoreInsight                string      `json:"sleepScoreInsight"`
	BreathingDisruptionSeverity string      `json:"breathingDisruptionSeverity"`
}

// SleepScore is a 0-100 score with Garmin's qualifier, e.g. "GOOD".
type SleepScore struct {
	Value     int    `json:"value"`
	Qualifier string `json:"qualifierKey"`
}

// SleepScores holds the overall score and the stage-percentage scores.
type SleepScores struct {
	Overall SleepScore `json:"overall"`
	Deep    SleepScore `json:"deepPercentage"`
	Light   SleepScore `json:"lightPercentage"`
	REM     SleepScore `json:"remPercentage"`
}

// Sleep returns the sleep summary for the night ending on date.
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

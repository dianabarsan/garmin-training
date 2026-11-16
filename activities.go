package garmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/url"
	"strconv"
	"time"
)

// ActivityType identifies the kind of activity, e.g. key "running".
type ActivityType struct {
	ID       int    `json:"typeId"`
	Key      string `json:"typeKey"`
	ParentID int    `json:"parentTypeId"`
}

// ActivityMetrics are the measurements shared by activity list entries and
// activity detail. Distances are metres, durations seconds, speeds metres per
// second. Fields Garmin does not report for an activity are zero.
type ActivityMetrics struct {
	StartTime             Timestamp `json:"startTimeGMT"`
	Distance              float64   `json:"distance"`
	Duration              float64   `json:"duration"`
	ElapsedDuration       float64   `json:"elapsedDuration"`
	MovingDuration        float64   `json:"movingDuration"`
	Calories              float64   `json:"calories"`
	BMRCalories           float64   `json:"bmrCalories"`
	AverageHR             float64   `json:"averageHR"`
	MaxHR                 float64   `json:"maxHR"`
	MinHR                 float64   `json:"minHR"` // detail only
	AverageSpeed          float64   `json:"averageSpeed"`
	AverageMovingSpeed    float64   `json:"averageMovingSpeed"` // detail only
	AvgElevation          float64   `json:"avgElevation"`
	Steps                 int       `json:"steps"`
	AerobicTrainingEffect float64   `json:"aerobicTrainingEffect"`
	// AnaerobicTrainingEffect and TrainingLoad are Garmin's own scores.
	AnaerobicTrainingEffect float64 `json:"anaerobicTrainingEffect"`
	TrainingLoad            float64 `json:"activityTrainingLoad"`
	TrainingEffectLabel     string  `json:"trainingEffectLabel"`
	BodyBatteryChange       int     `json:"differenceBodyBattery"`
}

// Activity is one entry from the activity list.
type Activity struct {
	rawJSON
	ID   int64        `json:"activityId"`
	Name string       `json:"activityName"`
	Type ActivityType `json:"activityType"`
	ActivityMetrics
	// HRTimeInZone is seconds spent in heart-rate zones 1 to 5.
	HRTimeInZone [5]float64 `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *Activity) UnmarshalJSON(b []byte) error {
	type plain Activity // drops methods, avoiding recursion
	aux := struct {
		*plain
		Z1 float64 `json:"hrTimeInZone_1"`
		Z2 float64 `json:"hrTimeInZone_2"`
		Z3 float64 `json:"hrTimeInZone_3"`
		Z4 float64 `json:"hrTimeInZone_4"`
		Z5 float64 `json:"hrTimeInZone_5"`
	}{plain: (*plain)(a)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	a.HRTimeInZone = [5]float64{aux.Z1, aux.Z2, aux.Z3, aux.Z4, aux.Z5}
	return nil
}

// ActivityDetail is the response for a single activity. Lap, split and
// time-series data is not modelled; use Raw or DownloadActivity.
type ActivityDetail struct {
	rawJSON
	ID       int64
	Name     string
	Type     ActivityType
	Summary  ActivityMetrics
	TimeZone string
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *ActivityDetail) UnmarshalJSON(b []byte) error {
	var aux struct {
		ID      int64        `json:"activityId"`
		Name    string       `json:"activityName"`
		Type    ActivityType `json:"activityTypeDTO"`
		Summary struct {
			ActivityMetrics
			TrainingEffect float64 `json:"trainingEffect"` // detail's name for the aerobic score
		} `json:"summaryDTO"`
		TimeZone struct {
			TimeZone string `json:"timeZone"`
		} `json:"timeZoneUnitDTO"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	d.ID, d.Name, d.Type = aux.ID, aux.Name, aux.Type
	d.Summary = aux.Summary.ActivityMetrics
	if d.Summary.AerobicTrainingEffect == 0 {
		d.Summary.AerobicTrainingEffect = aux.Summary.TrainingEffect
	}
	d.TimeZone = aux.TimeZone.TimeZone
	return nil
}

// ActivitiesOptions filters and pages the activity list. The zero value
// returns the 20 most recent activities.
type ActivitiesOptions struct {
	// Start is the offset into the result list (0 = newest).
	Start int
	// Limit is the page size. Defaults to 20.
	Limit int
	// StartDate and EndDate bound activities by date when non-zero.
	StartDate, EndDate time.Time
	// TypeKey filters by parent activity type key (see ActivityTypeKeys).
	TypeKey string
}

func (o ActivitiesOptions) query() (url.Values, error) {
	if o.Start < 0 || o.Limit < 0 {
		return nil, fmt.Errorf("garmin: negative start or limit")
	}
	limit := o.Limit
	if limit == 0 {
		limit = 20
	}
	q := url.Values{
		"start": {strconv.Itoa(o.Start)},
		"limit": {strconv.Itoa(limit)},
	}
	if !o.StartDate.IsZero() {
		q.Set("startDate", dateParam(o.StartDate))
	}
	if !o.EndDate.IsZero() {
		q.Set("endDate", dateParam(o.EndDate))
	}
	if !o.StartDate.IsZero() && !o.EndDate.IsZero() {
		if err := checkRange(o.StartDate, o.EndDate); err != nil {
			return nil, err
		}
	}
	if o.TypeKey != "" {
		q.Set("activityType", o.TypeKey)
	}
	return q, nil
}

// Activities returns one page of activities, newest first.
func (c *Client) Activities(ctx context.Context, opts ActivitiesOptions) ([]Activity, error) {
	q, err := opts.query()
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/activitylist-service/activities/search/activities", q, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[Activity](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding activities: %w", err)
	}
	return out, nil
}

// AllActivities iterates over every activity matching opts, fetching pages
// lazily and stopping at the first empty page. opts.Start sets where to begin;
// opts.Limit sets the page size.
//
//	for a, err := range c.AllActivities(ctx, garmin.ActivitiesOptions{}) {
//		if err != nil { return err }
//		...
//	}
func (c *Client) AllActivities(ctx context.Context, opts ActivitiesOptions) iter.Seq2[Activity, error] {
	return func(yield func(Activity, error) bool) {
		for {
			page, err := c.Activities(ctx, opts)
			if err != nil {
				yield(Activity{}, err)
				return
			}
			if len(page) == 0 {
				return
			}
			for _, a := range page {
				if !yield(a, nil) {
					return
				}
			}
			opts.Start += len(page)
		}
	}
}

// ActivityDetail returns a single activity's summary.
func (c *Client) ActivityDetail(ctx context.Context, id int64) (ActivityDetail, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/activity-service/activity/"+strconv.FormatInt(id, 10), nil, &raw); err != nil {
		return ActivityDetail{}, err
	}
	d, err := decodeOne[ActivityDetail](raw)
	if err != nil {
		return ActivityDetail{}, fmt.Errorf("garmin: decoding activity %d: %w", id, err)
	}
	return d, nil
}

// ActivityTypeKeys returns the activity type keys usable in
// ActivitiesOptions.TypeKey.
func (c *Client) ActivityTypeKeys(ctx context.Context) ([]string, error) {
	var items []struct {
		Key string `json:"key"`
	}
	if err := c.get(ctx, "/activity-service/activity/activityTypes/simple", nil, &items); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys, nil
}

// DownloadActivity streams the activity's original file to w. Garmin serves it
// as a ZIP archive containing the FIT file.
func (c *Client) DownloadActivity(ctx context.Context, id int64, w io.Writer) error {
	path := "/download-service/files/activity/" + strconv.FormatInt(id, 10)
	return c.stream(ctx, path, "application/zip, */*", w)
}

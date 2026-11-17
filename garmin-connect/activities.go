package garminconnect

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
	StartTime          Timestamp `json:"startTimeGMT"`
	Distance           float64   `json:"distance"`
	Duration           float64   `json:"duration"`
	ElapsedDuration    float64   `json:"elapsedDuration"`
	MovingDuration     float64   `json:"movingDuration"`
	Calories           float64   `json:"calories"`
	BMRCalories        float64   `json:"bmrCalories"`
	AverageHR          float64   `json:"averageHR"`
	MaxHR              float64   `json:"maxHR"`
	MinHR              float64   `json:"minHR"` // detail only
	AverageSpeed       float64   `json:"averageSpeed"`
	AverageMovingSpeed float64   `json:"averageMovingSpeed"` // detail only
	// Elevation figures are metres. Gain and loss are Garmin's own totals.
	// Activities without altitude data (indoor, strength, HIIT) report zero
	// for all of them; Garmin sends -500 for the average, which is mapped to 0.
	AvgElevation          float64 `json:"avgElevation"`
	MinElevation          float64 `json:"minElevation"`
	MaxElevation          float64 `json:"maxElevation"`
	ElevationGain         float64 `json:"elevationGain"`
	ElevationLoss         float64 `json:"elevationLoss"`
	Steps                 int     `json:"steps"`
	AerobicTrainingEffect float64 `json:"aerobicTrainingEffect"`
	// AnaerobicTrainingEffect and TrainingLoad are Garmin's own scores.
	AnaerobicTrainingEffect float64 `json:"anaerobicTrainingEffect"`
	TrainingLoad            float64 `json:"activityTrainingLoad"`
	TrainingEffectLabel     string  `json:"trainingEffectLabel"`
	BodyBatteryChange       int     `json:"differenceBodyBattery"`
	// Strength totals. TotalReps is "totalReps" in the activity list and
	// "totalExerciseReps" in the detail response; both land here.
	ActiveSets int `json:"activeSets"`
	TotalSets  int `json:"totalSets"`
	TotalReps  int `json:"totalReps"`
}

// noElevation is the value Garmin sends where an activity has no altitude data.
const noElevation = -500

// normalize replaces Garmin's "no data" sentinels with the package's zero value.
func (m *ActivityMetrics) normalize() {
	for _, v := range []*float64{&m.AvgElevation, &m.MinElevation, &m.MaxElevation} {
		if *v == noElevation {
			*v = 0
		}
	}
}

// ExerciseSummary is one exercise's totals within a strength session. Garmin
// does not expose individual sets, only these per-exercise sums.
type ExerciseSummary struct {
	// Category is Garmin's exercise group, e.g. "SQUAT" or "WARM_UP".
	Category string `json:"category"`
	// SubCategory is the specific movement, e.g. "DUMBBELL_FRONT_SQUAT".
	// It is empty for categories such as WARM_UP.
	SubCategory string `json:"subCategory,omitempty"`
	Sets        int    `json:"sets"`
	Reps        int    `json:"reps"`
	// MaxWeightKg is the heaviest weight used; 0 for bodyweight or banded work.
	MaxWeightKg float64 `json:"maxWeightKg"`
	// VolumeKg is the total weight lifted: the sum of reps times weight.
	VolumeKg float64 `json:"volumeKg"`
	// DurationSeconds is time under load. Rest between sets is excluded, so
	// the exercises add up to the activity's MovingDuration.
	DurationSeconds float64 `json:"durationSeconds"`
}

// UnmarshalJSON implements json.Unmarshaler. Garmin sends weights in grams
// and durations in milliseconds (verified against the session totals).
func (e *ExerciseSummary) UnmarshalJSON(b []byte) error {
	var aux struct {
		Category    string  `json:"category"`
		SubCategory string  `json:"subCategory"`
		Sets        int     `json:"sets"`
		Reps        int     `json:"reps"`
		MaxWeight   float64 `json:"maxWeight"`
		Volume      float64 `json:"volume"`
		Duration    float64 `json:"duration"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*e = ExerciseSummary{
		Category:        aux.Category,
		SubCategory:     aux.SubCategory,
		Sets:            aux.Sets,
		Reps:            aux.Reps,
		MaxWeightKg:     aux.MaxWeight / 1000,
		VolumeKg:        aux.Volume / 1000,
		DurationSeconds: aux.Duration / 1000,
	}
	return nil
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
	// Exercises is the per-exercise breakdown of a strength session; nil for
	// other activity types. Only the activity list carries it.
	Exercises []ExerciseSummary `json:"-"`
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

		Exercises []ExerciseSummary `json:"summarizedExerciseSets"`
	}{plain: (*plain)(a)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	a.HRTimeInZone = [5]float64{aux.Z1, aux.Z2, aux.Z3, aux.Z4, aux.Z5}
	a.Exercises = aux.Exercises
	a.ActivityMetrics.normalize()
	return nil
}

// ActivityDetail is the response for a single activity's summary. Laps and
// splits are not available from the Personal API (the obvious paths return
// 404). Per-sample metrics and the GPS track exist at
// /activity-service/activity/{id}/details but are not wrapped yet.
type ActivityDetail struct {
	rawJSON
	ID       int64
	Name     string
	Type     ActivityType
	Summary  ActivityMetrics
	TimeZone string
	// Sensors lists the external sensors paired during the activity, such as
	// a chest strap or a power meter. The watch itself is not listed.
	Sensors []Sensor
}

// Sensor is an external sensor recorded with an activity. String fields hold
// Garmin's enum names verbatim, e.g. "POLAR_ELECTRO", "ANTPLUS", "HEART_RATE".
type Sensor struct {
	Manufacturer string `json:"manufacturer"`
	// SourceType is the connection: "ANTPLUS", "BLUETOOTH_LOW_ENERGY", ...
	SourceType string `json:"sourceType"`
	// DeviceType is what the sensor measures, e.g. "HEART_RATE" or "BIKE_POWER".
	DeviceType      string  `json:"antplusDeviceType"`
	SoftwareVersion float64 `json:"softwareVersion"`
	SerialNumber    int64   `json:"serialNumber"`
	// BatteryStatus is Garmin's label ("OK", "LOW", ...); BatteryLevel is a
	// percentage, nil when the sensor does not report one.
	BatteryStatus string `json:"batteryStatus"`
	BatteryLevel  *int   `json:"batteryLevel"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *ActivityDetail) UnmarshalJSON(b []byte) error {
	var aux struct {
		ID      int64        `json:"activityId"`
		Name    string       `json:"activityName"`
		Type    ActivityType `json:"activityTypeDTO"`
		Summary struct {
			ActivityMetrics
			TrainingEffect    float64 `json:"trainingEffect"`    // detail's name for the aerobic score
			TotalExerciseReps int     `json:"totalExerciseReps"` // detail's name for totalReps
		} `json:"summaryDTO"`
		TimeZone struct {
			TimeZone string `json:"timeZone"`
		} `json:"timeZoneUnitDTO"`
		Metadata struct {
			Sensors []Sensor `json:"sensors"`
		} `json:"metadataDTO"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	d.ID, d.Name, d.Type = aux.ID, aux.Name, aux.Type
	d.Sensors = aux.Metadata.Sensors
	d.Summary = aux.Summary.ActivityMetrics
	d.Summary.normalize()
	if d.Summary.AerobicTrainingEffect == 0 {
		d.Summary.AerobicTrainingEffect = aux.Summary.TrainingEffect
	}
	if d.Summary.TotalReps == 0 {
		d.Summary.TotalReps = aux.Summary.TotalExerciseReps
	}
	d.TimeZone = aux.TimeZone.TimeZone
	return nil
}

// ActivitiesOptions filters, sorts and pages the activity list. The zero value
// returns the 99 most recent activities (Garmin's own defaults). Zero-valued
// filters are omitted, so a bound of 0 means "no bound".
type ActivitiesOptions struct {
	// Start is the offset into the result list (0 = first).
	Start int
	// Limit is the page size. Defaults to 99.
	Limit int

	// StartDate and EndDate bound activities by local start date, inclusive.
	StartDate, EndDate time.Time

	// TypeKey filters by parent activity type, e.g. "running", "cycling" or
	// "fitness_equipment". Child types such as "strength_training" are
	// rejected by Garmin with HTTP 400; use SubTypeKey for those.
	TypeKey string
	// SubTypeKey filters by child activity type, e.g. "strength_training" or
	// "trail_running". Parent types are rejected with HTTP 400. When TypeKey is
	// also set, the child must belong to that parent.
	SubTypeKey string
	// ExcludedSubTypes drops activities of these child types.
	ExcludedSubTypes []string
	// EventType filters by event classification: race, training, recreation,
	// fitness, specialEvent or uncategorized. Garmin does not validate it: a
	// value no activity has (activities are often all "uncategorized") returns
	// an empty list, not an error.
	EventType string

	// Search matches activity names and descriptions.
	Search string
	// Favorite filters on favorite status; nil applies no filter.
	Favorite *bool
	// IncludeChildren includes child activities such as the legs of a
	// multisport activity, which Garmin excludes by default.
	IncludeChildren bool

	// SortBy names the field to sort by, such as "name", "distance" or
	// "duration". Defaults to the local start time.
	SortBy string
	// Ascending sorts oldest/smallest first. Garmin sorts descending by default.
	Ascending bool

	// Range filters. Distances and elevation are metres, durations seconds
	// (MinDuration/MaxDuration use the timer; the Elapsed variants include
	// paused time), power is watts. Elevation is total gain.
	MinDistance, MaxDistance               int64
	MinDuration, MaxDuration               int64
	MinElapsedDuration, MaxElapsedDuration int64
	MinElevationGain, MaxElevationGain     int64
	MinAvgPower                            int64

	// RequiredFields lists activity fields that must be non-null for an
	// activity to be included. Names follow Garmin's search vocabulary, which
	// does not always match the response JSON: "avgHr" works while the
	// response key "averageHR" matches nothing. Unrecognised names are not an
	// error, they silently match no activities, so check results when using this.
	RequiredFields []string
}

func (o ActivitiesOptions) query() (url.Values, error) {
	if o.Start < 0 || o.Limit < 0 {
		return nil, fmt.Errorf("garmin: negative start or limit")
	}
	limit := o.Limit
	if limit == 0 {
		limit = 99
	}
	q := url.Values{
		"start": {strconv.Itoa(o.Start)},
		"limit": {strconv.Itoa(limit)},
	}
	if err := o.addDates(q); err != nil {
		return nil, err
	}
	o.addFilters(q)
	if err := o.addRanges(q); err != nil {
		return nil, err
	}
	return q, nil
}

// addDates sets the date bounds, checking they are in order.
func (o ActivitiesOptions) addDates(q url.Values) error {
	if !o.StartDate.IsZero() {
		q.Set("startDate", dateParam(o.StartDate))
	}
	if !o.EndDate.IsZero() {
		q.Set("endDate", dateParam(o.EndDate))
	}
	if !o.StartDate.IsZero() && !o.EndDate.IsZero() {
		return checkRange(o.StartDate, o.EndDate)
	}
	return nil
}

// addFilters sets the type, text, sorting and flag filters; zero values are
// left out so Garmin's defaults apply.
func (o ActivitiesOptions) addFilters(q url.Values) {
	for key, v := range map[string]string{
		"activityType":    o.TypeKey,
		"activitySubType": o.SubTypeKey,
		"eventType":       o.EventType,
		"search":          o.Search,
		"sortBy":          o.SortBy,
	} {
		if v != "" {
			q.Set(key, v)
		}
	}
	for _, k := range o.ExcludedSubTypes {
		q.Add("excludedActivitySubType", k)
	}
	for _, f := range o.RequiredFields {
		q.Add("requiredField", f)
	}
	if o.Ascending {
		q.Set("sortOrder", "asc")
	}
	if o.IncludeChildren {
		q.Set("excludeChildren", "false")
	}
	if o.Favorite != nil {
		if *o.Favorite {
			q.Set("favorite", "1")
		} else {
			q.Set("favorite", "0")
		}
	}
}

// addRanges sets the distance, duration, elevation and power bounds. Negative
// values and a minimum above its maximum are errors.
func (o ActivitiesOptions) addRanges(q url.Values) error {
	ranges := []struct {
		minKey, maxKey string
		lo, hi         int64
	}{
		{"minDistance", "maxDistance", o.MinDistance, o.MaxDistance},
		{"minDuration", "maxDuration", o.MinDuration, o.MaxDuration},
		{"minElapsedDuration", "maxElapsedDuration", o.MinElapsedDuration, o.MaxElapsedDuration},
		{"minElevation", "maxElevation", o.MinElevationGain, o.MaxElevationGain},
	}
	for _, r := range ranges {
		if r.lo < 0 || r.hi < 0 {
			return fmt.Errorf("garmin: negative %s/%s", r.minKey, r.maxKey)
		}
		if r.lo > 0 && r.hi > 0 && r.lo > r.hi {
			return fmt.Errorf("garmin: %s (%d) exceeds %s (%d)", r.minKey, r.lo, r.maxKey, r.hi)
		}
		if r.lo > 0 {
			q.Set(r.minKey, strconv.FormatInt(r.lo, 10))
		}
		if r.hi > 0 {
			q.Set(r.maxKey, strconv.FormatInt(r.hi, 10))
		}
	}
	if o.MinAvgPower < 0 {
		return fmt.Errorf("garmin: negative minAvgPower")
	}
	if o.MinAvgPower > 0 {
		q.Set("minAvgPower", strconv.FormatInt(o.MinAvgPower, 10))
	}
	return nil
}

// Activities returns one page of activities, newest first unless opts sorts otherwise.
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
//	for a, err := range c.AllActivities(ctx, garminconnect.ActivitiesOptions{}) {
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

// ActivityTypeKeys returns every activity type key Garmin knows, parents and
// children together. The list does not say which is which: parent keys (such
// as "running") go in ActivitiesOptions.TypeKey and child keys (such as
// "trail_running") in ActivitiesOptions.SubTypeKey.
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

package mcpserver

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// authed returns a harness that is already authenticated.
func authed(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.authenticate(t)
	return h
}

// authedCS returns an authenticated harness's client session and a call helper.
func authedCS(t *testing.T) (*harness, func(name string, args map[string]any) *mcp.CallToolResult) {
	t.Helper()
	h := authed(t)
	cs := h.connect(t)
	return h, func(name string, args map[string]any) *mcp.CallToolResult { return callTool(t, cs, name, args) }
}

// --- series fixture: a runner at constant speed ---------------------------------

// detailsBody builds a /details response: speed m/s for totalDist metres, one
// sample per second, with heart rate and elevation.
func detailsBody(speed, totalDist float64, withDistance bool) string {
	desc := []map[string]any{
		{"key": "directTimestamp", "metricsIndex": 0}, {"key": "sumDuration", "metricsIndex": 1}, {"key": "sumDistance", "metricsIndex": 2},
		{"key": "directHeartRate", "metricsIndex": 3}, {"key": "directElevation", "metricsIndex": 4}, {"key": "directDoubleCadence", "metricsIndex": 5},
	}
	var rows []map[string]any
	base := time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC).UnixMilli()
	for sec := 0; ; sec++ {
		d := 0.0
		if withDistance {
			d = min(speed*float64(sec), totalDist)
		}
		rows = append(rows, map[string]any{"metrics": []any{base + int64(sec)*1000, float64(sec), d, 150 + sec%5, 100.0 + float64(sec)/100, 176}})
		if !withDistance && sec >= 600 || withDistance && d >= totalDist {
			break
		}
	}
	b, _ := json.Marshal(map[string]any{"activityDetailMetrics": rows, "metricDescriptors": desc, "metricsCount": len(rows), "totalMetricsCount": len(rows)})
	return string(b)
}

const (
	runListJSON = `{"activityId":42,"activityName":"Morning Run","activityType":{"typeKey":"running","typeId":1,"parentTypeId":17},
	  "startTimeGMT":"2026-11-14 07:00:00","distance":10234.567,"duration":3000.04,"movingDuration":2990.0,"calories":640,"averageHR":150.55,
	  "maxHR":178,"averageSpeed":3.4115,"elevationGain":125,"elevationLoss":124,"aerobicTrainingEffect":3.44,"anaerobicTrainingEffect":1.2,
	  "activityTrainingLoad":120.5,"trainingEffectLabel":"TEMPO","hrTimeInZone_1":100,"hrTimeInZone_2":200.5,"hrTimeInZone_3":300,"hrTimeInZone_4":400,"hrTimeInZone_5":50,"steps":9000}`
	strengthListJSON = `{"activityId":43,"activityName":"Strength","activityType":{"typeKey":"strength_training","typeId":13,"parentTypeId":29},
	  "startTimeGMT":"2026-11-16 08:00:00","distance":0,"duration":4003.8,"movingDuration":2714,"calories":347,"averageHR":121,"totalSets":6,"totalReps":64,
	  "summarizedExerciseSets":[
	    {"category":"SQUAT","subCategory":"DUMBBELL_FRONT_SQUAT","reps":60,"volume":1800000,"duration":290926,"sets":4,"maxWeight":30000},
	    {"category":"WARM_UP","reps":4,"volume":0,"duration":512278,"sets":2,"maxWeight":0}]}`
)

func TestTools_ListAreRegisteredReadOnlyWithSchemas(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s lacks a description or schema", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{"auth_status", "get_activity", "get_activity_series", "get_activity_splits", "get_body_battery", "get_daily_summaries",
		"get_heart_rate", "get_heart_rate_zones", "get_hrv", "get_sleep", "get_steps", "get_stress", "get_vo2max", "list_activities"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v\nwant    %v", names, want)
	}
}

func TestTools_AuthStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t)
	type out struct {
		Authenticated bool   `json:"authenticated"`
		Help          string `json:"help"`
	}
	before := decodeResult[out](t, callTool(t, cs, "auth_status", nil))
	if before.Authenticated || !strings.Contains(before.Help, "/auth") {
		t.Errorf("before: %+v", before)
	}
	h.authenticate(t)
	after := decodeResult[out](t, callTool(t, cs, "auth_status", nil))
	if !after.Authenticated || after.Help != "" {
		t.Errorf("after: %+v", after)
	}
}

func TestTools_AllRequireAuthentication(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t)
	for name, args := range map[string]map[string]any{
		"list_activities": nil, "get_activity": {"activity_id": 1}, "get_activity_splits": {"activity_id": 1},
		"get_activity_series": {"activity_id": 1}, "get_daily_summaries": nil, "get_sleep": nil, "get_heart_rate": nil,
		"get_hrv": nil, "get_body_battery": nil, "get_stress": nil, "get_steps": nil, "get_vo2max": nil, "get_heart_rate_zones": nil,
	} {
		res := callTool(t, cs, name, args)
		txt := resultText(res)
		if !res.IsError || !strings.Contains(txt, "no Garmin API key") || !strings.Contains(txt, "/auth") {
			t.Errorf("%s: IsError=%v text=%q", name, res.IsError, txt)
		}
	}
}

func TestTools_ExpiredKeyGivesActionableMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.authenticate(t)
	h.fg.status["/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16"] = 401
	res := callTool(t, h.connect(t), "get_steps", nil)
	if !res.IsError || !strings.Contains(resultText(res), "expired") || !strings.Contains(resultText(res), "/auth") {
		t.Errorf("text = %q", resultText(res))
	}
}

func TestTools_ListActivities(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activitylist-service/activities/search/activities", "["+runListJSON+","+strengthListJSON+"]")

	type exercise struct {
		Category    string  `json:"category"`
		SubCategory string  `json:"sub_category"`
		Sets        int     `json:"sets"`
		Reps        int     `json:"reps"`
		MaxWeightKg float64 `json:"max_weight_kg"`
		VolumeKg    float64 `json:"volume_kg"`
		DurationS   float64 `json:"duration_s"`
	}
	type activity struct {
		ID            int64      `json:"id"`
		Name          string     `json:"name"`
		Type          string     `json:"type"`
		StartTime     string     `json:"start_time"`
		DistanceM     float64    `json:"distance_m"`
		DurationS     float64    `json:"duration_s"`
		AvgHR         float64    `json:"avg_hr"`
		AvgSpeed      float64    `json:"avg_speed_mps"`
		ElevationGain float64    `json:"elevation_gain_m"`
		Aerobic       float64    `json:"aerobic_training_effect"`
		Zones         []float64  `json:"hr_time_in_zone_s"`
		TotalSets     int        `json:"total_sets"`
		Exercises     []exercise `json:"exercises"`
	}
	type out struct {
		Count      int        `json:"count"`
		NextOffset *int       `json:"next_offset"`
		Activities []activity `json:"activities"`
	}
	got := decodeResult[out](t, callTool(t, cs, "list_activities", map[string]any{"limit": 2}))
	if got.Count != 2 || got.NextOffset == nil || *got.NextOffset != 2 {
		t.Fatalf("count=%d next=%v (a full page must offer a next offset)", got.Count, got.NextOffset)
	}
	run, st := got.Activities[0], got.Activities[1]
	if run.ID != 42 || run.Type != "running" || run.StartTime != "2026-11-14T07:00:00Z" || run.DistanceM != 10234.6 || run.DurationS != 3000 ||
		run.AvgHR != 150.6 || run.AvgSpeed != 3.41 || run.ElevationGain != 125 || run.Aerobic != 3.4 {
		t.Errorf("run = %+v (numbers must be rounded)", run)
	}
	if len(run.Zones) != 5 || run.Zones[1] != 200.5 {
		t.Errorf("zones = %v", run.Zones)
	}
	if st.Type != "strength_training" || st.TotalSets != 6 || len(st.Exercises) != 2 {
		t.Fatalf("strength = %+v", st)
	}
	if sq := st.Exercises[0]; sq.SubCategory != "DUMBBELL_FRONT_SQUAT" || sq.MaxWeightKg != 30 || sq.VolumeKg != 1800 || sq.DurationS != 290.9 {
		t.Errorf("exercise units not converted: %+v", sq)
	}
	if q, _ := url.ParseQuery(h.fg.query("/activitylist-service/activities/search/activities")); q.Get("limit") != "2" || q.Get("start") != "0" {
		t.Errorf("query = %v", q)
	}

	short := decodeResult[out](t, callTool(t, cs, "list_activities", map[string]any{"limit": 5}))
	if short.NextOffset != nil {
		t.Errorf("a short page must not offer a next offset: %v", *short.NextOffset)
	}
}

func TestTools_ListActivitiesForwardsFilters(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activitylist-service/activities/search/activities", "[]")
	callTool(t, cs, "list_activities", map[string]any{
		"start_date": "2026-10-01", "end_date": "2026-11-15", "type": "fitness_equipment", "sub_type": "strength_training",
		"search": "leg day", "min_distance_m": 5000, "max_duration_s": 7200, "sort_by": "distance", "ascending": true, "offset": 40,
	})
	q, _ := url.ParseQuery(h.fg.query("/activitylist-service/activities/search/activities"))
	for k, want := range map[string]string{
		"startDate": "2026-10-01", "endDate": "2026-11-15", "activityType": "fitness_equipment", "activitySubType": "strength_training",
		"search": "leg day", "minDistance": "5000", "maxDuration": "7200", "sortBy": "distance", "sortOrder": "asc", "start": "40", "limit": "20",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q (query %v)", k, q.Get(k), want, q)
		}
	}
}

func TestTools_ListActivitiesValidatesInput(t *testing.T) {
	t.Parallel()
	_, call := authedCS(t)
	for name, args := range map[string]map[string]any{
		"limit too high":   {"limit": 101},
		"negative limit":   {"limit": -1},
		"bad start date":   {"start_date": "last tuesday"},
		"bad end date":     {"end_date": "2026-13-45"},
		"reversed dates":   {"start_date": "2026-11-15", "end_date": "2026-11-01"},
		"wrong field type": {"limit": "many"},
	} {
		if res := call("list_activities", args); !res.IsError {
			t.Errorf("%s: expected a tool error, got %s", name, resultText(res))
		}
	}
}

func TestTools_GetActivity(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activity-service/activity/42", `{"activityId":42,"activityName":"Morning Run","activityTypeDTO":{"typeKey":"running"},
	  "timeZoneUnitDTO":{"timeZone":"Europe/Bucharest"},
	  "summaryDTO":{"startTimeGMT":"2026-11-14 07:00:00.0","distance":20335.37,"duration":6476.17,"averageHR":175,"elevationGain":125,"trainingEffect":4.1},
	  "metadataDTO":{"sensors":[{"manufacturer":"POLAR_ELECTRO","serialNumber":424242,"sourceType":"ANTPLUS","antplusDeviceType":"HEART_RATE",
	    "softwareVersion":1.0,"batteryStatus":"OK","batteryLevel":90}]}}`)
	type out struct {
		ID        int64   `json:"id"`
		Type      string  `json:"type"`
		TimeZone  string  `json:"time_zone"`
		DistanceM float64 `json:"distance_m"`
		Aerobic   float64 `json:"aerobic_training_effect"`
		Start     string  `json:"start_time"`
		Sensors   []struct {
			Type, Manufacturer, Connection string
			BatteryStatus                  string  `json:"battery_status"`
			SoftwareVersion                float64 `json:"software_version"`
			BatteryPercent                 *int    `json:"battery_percent"`
		} `json:"sensors"`
	}
	res := callTool(t, cs, "get_activity", map[string]any{"activity_id": 42})
	got := decodeResult[out](t, res)
	if got.ID != 42 || got.Type != "running" || got.TimeZone != "Europe/Bucharest" || got.DistanceM != 20335.4 || got.Aerobic != 4.1 || got.Start != "2026-11-14T07:00:00Z" {
		t.Errorf("got %+v", got)
	}
	if len(got.Sensors) != 1 {
		t.Fatalf("sensors = %+v", got.Sensors)
	}
	if s := got.Sensors[0]; s.Type != "HEART_RATE" || s.Manufacturer != "POLAR_ELECTRO" || s.Connection != "ANTPLUS" ||
		s.BatteryStatus != "OK" || s.SoftwareVersion != 1 || s.BatteryPercent == nil || *s.BatteryPercent != 90 {
		t.Errorf("sensor = %+v", s)
	}
	// Every field Garmin sent, including those without a curated equivalent,
	// arrives unmodified under "raw".
	type rawOut struct {
		Raw struct {
			Metadata struct {
				Sensors []struct {
					Serial int64 `json:"serialNumber"`
				} `json:"sensors"`
			} `json:"metadataDTO"`
			Summary map[string]any `json:"summaryDTO"`
		} `json:"raw"`
	}
	whole := decodeResult[rawOut](t, res)
	if s := whole.Raw.Metadata.Sensors; len(s) != 1 || s[0].Serial != 424242 {
		t.Errorf("raw.metadataDTO.sensors = %+v", s)
	}
	if whole.Raw.Summary["trainingEffect"] != 4.1 {
		t.Errorf("raw.summaryDTO = %v", whole.Raw.Summary)
	}
	if res := callTool(t, cs, "get_activity", map[string]any{"activity_id": 999}); !res.IsError || !strings.Contains(resultText(res), "not found") {
		t.Errorf("unknown id: %q", resultText(res))
	}
	if res := callTool(t, cs, "get_activity", nil); !res.IsError {
		t.Error("a missing activity_id must be rejected by the input schema")
	}
}

func TestTools_ActivitySplits(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activity-service/activity/42/details", detailsBody(4, 2500, true))
	type split struct {
		Index   int     `json:"index"`
		Dist    float64 `json:"distance_m"`
		Dur     float64 `json:"duration_s"`
		Pace    float64 `json:"pace_s_per_km"`
		Cadence float64 `json:"avg_cadence_spm"`
		HR      float64 `json:"avg_hr"`
		Partial bool    `json:"partial"`
	}
	type out struct {
		SplitM float64 `json:"split_distance_m"`
		Splits []split `json:"splits"`
	}
	got := decodeResult[out](t, callTool(t, cs, "get_activity_splits", map[string]any{"activity_id": 42}))
	if got.SplitM != 1000 || len(got.Splits) != 3 {
		t.Fatalf("got %+v", got)
	}
	if s := got.Splits[0]; s.Dist != 1000 || s.Dur != 250 || s.Pace != 250 || s.Cadence != 176 || s.HR < 150 || s.HR > 155 || s.Partial {
		t.Errorf("split 1 = %+v", s)
	}
	if s := got.Splits[2]; !s.Partial || s.Dist != 500 || s.Dur != 125 {
		t.Errorf("final split = %+v", s)
	}
	if q, _ := url.ParseQuery(h.fg.query("/activity-service/activity/42/details")); q.Get("maxChartSize") != "20000" {
		t.Errorf("full resolution not requested: %v", q)
	}
	mile := decodeResult[out](t, callTool(t, cs, "get_activity_splits", map[string]any{"activity_id": 42, "split_distance_m": 1609.344}))
	if mile.SplitM != 1609.344 || len(mile.Splits) != 2 {
		t.Errorf("miles: %+v", mile)
	}
	if res := callTool(t, cs, "get_activity_splits", map[string]any{"activity_id": 42, "split_distance_m": 5}); !res.IsError {
		t.Error("a tiny split size must be rejected")
	}
}

func TestTools_ActivitySplitsWithoutDistance(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activity-service/activity/43/details", detailsBody(0, 0, false))
	res := callTool(t, cs, "get_activity_splits", map[string]any{"activity_id": 43})
	if !res.IsError || !strings.Contains(resultText(res), "no distance data") || !strings.Contains(resultText(res), "strength") {
		t.Errorf("IsError=%v text=%q", res.IsError, resultText(res))
	}
}

func TestTools_ActivitySeriesIsThinnedEvenlyAndHasNoGPS(t *testing.T) {
	t.Parallel()
	h := authed(t)
	cs := h.connect(t)
	h.fg.route("/activity-service/activity/42/details", detailsBody(4, 2500, true)) // 626 samples
	type point struct {
		TimeS float64  `json:"time_s"`
		Dist  float64  `json:"distance_m"`
		HR    *float64 `json:"hr"`
		Elev  *float64 `json:"elevation_m"`
	}
	type out struct {
		Total  int     `json:"total_points"`
		Points []point `json:"points"`
	}
	got := decodeResult[out](t, callTool(t, cs, "get_activity_series", map[string]any{"activity_id": 42, "max_points": 10}))
	if got.Total != 626 || len(got.Points) != 10 {
		t.Fatalf("total=%d points=%d", got.Total, len(got.Points))
	}
	if got.Points[0].TimeS != 0 || got.Points[9].TimeS != 625 || got.Points[9].Dist != 2500 {
		t.Errorf("first/last = %+v / %+v (thinning must keep both ends)", got.Points[0], got.Points[9])
	}
	for i := 1; i < len(got.Points); i++ {
		if got.Points[i].TimeS <= got.Points[i-1].TimeS {
			t.Fatalf("points not strictly increasing in time at %d: %+v", i, got.Points)
		}
	}
	if got.Points[0].HR == nil || got.Points[0].Elev == nil {
		t.Errorf("optional metrics missing: %+v", got.Points[0])
	}
	all := decodeResult[out](t, callTool(t, cs, "get_activity_series", map[string]any{"activity_id": 42, "max_points": 1000}))
	if len(all.Points) != 626 {
		t.Errorf("when everything fits, nothing is dropped: got %d", len(all.Points))
	}
	def := decodeResult[out](t, callTool(t, cs, "get_activity_series", map[string]any{"activity_id": 42}))
	if len(def.Points) != 200 {
		t.Errorf("default max_points: got %d, want 200", len(def.Points))
	}
	for _, bad := range []int{5, 1001, -3} {
		if res := callTool(t, cs, "get_activity_series", map[string]any{"activity_id": 42, "max_points": bad}); !res.IsError {
			t.Errorf("max_points %d must be rejected", bad)
		}
	}
	raw := resultText(callTool(t, cs, "get_activity_series", map[string]any{"activity_id": 42}))
	for _, forbidden := range []string{"lat", "lon", "polyline", "coordinate"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Errorf("series output mentions %q", forbidden)
		}
	}
}

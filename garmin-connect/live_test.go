//go:build live

package garminconnect

// Live capture: calls the real Personal API and writes the *shape* of each
// response (field names and types, no values) to testdata/shapes/.
//
//	go test -tags live -run TestLiveCapture -v ./garmin-connect
//
// The API key is read from the file named by GARMIN_KEY_FILE (default "../key", the repository root).
// Values are blanked before anything touches disk, so the shape files contain
// no personal data and are safe to commit.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

func TestLiveCapture(t *testing.T) {
	keyFile := os.Getenv("GARMIN_KEY_FILE")
	if keyFile == "" {
		keyFile = "../key"
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Skipf("no key file: %v", err)
	}
	c := New(WithTokenSource(auth.NewTokenSource(string(key))))

	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	week := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	type call struct {
		name, path string
		q          url.Values
	}
	calls := []call{
		{"user_settings", "/userprofile-service/userprofile/settings", nil},
		{"activities_search", "/activitylist-service/activities/search/activities", url.Values{"start": {"0"}, "limit": {"3"}}},
		{"daily_summaries", "/usersummary-service/v2/dailysummaries", url.Values{"startDate": {week}, "endDate": {day}}},
		{"sleep_daily", "/sleep-service/sleep/dailySleep", url.Values{"date": {day}}},
		{"sleep_summaries", "/sleep-service/sleep/dailySleepSummariesByDate", url.Values{"startDate": {week}, "endDate": {day}}},
		{"heart_rate_daily", "/wellness-service/wellness/dailyHeartRate", url.Values{"date": {day}}},
		{"hrv_day", "/hrv-service/hrv/" + day, nil},
		{"body_battery", "/wellness-service/wellness/bodyBattery/reports/daily", url.Values{"startDate": {week}, "endDate": {day}}},
		{"steps_daily", "/usersummary-service/stats/steps/daily/" + week + "/" + day, nil},
		{"stress_daily", "/usersummary-service/stats/stress/daily/" + week + "/" + day, nil},
	}

	dir := filepath.Join("testdata", "shapes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Activity detail needs a real ID; take the newest one from the search.
	var found []struct {
		ID int64 `json:"activityId"`
	}
	if err := c.get(ctx, calls[1].path, calls[1].q, &found); err == nil && len(found) > 0 {
		id := strconv.FormatInt(found[0].ID, 10)
		calls = append(calls,
			call{"activity_detail", "/activity-service/activity/" + id, nil},
			call{"activity_types", "/activity-service/activity/activityTypes/simple", nil},
		)
	}

	for _, cl := range calls {
		var raw json.RawMessage
		if err := c.get(ctx, cl.path, cl.q, &raw); err != nil {
			t.Logf("%-18s FAILED: %v", cl.name, errSummary(err))
			continue
		}
		shape, err := shapeOf(raw)
		if err != nil {
			t.Logf("%-18s not JSON: %v", cl.name, err)
			continue
		}
		out := filepath.Join(dir, cl.name+".json")
		if err := os.WriteFile(out, shape, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%-18s ok  -> %s (%d bytes)", cl.name, out, len(shape))
	}
}

// errSummary reports status codes without echoing response bodies, which may
// contain personal data.
func errSummary(err error) string {
	if ae, ok := err.(*APIError); ok {
		return strings.TrimSpace(ae.Method + " HTTP " + itoa(ae.StatusCode))
	}
	return err.Error()
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// shapeOf blanks every value in a JSON document, keeping keys and types.
// Arrays keep only their first element's shape; numbers stay int-vs-float.
func shapeOf(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(blank(v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func blank(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[k] = blank(x[k])
		}
		return out
	case []any:
		if len(x) == 0 {
			return []any{}
		}
		return []any{blank(x[0])}
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return 0.5
		}
		return 1
	case string:
		return ""
	case bool:
		return false
	default:
		return nil
	}
}

// TestLiveTyped runs the typed endpoints against the real API and reports
// only counts and parse health, never values.
//
//	go test -tags live -run TestLiveTyped -v ./garmin-connect
func TestLiveTyped(t *testing.T) {
	keyFile := os.Getenv("GARMIN_KEY_FILE")
	if keyFile == "" {
		keyFile = "../key"
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Skipf("no key file: %v", err)
	}
	c := New(WithTokenSource(auth.NewTokenSource(string(key))))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	end := time.Now().AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -13)

	acts, err := c.Activities(ctx, ActivitiesOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Activities: %v", err)
	}
	withTime, withType := 0, 0
	for _, a := range acts {
		if !a.StartTime.IsZero() {
			withTime++
		}
		if a.Type.Key != "" {
			withType++
		}
	}
	t.Logf("Activities: %d (startTime parsed: %d, type key: %d)", len(acts), withTime, withType)

	n := 0
	for _, err := range c.AllActivities(ctx, ActivitiesOptions{Limit: 7}) {
		if err != nil {
			t.Fatalf("AllActivities: %v", err)
		}
		if n++; n >= 16 {
			break
		}
	}
	t.Logf("AllActivities paged through %d across page size 7", n)

	if len(acts) > 0 {
		d, err := c.ActivityDetail(ctx, acts[0].ID)
		if err != nil {
			t.Fatalf("ActivityDetail: %v", err)
		}
		t.Logf("ActivityDetail: id match=%v startTime parsed=%v distance>0=%v", d.ID == acts[0].ID, !d.Summary.StartTime.IsZero(), d.Summary.Distance > 0)

		var buf bytes.Buffer
		if err := c.DownloadActivity(ctx, acts[0].ID, &buf); err != nil {
			t.Errorf("DownloadActivity: %v", err)
		} else {
			t.Logf("DownloadActivity: %d bytes, zip magic=%v", buf.Len(), bytes.HasPrefix(buf.Bytes(), []byte("PK")))
		}
	}

	if keys, err := c.ActivityTypeKeys(ctx); err != nil {
		t.Errorf("ActivityTypeKeys: %v", err)
	} else {
		t.Logf("ActivityTypeKeys: %d", len(keys))
	}

	if ds, err := c.DailySummaries(ctx, start, end); err != nil {
		t.Errorf("DailySummaries: %v", err)
	} else {
		steps := 0
		for _, d := range ds {
			if d.Steps != nil && d.Steps.Value > 0 {
				steps++
			}
		}
		t.Logf("DailySummaries: %d days, %d with steps", len(ds), steps)
	}
	if s, err := c.Steps(ctx, start, end); err != nil {
		t.Errorf("Steps: %v", err)
	} else {
		t.Logf("Steps: %d days", len(s))
	}
	if s, err := c.Stress(ctx, start, end); err != nil {
		t.Errorf("Stress: %v", err)
	} else {
		t.Logf("Stress: %d days", len(s))
	}

	if s, err := c.Sleep(ctx, end); err != nil {
		t.Errorf("Sleep: %v", err)
	} else {
		t.Logf("Sleep: start parsed=%v total>0=%v score present=%v", !s.Start.IsZero(), s.TotalSeconds > 0, s.Scores.Overall.Value > 0)
	}
	if ss, err := c.SleepRange(ctx, start, end); err != nil {
		t.Errorf("SleepRange: %v", err)
	} else {
		t.Logf("SleepRange: %d nights", len(ss))
	}

	if h, err := c.HeartRate(ctx, end); err != nil {
		t.Errorf("HeartRate: %v", err)
	} else {
		t.Logf("HeartRate: resting>0=%v samples=%d first sample parsed=%v", h.Resting > 0, len(h.Samples), len(h.Samples) > 0 && !h.Samples[0].Time.IsZero())
	}
	if h, err := c.HRV(ctx, end); err != nil {
		t.Errorf("HRV: %v", err)
	} else {
		t.Logf("HRV: summary present=%v readings=%d", h.Summary != nil, len(h.Readings))
	}
	if bb, err := c.BodyBattery(ctx, start, end); err != nil {
		t.Errorf("BodyBattery: %v", err)
	} else {
		samples := 0
		for _, d := range bb {
			samples += len(d.Samples)
		}
		t.Logf("BodyBattery: %d days, %d samples", len(bb), samples)
	}
}

// TestLiveStoreActivity saves the raw responses for one activity to
// testdata/live/ (git-ignored: they contain real GPS and health data).
// It picks the newest activity of type GARMIN_ACTIVITY_TYPE (default
// "running"), or the one in GARMIN_ACTIVITY_ID.
//
//	go test -tags live -run TestLiveStoreActivity -v ./garmin-connect
func TestLiveStoreActivity(t *testing.T) {
	keyFile := os.Getenv("GARMIN_KEY_FILE")
	if keyFile == "" {
		keyFile = "../key"
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Skipf("no key file: %v", err)
	}
	c := New(WithTokenSource(auth.NewTokenSource(string(key))))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	typeKey := os.Getenv("GARMIN_ACTIVITY_TYPE")
	if typeKey == "" {
		typeKey = "running"
	}
	var listEntry json.RawMessage
	id := os.Getenv("GARMIN_ACTIVITY_ID")
	if id == "" {
		acts, err := c.Activities(ctx, ActivitiesOptions{Limit: 1, TypeKey: typeKey})
		if err != nil || len(acts) == 0 {
			t.Fatalf("no %q activity found: %v", typeKey, err)
		}
		id = strconv.FormatInt(acts[0].ID, 10)
		listEntry = acts[0].Raw()
	}

	dir := filepath.Join("testdata", "live")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	save := func(name string, raw json.RawMessage) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("saved %-26s %7d bytes", name, buf.Len())
	}
	if listEntry != nil {
		save("activity_list_entry.json", listEntry)
	}
	for name, path := range map[string]string{
		"activity_detail.json":  "/activity-service/activity/" + id,
		"activity_details.json": "/activity-service/activity/" + id + "/details",
	} {
		var raw json.RawMessage
		if err := c.get(ctx, path, nil, &raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		save(name, raw)
	}
}

package mcpserver

import (
	"encoding/json"
	"maps"
	"testing"
)

// TestTools_RawData checks that each tool backed by a Garmin record returns
// that record unmodified under "raw" by default, so fields the curated output
// does not map still reach the client, and leaves it out when the client sets
// include_raw_data to false. The probe field has no curated equivalent.
func TestTools_RawData(t *testing.T) {
	t.Parallel()
	const probe = `"probe":{"nested":[1,2]}`
	tests := []struct {
		tool, path, body string
		args             map[string]any
		// items extracts the per-item objects from the tool's result.
		items func(map[string]json.RawMessage) []json.RawMessage
	}{
		{tool: "list_activities", path: "/activitylist-service/activities/search/activities",
			body: `[{"activityId":1,"activityType":{"typeKey":"running"},` + probe + `}]`, items: list("activities")},
		{tool: "get_activity", path: "/activity-service/activity/1", args: map[string]any{"activity_id": 1},
			body: `{"activityId":1,"activityTypeDTO":{"typeKey":"running"},` + probe + `}`, items: self},
		{tool: "get_daily_summaries", path: "/usersummary-service/v2/dailysummaries",
			body: `{"data":[{"calendarDate":"2026-11-15",` + probe + `}]}`, items: list("days")},
		{tool: "get_sleep", path: "/sleep-service/sleep/dailySleepSummariesByDate",
			body: `[{"calendarDate":"2026-11-15",` + probe + `}]`, items: list("days")},
		{tool: "get_heart_rate", path: "/wellness-service/wellness/dailyHeartRate",
			body: `{"calendarDate":"2026-11-16",` + probe + `}`, items: self},
		{tool: "get_hrv", path: "/hrv-service/hrv/daily/2026-11-10/2026-11-16",
			body: `{"hrvSummaries":[{"calendarDate":"2026-11-15",` + probe + `}]}`, items: list("days")},
		{tool: "get_body_battery", path: "/wellness-service/wellness/bodyBattery/reports/daily",
			body: `[{"date":"2026-11-15",` + probe + `}]`, items: list("days")},
		{tool: "get_stress", path: "/usersummary-service/stats/stress/daily/2026-11-10/2026-11-16",
			body: `[{"calendarDate":"2026-11-15","values":{},` + probe + `}]`, items: list("days")},
		{tool: "get_steps", path: "/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16",
			body: `[{"calendarDate":"2026-11-15",` + probe + `}]`, items: list("days")},
		{tool: "get_vo2max", path: "/metrics-service/metrics/maxmet/daily/2026-11-10/2026-11-16",
			args: map[string]any{"start": "2026-11-10", "end": "2026-11-16"},
			body: `[{"generic":{"calendarDate":"2026-11-14","sport":"RUNNING"},` + probe + `}]`, items: list("days")},
		{tool: "get_heart_rate_zones", path: "/biometric-service/heartRateZones/v2",
			body: `[{"sport":"RUNNING","trainingMethod":"HR_MAX",` + probe + `}]`, items: self},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			h := authed(t)
			cs := h.connect(t)
			h.fg.route(tt.path, tt.body)

			// call runs the tool with the test's arguments plus extra, and
			// returns its single item.
			call := func(extra map[string]any) json.RawMessage {
				t.Helper()
				args := maps.Clone(tt.args)
				if args == nil {
					args = map[string]any{}
				}
				maps.Copy(args, extra)
				res := callTool(t, cs, tt.tool, args)
				if res.IsError {
					t.Fatalf("tool error: %s", resultText(res))
				}
				items := tt.items(decodeResult[map[string]json.RawMessage](t, res))
				if len(items) != 1 {
					t.Fatalf("got %d items, want 1: %s", len(items), resultText(res))
				}
				return items[0]
			}

			for name, extra := range map[string]map[string]any{
				"default":  nil,
				"explicit": {"include_raw_data": true},
			} {
				var item struct {
					Raw struct {
						Probe struct {
							Nested []int `json:"nested"`
						} `json:"probe"`
					} `json:"raw"`
				}
				got := call(extra)
				if err := json.Unmarshal(got, &item); err != nil {
					t.Fatal(err)
				}
				if n := item.Raw.Probe.Nested; len(n) != 2 || n[0] != 1 || n[1] != 2 {
					t.Errorf("%s: raw.probe missing or altered in %s", name, got)
				}
			}

			var off map[string]json.RawMessage
			if err := json.Unmarshal(call(map[string]any{"include_raw_data": false}), &off); err != nil {
				t.Fatal(err)
			}
			if _, ok := off["raw"]; ok {
				t.Errorf("include_raw_data=false must leave raw out: %v", off)
			}
		})
	}
}

func TestRawData(t *testing.T) {
	t.Parallel()
	rec := json.RawMessage(`{"a":1}`)
	if got := rawData(true, rec); string(got.(json.RawMessage)) != `{"a":1}` {
		t.Errorf("rawData(true, rec) = %v", got)
	}
	// nil, not a typed nil inside the interface, so omitempty drops the field.
	for name, got := range map[string]any{
		"disabled":         rawData(false, rec),
		"no record (204)":  rawData(true, nil),
		"disabled, empty":  rawData(false, nil),
		"empty raw record": rawData(true, json.RawMessage{}),
	} {
		if got != nil {
			t.Errorf("%s: got %#v, want nil", name, got)
		}
	}
	b, err := json.Marshal(StepsDayOut{Date: "2026-11-15", Raw: rawData(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["raw"]; ok {
		t.Errorf("an empty raw record must be omitted: %s", b)
	}
}

func TestWantRaw(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	if !wantRaw(nil) || !wantRaw(&yes) || wantRaw(&no) {
		t.Error("include_raw_data must default to true and honour an explicit value")
	}
}

func self(m map[string]json.RawMessage) []json.RawMessage {
	b, _ := json.Marshal(m)
	return []json.RawMessage{b}
}

func list(key string) func(map[string]json.RawMessage) []json.RawMessage {
	return func(m map[string]json.RawMessage) []json.RawMessage {
		var items []json.RawMessage
		_ = json.Unmarshal(m[key], &items)
		return items
	}
}

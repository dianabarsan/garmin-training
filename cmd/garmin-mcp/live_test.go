//go:build live

package main

// Live end-to-end check: saves the real Garmin key with -set-key, starts the
// server with it, connects as an MCP client and calls every tool against the
// real API.
// Only counts and sanity results are logged, never values.
//
//	go test -tags live -run TestLive -v ./cmd/garmin-mcp
//
// The key is read from the file named by GARMIN_KEY_FILE (default ../../key).

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveAllTools(t *testing.T) {
	keyFile := os.Getenv("GARMIN_KEY_FILE")
	if keyFile == "" {
		keyFile = "../../key"
	}
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		t.Skipf("no key file: %v", err)
	}
	key := strings.TrimSpace(string(keyBytes))

	// 1. Save the key the way setup.sh does: checked with Garmin, then stored.
	store := filepath.Join(t.TempDir(), "garmin.key")
	var out syncBuf
	if err := run(context.Background(), []string{"-set-key", "-key-store", store}, noEnv, strings.NewReader(key), &out, io.Discard, nil); err != nil {
		t.Fatalf("-set-key: %v", err)
	}
	if strings.Contains(out.String(), key) {
		t.Fatal("-set-key printed the key")
	}

	base, logs, stop := start(t, nil, "-key-store", store)
	defer func() { _ = stop() }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "live", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: protocol error %v", name, err)
		}
		if res.IsError {
			var sb strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			t.Logf("%-22s tool error: %s", name, sb.String())
			return nil, false
		}
		b, _ := json.Marshal(res.StructuredContent)
		t.Logf("%-22s %7d bytes of output", name, len(b))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, true
	}

	// 2. Every data tool.
	n := func(m map[string]any, field string) int {
		if a, ok := m[field].([]any); ok {
			return len(a)
		}
		return -1
	}
	end := "2026-11-15"
	rng := map[string]any{"start": "2026-11-02", "end": end}

	acts, ok := call("list_activities", map[string]any{"limit": 10})
	if !ok || n(acts, "activities") < 1 {
		t.Fatalf("list_activities: %v", acts)
	}
	list := acts["activities"].([]any)
	var runID, strengthID float64
	for _, a := range list {
		m := a.(map[string]any)
		switch m["type"] {
		case "running":
			if runID == 0 {
				runID = m["id"].(float64)
			}
		case "strength_training":
			if strengthID == 0 {
				strengthID = m["id"].(float64)
			}
		}
	}
	t.Logf("list_activities: %d activities (next_offset=%v); found a run=%v and a strength session=%v", len(list), acts["next_offset"], runID != 0, strengthID != 0)

	strength, ok := call("list_activities", map[string]any{"sub_type": "strength_training", "limit": 3})
	if ok {
		first := strength["activities"].([]any)[0].(map[string]any)
		t.Logf("list_activities(sub_type=strength_training): %d, all strength=%v, first has %d exercises", n(strength, "activities"), allType(strength, "strength_training"), len(asSlice(first["exercises"])))
	}
	if runID != 0 {
		if d, ok := call("get_activity", map[string]any{"activity_id": runID}); ok {
			t.Logf("get_activity: type=%v distance>0=%v time_zone set=%v", d["type"], d["distance_m"].(float64) > 0, d["time_zone"] != "")
		}
		if sp, ok := call("get_activity_splits", map[string]any{"activity_id": runID}); ok {
			var total float64
			for _, s := range sp["splits"].([]any) {
				total += s.(map[string]any)["distance_m"].(float64)
			}
			t.Logf("get_activity_splits: %d splits covering %.0f m", n(sp, "splits"), total)
		}
		if se, ok := call("get_activity_series", map[string]any{"activity_id": runID, "max_points": 50}); ok {
			t.Logf("get_activity_series: %v samples thinned to %d", se["total_points"], n(se, "points"))
		}
	}
	if strengthID != 0 {
		if _, ok := call("get_activity_splits", map[string]any{"activity_id": strengthID}); ok {
			t.Error("strength splits should be a tool error")
		}
	}
	for _, c := range []struct{ tool, field string }{
		{"get_daily_summaries", "days"}, {"get_sleep", "days"}, {"get_hrv", "days"}, {"get_body_battery", "days"},
		{"get_stress", "days"}, {"get_steps", "days"}, {"get_vo2max", "days"},
	} {
		if out, ok := call(c.tool, rng); ok {
			t.Logf("%-22s %d days returned (count=%v)", c.tool, n(out, c.field), out["count"])
		} else {
			t.Errorf("%s failed", c.tool)
		}
	}
	if hr, ok := call("get_heart_rate", map[string]any{"date": end, "include_samples": true, "bucket_minutes": 60}); ok {
		t.Logf("get_heart_rate: resting>0=%v, %v readings thinned to %d hourly buckets", hr["resting_hr"].(float64) > 0, hr["sample_count"], n(hr, "samples"))
	}
	if z, ok := call("get_heart_rate_zones", nil); ok {
		t.Logf("get_heart_rate_zones: sport=%v configured=%v zones=%d", z["sport"], z["configured"], n(z, "zones"))
	}

	// 3. A year of sleep goes through the range splitting transparently.
	if sl, ok := call("get_sleep", map[string]any{"start": "2025-11-16", "end": end}); ok {
		t.Logf("get_sleep over 365 days: %d entries", n(sl, "days"))
	}

	// 4. Nothing secret in the server's log.
	if strings.Contains(logs.String(), key) {
		t.Error("the key appears in the server log")
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func allType(m map[string]any, typ string) bool {
	for _, a := range m["activities"].([]any) {
		if a.(map[string]any)["type"] != typ {
			return false
		}
	}
	return true
}

package garminconnect_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

// Made-up numbers in Garmin's real response layout.
const zonesJSON = `[
  {"trainingMethod":"LACTATE_THRESHOLD","restingHeartRateUsed":50,"lactateThresholdHeartRateUsed":170,
   "zone1Floor":120,"zone2Floor":140,"zone3Floor":155,"zone4Floor":165,"zone5Floor":175,
   "maxHeartRateUsed":190,"restingHrAutoUpdateUsed":true,"sport":"DEFAULT"},
  {"trainingMethod":"LACTATE_THRESHOLD","restingHeartRateUsed":50,"lactateThresholdHeartRateUsed":170,
   "zone1Floor":110,"zone2Floor":135,"zone3Floor":150,"zone4Floor":160,"zone5Floor":170,
   "maxHeartRateUsed":190,"restingHrAutoUpdateUsed":false,"sport":"RUNNING"}]`

// The unconfigured response from Garmin's Swagger docs.
const unconfiguredZonesJSON = `[{"trainingMethod":"HR_MAX","restingHeartRateUsed":0,"lactateThresholdHeartRateUsed":0,
  "zone1Floor":0,"zone2Floor":0,"zone3Floor":0,"zone4Floor":0,"zone5Floor":0,"maxHeartRateUsed":0,
  "restingHrAutoUpdateUsed":true,"sport":"DEFAULT"}]`

func TestHeartRateZones_Decode(t *testing.T) {
	t.Parallel()
	c, last := serve(t, zonesJSON)
	got, err := c.HeartRateZones(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if last.URL.Path != "/biometric-service/heartRateZones/v2" {
		t.Errorf("path = %s", last.URL.Path)
	}
	if _, has := last.URL.Query()["generateDefaults"]; has {
		t.Errorf("Garmin's default is true; nothing should be sent: %v", last.URL.Query())
	}
	d := got[0]
	if d.Sport != "DEFAULT" || d.TrainingMethod != "LACTATE_THRESHOLD" || d.RestingHR != 50 || d.LactateThresholdHR != 170 || d.MaxHR != 190 || !d.RestingHRAutoUpdate {
		t.Errorf("default = %+v", d)
	}
	want := []garminconnect.HeartRateZone{{1, 120, 139}, {2, 140, 154}, {3, 155, 164}, {4, 165, 174}, {5, 175, 190}}
	if len(d.Zones) != 5 {
		t.Fatalf("zones = %+v", d.Zones)
	}
	for i, w := range want {
		if d.Zones[i] != w {
			t.Errorf("zone %d = %+v, want %+v", i+1, d.Zones[i], w)
		}
	}
	if !d.Configured() || got[1].Sport != "RUNNING" || got[1].Zones[0].MinBPM != 110 || got[1].RestingHRAutoUpdate {
		t.Errorf("running = %+v", got[1])
	}
	if !strings.Contains(string(d.Raw()), "zone1Floor") {
		t.Error("raw not retained")
	}
}

func TestHeartRateZones_UnconfiguredFromSwaggerExample(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, unconfiguredZonesJSON)
	got, err := c.HeartRateZones(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	z := got[0]
	if z.Configured() || z.Zones != nil || z.ZoneFor(150) != 0 {
		t.Errorf("unconfigured zones must be empty: %+v", z)
	}
	if z.TrainingMethod != "HR_MAX" || z.Sport != "DEFAULT" || !z.RestingHRAutoUpdate {
		t.Errorf("fields = %+v", z)
	}
}

func TestHeartRateZones_ZoneFor(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, zonesJSON)
	zs, _ := c.HeartRateZones(context.Background())
	z := zs[0] // floors 120 140 155 165 175, max 190
	tests := []struct{ bpm, want int }{
		{0, 0}, {119, 0}, {120, 1}, {139, 1}, {140, 2}, {154, 2}, {155, 3}, {164, 3},
		{165, 4}, {174, 4}, {175, 5}, {190, 5}, {230, 5}, // above max still counts as zone 5
	}
	for _, tc := range tests {
		if got := z.ZoneFor(tc.bpm); got != tc.want {
			t.Errorf("ZoneFor(%d) = %d, want %d", tc.bpm, got, tc.want)
		}
	}
}

func TestHeartRateZones_MaxBelowTopFloorStillYieldsValidRange(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"sport":"DEFAULT","zone1Floor":100,"zone2Floor":120,"zone3Floor":140,"zone4Floor":160,"zone5Floor":180,"maxHeartRateUsed":170}]`)
	zs, err := c.HeartRateZones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if z5 := zs[0].Zones[4]; z5.MaxBPM < z5.MinBPM {
		t.Errorf("zone 5 range inverted: %+v", z5)
	}
}

func TestHeartRateZones_WithoutDefaultZones(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[]`)
	if _, err := c.HeartRateZones(context.Background(), garminconnect.WithoutDefaultZones()); err != nil {
		t.Fatal(err)
	}
	if got := last.URL.Query().Get("generateDefaults"); got != "false" {
		t.Errorf("generateDefaults = %q, want false", got)
	}
}

func TestHeartRateZonesFor(t *testing.T) {
	t.Parallel()
	c, last := serve(t, zonesJSON)
	ctx := context.Background()

	r, err := c.HeartRateZonesFor(ctx, "running")
	if err != nil || r.Sport != "RUNNING" || r.Zones[0].MinBPM != 110 {
		t.Errorf("running = %+v err %v", r, err)
	}
	d, err := c.HeartRateZonesFor(ctx, "cycling") // no entry: falls back to DEFAULT
	if err != nil || d.Sport != "DEFAULT" {
		t.Errorf("cycling fallback = %+v err %v", d, err)
	}
	if _, err := c.HeartRateZonesFor(ctx, "running", garminconnect.WithoutDefaultZones()); err != nil {
		t.Fatal(err)
	}
	if last.URL.Query().Get("generateDefaults") != "false" {
		t.Error("options must be passed through")
	}

	noDefault, _ := serve(t, `[{"sport":"RUNNING","zone1Floor":100}]`)
	if _, err := noDefault.HeartRateZonesFor(ctx, "cycling"); !errors.Is(err, garminconnect.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := noContentClient(t).HeartRateZonesFor(ctx, "running"); !errors.Is(err, garminconnect.ErrNotFound) {
		t.Errorf("no data err = %v, want ErrNotFound", err)
	}
}

func TestHeartRateZones_NoContentAndErrors(t *testing.T) {
	t.Parallel()
	if got, err := noContentClient(t).HeartRateZones(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("204: %v err %v", got, err)
	}
	c := newTestClient(t, http.NotFoundHandler())
	if _, err := c.HeartRateZones(context.Background()); !errors.Is(err, garminconnect.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	bad, _ := serve(t, `{"not":"a list"}`)
	if _, err := bad.HeartRateZones(context.Background()); err == nil {
		t.Error("expected decode error")
	}
}

func TestHeartRateZones_MarshalsReadableJSON(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, zonesJSON)
	zs, _ := c.HeartRateZones(context.Background())
	b, err := json.Marshal(zs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"sport":"DEFAULT"`, `"maxHeartRate":190`, `"zone":1`, `"minBpm":120`, `"maxBpm":139`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json %s lacks %s", b, want)
		}
	}
}

// Garmin reports time spent in each zone for every activity. Recomputing it
// from per-second heart rate with our zone boundaries must agree, which proves
// that a floor is an inclusive lower bound. Uses the local, git-ignored
// captures of a real run; skipped when absent.
func TestHeartRateZones_LocalRealRunMatchesGarminTimeInZone(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "live")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skipf("no local capture %s", name)
		}
		return b
	}
	zonesRaw, details, entry := read("hr_zones.json"), read("nov12_0_details.json"), read("nov12_0_list_entry.json")

	zc, _ := serve(t, string(zonesRaw))
	zs, err := zc.HeartRateZonesFor(context.Background(), "running")
	if err != nil || !zs.Configured() {
		t.Fatalf("zones = %+v err %v", zs, err)
	}
	sc, _ := serve(t, string(details))
	series, err := sc.ActivitySeries(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ac, _ := serve(t, "["+string(entry)+"]")
	acts, err := ac.Activities(context.Background(), garminconnect.ActivitiesOptions{})
	if err != nil || len(acts) != 1 {
		t.Fatalf("activities: %v", err)
	}

	var secs [6]float64 // index 0 = below zone 1
	for i := 1; i < len(series.Points); i++ {
		a, b := series.Points[i-1], series.Points[i]
		if a.HeartRate == nil || b.Duration < a.Duration {
			continue
		}
		secs[zs.ZoneFor(int(*a.HeartRate))] += b.Duration - a.Duration
	}
	for z := 1; z <= 5; z++ {
		want := acts[0].HRTimeInZone[z-1]
		if !near(secs[z], want, max(10, 0.02*want)) {
			t.Errorf("zone %d: %.0f s from series, Garmin says %.0f s", z, secs[z], want)
		}
	}
}

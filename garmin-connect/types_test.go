package garminconnect_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

func TestTimestamp_Unmarshal(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 15, 6, 30, 5, 0, time.UTC)
	tests := []struct {
		name, in string
		want     time.Time
		wantErr  bool
	}{
		{"space layout", `"2026-11-15 06:30:05"`, want, false},
		{"fractional", `"2026-11-15 06:30:05.0"`, want, false},
		{"T layout", `"2026-11-15T06:30:05.0"`, want, false},
		{"rfc3339 with offset", `"2026-11-15T07:30:05+01:00"`, want, false},
		{"epoch ms", strconv.FormatInt(want.UnixMilli(), 10), want, false},
		{"epoch ms float", strconv.FormatInt(want.UnixMilli(), 10) + ".0", want, false},
		{"null", `null`, time.Time{}, false},
		{"empty string", `""`, time.Time{}, false},
		{"garbage string", `"yesterday"`, time.Time{}, true},
		{"bool", `true`, time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ts garminconnect.Timestamp
			err := json.Unmarshal([]byte(tc.in), &ts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !ts.Equal(tc.want) {
				t.Errorf("got %v, want %v", ts.Time, tc.want)
			}
			if !tc.wantErr && ts.Location() != time.UTC && !ts.IsZero() {
				t.Errorf("location = %v, want UTC", ts.Location())
			}
		})
	}
}

func TestTimestamp_RoundTrip(t *testing.T) {
	t.Parallel()
	in := garminconnect.Timestamp{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out garminconnect.Timestamp
	if err := json.Unmarshal(b, &out); err != nil || !out.Equal(in.Time) {
		t.Errorf("round trip: %s -> %v (%v)", b, out.Time, err)
	}
}

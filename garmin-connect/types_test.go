package garminconnect_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
		{"epoch ms out of range", `1e300`, time.Time{}, true},
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

func TestRaw_ReturnsACopy(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"calendarDate":"2026-11-01","totalSteps":5}]`)
	days, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01"))
	if err != nil || len(days) != 1 {
		t.Fatalf("days=%v err=%v", days, err)
	}
	r := days[0].Raw()
	r[0] = 'X'
	if days[0].Raw()[0] != '{' {
		t.Error("changing the result of Raw changed the model")
	}
}

// Garmin sometimes writes a whole number as 1234.0. That must not fail the
// response, while a real fraction in an integer field still does.
func TestDecoding_WholeNumbersWithAFractionFitIntegerFields(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `[{"calendarDate":"2026-11-01","totalSteps":1234.0,"stepGoal":1e4,"totalDistance":812.5}]`)
	days, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01"))
	if err != nil || len(days) != 1 || days[0].TotalSteps != 1234 || days[0].StepGoal != 10000 || days[0].TotalDistance != 812.5 {
		t.Fatalf("days=%+v err=%v", days, err)
	}
	if !strings.Contains(string(days[0].Raw()), "1234.0") {
		t.Error("Raw must keep Garmin's original text")
	}
	c, _ = serve(t, `[{"calendarDate":"2026-11-01","totalSteps":12.5}]`)
	if _, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01")); err == nil {
		t.Error("a fraction in an integer field must still be an error")
	}
}

func TestDecoding_NullMeansNoData(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `null`)
	s, err := c.Sleep(context.Background(), day("2026-11-01"))
	if err != nil || s.Raw() != nil || s.CalendarDate != "" {
		t.Errorf("a null body: sleep=%+v err=%v, want the zero value", s, err)
	}
	c, _ = serve(t, `[null,{"calendarDate":"2026-11-01","totalSteps":5},null]`)
	days, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01"))
	if err != nil || len(days) != 1 {
		t.Errorf("null elements should be skipped: %d days, err %v", len(days), err)
	}
}

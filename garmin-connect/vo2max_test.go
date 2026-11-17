package garminconnect_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

func TestVO2Max(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[
	  {"generic":{"calendarDate":"2026-11-10","sport":"RUNNING","subSport":"GENERIC","vo2MaxValue":47.0,"vo2MaxPreciseValue":46.9},"cycling":null},
	  {"generic":{"calendarDate":"2026-11-14","sport":"RUNNING","subSport":"GENERIC","vo2MaxValue":48.0,"vo2MaxPreciseValue":47.6},
	   "cycling":{"calendarDate":"2026-11-14","sport":"CYCLING","subSport":"ROAD","vo2MaxValue":52.0,"vo2MaxPreciseValue":51.8}}]`)
	got, err := c.VO2Max(context.Background(), d1, d2)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if last.URL.Path != "/metrics-service/metrics/maxmet/daily/2026-11-09/2026-11-15" {
		t.Errorf("path = %s", last.URL.Path)
	}
	if _, has := last.URL.Query()["sport"]; has {
		t.Errorf("no sport filter was requested but query = %v", last.URL.Query())
	}
	g := got[0].Generic
	if g == nil || g.CalendarDate != "2026-11-10" || g.Sport != "RUNNING" || g.SubSport != "GENERIC" || g.Value != 47 || g.Precise != 46.9 {
		t.Errorf("generic = %+v", g)
	}
	if got[0].Cycling != nil {
		t.Errorf("null cycling must be nil: %+v", got[0].Cycling)
	}
	if cy := got[1].Cycling; cy == nil || cy.Sport != "CYCLING" || cy.Value != 52 || got[1].Generic.Precise != 47.6 {
		t.Errorf("entry with cycling = %+v / %+v", got[1].Generic, cy)
	}
	if !strings.Contains(string(got[0].Raw()), `"cycling":null`) {
		t.Errorf("raw = %s", got[0].Raw())
	}
}

func TestVO2Max_SportsAreRepeatedParams(t *testing.T) {
	t.Parallel()
	c, last := serve(t, `[]`)
	got, err := c.VO2Max(context.Background(), d1, d2, "running", "CYCLING")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v err %v", got, err)
	}
	if s := last.URL.Query()["sport"]; len(s) != 2 || s[0] != "running" || s[1] != "CYCLING" {
		t.Errorf("sport params = %v", s)
	}
}

func TestVO2Max_ErrorsAndRange(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	var apiErr *garminconnect.APIError
	if _, err := c.VO2Max(context.Background(), d1, d2, "bogus"); !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Errorf("unknown sport err = %v", err)
	}
	if _, err := c.VO2Max(context.Background(), d2, d1); !errors.Is(err, garminconnect.ErrInvalidRange) {
		t.Errorf("range err = %v", err)
	}
	c2, _ := serve(t, `{"not":"a list"}`)
	if _, err := c2.VO2Max(context.Background(), d1, d2); err == nil {
		t.Error("expected decode error")
	}
}

// --- Sleep: fields from Garmin's documented schema ---------------------------

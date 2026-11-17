package garminconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// HeartRateZone is one training zone as an inclusive beats-per-minute range.
type HeartRateZone struct {
	Number int `json:"zone"` // 1 to 5
	MinBPM int `json:"minBpm"`
	MaxBPM int `json:"maxBpm"`
}

// HeartRateZones is the zone configuration for one sport.
type HeartRateZones struct {
	rawJSON
	// Sport is "DEFAULT", or a sport such as "RUNNING" or "CYCLING" that has
	// its own zones. DEFAULT applies to every sport without an entry.
	Sport string `json:"sport"`
	// TrainingMethod is how the zones are derived, e.g. "LACTATE_THRESHOLD"
	// or "HR_MAX".
	TrainingMethod      string `json:"trainingMethod"`
	RestingHR           int    `json:"restingHeartRate"`
	LactateThresholdHR  int    `json:"lactateThresholdHeartRate"`
	MaxHR               int    `json:"maxHeartRate"`
	RestingHRAutoUpdate bool   `json:"restingHeartRateAutoUpdate"`
	// Zones lists zones 1 to 5 with their bpm ranges. It is empty when the
	// user has not configured zones (Garmin then reports every figure as 0).
	Zones []HeartRateZone `json:"zones"`
}

// UnmarshalJSON implements json.Unmarshaler. Garmin sends only the floor of
// each zone; a zone runs up to the next floor, and zone 5 up to the max heart
// rate. Zone figures verified against Garmin's own time-in-zone totals.
func (z *HeartRateZones) UnmarshalJSON(b []byte) error {
	var aux struct {
		Sport               string `json:"sport"`
		TrainingMethod      string `json:"trainingMethod"`
		RestingHR           int    `json:"restingHeartRateUsed"`
		LactateThresholdHR  int    `json:"lactateThresholdHeartRateUsed"`
		MaxHR               int    `json:"maxHeartRateUsed"`
		RestingHRAutoUpdate bool   `json:"restingHrAutoUpdateUsed"`
		Floor1              int    `json:"zone1Floor"`
		Floor2              int    `json:"zone2Floor"`
		Floor3              int    `json:"zone3Floor"`
		Floor4              int    `json:"zone4Floor"`
		Floor5              int    `json:"zone5Floor"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*z = HeartRateZones{
		Sport:               aux.Sport,
		TrainingMethod:      aux.TrainingMethod,
		RestingHR:           aux.RestingHR,
		LactateThresholdHR:  aux.LactateThresholdHR,
		MaxHR:               aux.MaxHR,
		RestingHRAutoUpdate: aux.RestingHRAutoUpdate,
	}
	floors := [5]int{aux.Floor1, aux.Floor2, aux.Floor3, aux.Floor4, aux.Floor5}
	if floors[0] > 0 {
		for i, f := range floors {
			upper := aux.MaxHR
			if i < 4 {
				upper = floors[i+1] - 1
			}
			z.Zones = append(z.Zones, HeartRateZone{Number: i + 1, MinBPM: f, MaxBPM: max(upper, f)})
		}
	}
	return nil
}

// Configured reports whether the zones carry real boundaries.
func (z HeartRateZones) Configured() bool { return len(z.Zones) > 0 }

// ZoneFor returns the zone (1 to 5) a heart rate falls in, or 0 when the rate
// is below zone 1 or the zones are not configured. Rates above the maximum
// count as zone 5.
func (z HeartRateZones) ZoneFor(bpm int) int {
	zone := 0
	for _, hz := range z.Zones {
		if bpm >= hz.MinBPM {
			zone = hz.Number
		}
	}
	return zone
}

type zonesConfig struct{ noDefaults bool }

// ZonesOption configures HeartRateZones and HeartRateZonesFor.
type ZonesOption func(*zonesConfig)

// WithoutDefaultZones stops Garmin from generating default zones for users who
// have none configured. By default it does, so the zones are populated.
func WithoutDefaultZones() ZonesOption {
	return func(c *zonesConfig) { c.noDefaults = true }
}

// HeartRateZones returns the heart-rate zone configuration for every sport the
// user has settings for, typically DEFAULT plus RUNNING and CYCLING.
func (c *Client) HeartRateZones(ctx context.Context, opts ...ZonesOption) ([]HeartRateZones, error) {
	var cfg zonesConfig
	for _, o := range opts {
		o(&cfg)
	}
	var q url.Values
	if cfg.noDefaults { // Garmin's own default is true
		q = url.Values{"generateDefaults": {"false"}}
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/biometric-service/heartRateZones/v2", q, &raw); err != nil {
		return nil, err
	}
	out, err := decodeList[HeartRateZones](raw)
	if err != nil {
		return nil, fmt.Errorf("garmin: decoding heart rate zones: %w", err)
	}
	return out, nil
}

// HeartRateZonesFor returns the zones that apply to sport (case-insensitive,
// e.g. "running"), falling back to the DEFAULT zones when the sport has none of
// its own. It returns ErrNotFound when neither exists.
func (c *Client) HeartRateZonesFor(ctx context.Context, sport string, opts ...ZonesOption) (HeartRateZones, error) {
	all, err := c.HeartRateZones(ctx, opts...)
	if err != nil {
		return HeartRateZones{}, err
	}
	var fallback *HeartRateZones
	for i := range all {
		switch {
		case strings.EqualFold(all[i].Sport, sport):
			return all[i], nil
		case strings.EqualFold(all[i].Sport, "DEFAULT"):
			fallback = &all[i]
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return HeartRateZones{}, fmt.Errorf("%w: no heart rate zones for %q", ErrNotFound, sport)
}

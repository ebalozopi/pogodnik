package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"pogodnik/storage"
)

// ═══════════════════════════════════════════════════════════════════════════
// HTTP client shared across all service calls
// ═══════════════════════════════════════════════════════════════════════════

var svcHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	},
}

// ═══════════════════════════════════════════════════════════════════════════
// Primary source — AVWX REST API
// ═══════════════════════════════════════════════════════════════════════════

const avwxStationEndpoint = "https://avwx.rest/api/station/"

// avwxStationResponse maps the subset of the AVWX JSON we need.
//
//	{
//	  "icao":      "KORD",
//	  "city":      "Chicago",
//	  "latitude":  41.9742,
//	  "longitude": -87.9073,
//	  "timezone":  "America/Chicago",
//	  ...
//	}
type avwxStationResponse struct {
	ICAO      string  `json:"icao"`
	City      string  `json:"city"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
	Country   string  `json:"country"`
	Name      string  `json:"name"`
}

// LookupAirport fetches station metadata for a four-letter ICAO code and
// returns a storage.Airport ready to be upserted into the database.
//
// Resolution order:
//  1. AVWX REST API (no key required for /station)
//  2. Fallback hardcoded table (covers the 11 default airports)
//
// The caller may pass an optional AVWX API token via avwxToken.
// If empty, the request is sent without an Authorization header
// (works for the /station endpoint on the free tier at low rates).
func LookupAirport(ctx context.Context, icao string, avwxToken string) (*storage.Airport, error) {
	icao = strings.ToUpper(strings.TrimSpace(icao))
	if len(icao) != 4 {
		return nil, fmt.Errorf("services: ICAO code must be exactly 4 characters, got %q", icao)
	}

	// Try the remote API first.
	apt, err := lookupAVWX(ctx, icao, avwxToken)
	if err == nil {
		return apt, nil
	}

	// Fall back to the built-in table so the bot always works offline.
	if fallback, ok := hardcodedAirports[icao]; ok {
		return &fallback, nil
	}

	return nil, fmt.Errorf("services: airport %s not found (API error: %v)", icao, err)
}

// lookupAVWX performs the actual HTTP call to avwx.rest.
func lookupAVWX(ctx context.Context, icao, token string) (*storage.Airport, error) {
	url := avwxStationEndpoint + icao

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Some endpoints honour a Bearer token for higher rate limits.
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := svcHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var data avwxStationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}

	if data.ICAO == "" {
		return nil, fmt.Errorf("empty ICAO in response")
	}

	city := data.City
	if city == "" {
		city = data.Name // some stations only populate "name"
	}

	tz := data.Timezone
	if tz == "" {
		tz = "UTC"
	}

	return &storage.Airport{
		ICAO:     data.ICAO,
		City:     city,
		Lat:      data.Latitude,
		Lon:      data.Longitude,
		Timezone: tz,
		IsMuted:  false,
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Fallback — hardcoded airports (matches monitor/config.go)
// ═══════════════════════════════════════════════════════════════════════════

var hardcodedAirports = map[string]storage.Airport{
	"NZWN": {ICAO: "NZWN", City: "Wellington", Lat: -41.3272, Lon: 174.8053, Timezone: "Pacific/Auckland"},
	"KORD": {ICAO: "KORD", City: "Chicago", Lat: 41.9742, Lon: -87.9073, Timezone: "America/Chicago"},
	"KMIA": {ICAO: "KMIA", City: "Miami", Lat: 25.7959, Lon: -80.2870, Timezone: "America/New_York"},
	"RKSI": {ICAO: "RKSI", City: "Seoul", Lat: 37.4602, Lon: 126.4407, Timezone: "Asia/Seoul"},
	"KDAL": {ICAO: "KDAL", City: "Dallas", Lat: 32.8471, Lon: -96.8518, Timezone: "America/Chicago"},
	"KSEA": {ICAO: "KSEA", City: "Seattle", Lat: 47.4502, Lon: -122.3088, Timezone: "America/Los_Angeles"},
	"KLGA": {ICAO: "KLGA", City: "New York", Lat: 40.7772, Lon: -73.8726, Timezone: "America/New_York"},
	"KATL": {ICAO: "KATL", City: "Atlanta", Lat: 33.6407, Lon: -84.4277, Timezone: "America/New_York"},
	"EGLC": {ICAO: "EGLC", City: "London", Lat: 51.5053, Lon: 0.0553, Timezone: "Europe/London"},
	"SAEZ": {ICAO: "SAEZ", City: "Buenos Aires", Lat: -34.8222, Lon: -58.5358, Timezone: "America/Argentina/Buenos_Aires"},
	"LTAC": {ICAO: "LTAC", City: "Ankara", Lat: 40.1281, Lon: 32.9951, Timezone: "Europe/Istanbul"},
}

// IsKnownAirport returns true if the ICAO code is in the hardcoded table.
func IsKnownAirport(icao string) bool {
	_, ok := hardcodedAirports[strings.ToUpper(icao)]
	return ok
}

// HardcodedList returns every fallback airport as a slice (useful for
// seeding the database on first run).
func HardcodedList() []storage.Airport {
	list := make([]storage.Airport, 0, len(hardcodedAirports))
	for _, a := range hardcodedAirports {
		list = append(list, a)
	}
	return list
}
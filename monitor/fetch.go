package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// Forecast result types
// ═══════════════════════════════════════════════════════════════════════════

// HourlyForecast is a single data-point from Open-Meteo.
type HourlyForecast struct {
	Time        time.Time
	TempCelsius float64
}

// HourlyExtended adds radiation and humidity to the hourly point.
type HourlyExtended struct {
	Time             time.Time
	TempCelsius      float64
	DirectRadiation  float64 // W/m²
	RelativeHumidity float64 // %
}

// FullForecast holds everything we extract from Open-Meteo for one airport.
type FullForecast struct {
	// Current-hour match (used for Δ calculation).
	CurrentHour     *HourlyForecast
	CurrentExtended *HourlyExtended

	// Next 6 hours starting from current hour (for the outlook block).
	Upcoming         []HourlyForecast
	UpcomingExtended []HourlyExtended

	// Today's daily extremes (index 0 in Open-Meteo daily arrays).
	DailyMax         float64
	DailyMin         float64
	HasDailyExtremes bool

	// Tomorrow's daily extremes (index 1 in Open-Meteo daily arrays).
	TomorrowMax         float64
	TomorrowMin         float64
	HasTomorrowExtremes bool

	// True when served from DB cache rather than a fresh API call.
	FromCache bool
}

// WeatherSnapshot is a read-only copy of WeatherState.
type WeatherSnapshot struct {
	Current     float64
	DailyHigh   float64
	DailyLow    float64
	TrackingDay time.Time
	LastUpdated time.Time
}

// ═══════════════════════════════════════════════════════════════════════════
// Shared HTTP transport
// ═══════════════════════════════════════════════════════════════════════════

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	},
}

// ═══════════════════════════════════════════════════════════════════════════
// METAR / SPECI — fetch from NOAA Aviation Weather
// ═══════════════════════════════════════════════════════════════════════════

// Primary source: NOAA Aviation Weather Center.
// We request the raw text with &hours=0 for the most recent observation
// and &sep=true so that multiple lines are separated clearly.
const noaaMetarEndpoint = "https://aviationweather.gov/api/data/metar"

// FetchMETAR retrieves the current METAR or SPECI for an ICAO station
// directly from NOAA's Aviation Weather API.
//
// This is the sole source for current weather — no AVWX, no Open-Meteo
// for observations. SPECI (special weather observation) is automatically
// detected and preferred over routine METAR when both are present.
func FetchMETAR(ctx context.Context, icao string) (*Observation, error) {
	icao = strings.ToUpper(strings.TrimSpace(icao))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, noaaMetarEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("metar %s: build request: %w", icao, err)
	}

	q := req.URL.Query()
	q.Set("ids", icao)
	q.Set("format", "raw")
	q.Set("hours", "2") // look back 2h for resilience; we pick the freshest
	q.Set("sep", "true")
	req.URL.RawQuery = q.Encode()

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metar %s: fetch: %w", icao, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("metar %s: HTTP %d: %s", icao, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, fmt.Errorf("metar %s: read body: %w", icao, err)
	}

	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return nil, fmt.Errorf("metar %s: empty response from NOAA", icao)
	}

	// NOAA may return multiple observations separated by newlines.
	// Prefer SPECI if present; otherwise take the first (most recent) line.
	bestLine := selectBestObservation(raw)
	if bestLine == "" {
		return nil, fmt.Errorf("metar %s: no valid observation in response", icao)
	}

	obs, err := ParseMETAR(bestLine)
	if err != nil {
		return nil, fmt.Errorf("metar %s: %w", icao, err)
	}

	return obs, nil
}

// selectBestObservation picks the best line from a multi-line NOAA response.
// Priority: SPECI > first METAR line (most recent).
func selectBestObservation(raw string) string {
	lines := strings.Split(raw, "\n")
	bestLine := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// SPECI always wins — it's a significant weather change.
		if strings.HasPrefix(line, "SPECI") || strings.Contains(line, " SPECI ") {
			return line
		}
		// Keep the first non-empty line as fallback (most recent METAR).
		if bestLine == "" {
			bestLine = line
		}
	}
	return bestLine
}

// ═══════════════════════════════════════════════════════════════════════════
// Forecast — Open-Meteo (for forecast ONLY, NOT current weather)
// ═══════════════════════════════════════════════════════════════════════════

const openMeteoEndpoint = "https://api.open-meteo.com/v1/forecast"

// OpenMeteoResponse maps the Open-Meteo JSON we need for forecasts.
type OpenMeteoResponse struct {
	Hourly struct {
		Time               []string  `json:"time"`
		Temperature2m      []float64 `json:"temperature_2m"`
		DirectRadiation    []float64 `json:"direct_radiation"`
		RelativeHumidity2m []float64 `json:"relative_humidity_2m"`
	} `json:"hourly"`

	Daily struct {
		Time             []string  `json:"time"`
		Temperature2mMax []float64 `json:"temperature_2m_max"`
		Temperature2mMin []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

// ═══════════════════════════════════════════════════════════════════════════
// FetchForecast — single current-hour point (backwards-compat)
// ═══════════════════════════════════════════════════════════════════════════

// FetchForecast returns only the current-hour match.
// Kept for callers (like /check) that don't need the full outlook.
func FetchForecast(ctx context.Context, apt Airport) (*HourlyForecast, error) {
	full, err := FetchFullForecastFromAPI(ctx, apt)
	if err != nil {
		return nil, err
	}
	if full.CurrentHour == nil {
		return nil, fmt.Errorf("forecast %s: no current-hour match", apt.ICAO)
	}
	return full.CurrentHour, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// FetchFullForecastFromAPI — hits Open-Meteo directly (no cache)
// ═══════════════════════════════════════════════════════════════════════════

// FetchFullForecastFromAPI retrieves fresh forecast data from Open-Meteo.
// This is used ONLY for forecasts — current weather comes from NOAA METAR.
func FetchFullForecastFromAPI(ctx context.Context, apt Airport) (*FullForecast, error) {
	_, rawJSON, err := fetchOpenMeteoRaw(ctx, apt)
	if err != nil {
		return nil, err
	}
	return ParseForecastJSON(rawJSON, apt)
}

// FetchOpenMeteoRawJSON fetches from API and returns the raw JSON string
// for caching in the database.
func FetchOpenMeteoRawJSON(ctx context.Context, apt Airport) (string, error) {
	_, rawJSON, err := fetchOpenMeteoRaw(ctx, apt)
	if err != nil {
		return "", err
	}
	return rawJSON, nil
}

// fetchOpenMeteoRaw performs the HTTP call and returns both the parsed
// struct and the raw JSON bytes.
func fetchOpenMeteoRaw(ctx context.Context, apt Airport) (*OpenMeteoResponse, string, error) {
	endpoint := fmt.Sprintf(
		"%s?latitude=%.4f&longitude=%.4f"+
			"&hourly=temperature_2m,direct_radiation,relative_humidity_2m"+
			"&daily=temperature_2m_max,temperature_2m_min"+
			"&timezone=%s"+
			"&forecast_days=2",
		openMeteoEndpoint,
		apt.Latitude,
		apt.Longitude,
		url.QueryEscape(apt.Timezone),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("forecast %s: build request: %w", apt.ICAO, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("forecast %s: fetch: %w", apt.ICAO, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("forecast %s: HTTP %d: %s", apt.ICAO, resp.StatusCode, body)
	}

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return nil, "", fmt.Errorf("forecast %s: read body: %w", apt.ICAO, err)
	}

	var payload OpenMeteoResponse
	if err := json.Unmarshal(rawBytes, &payload); err != nil {
		return nil, "", fmt.Errorf("forecast %s: decode JSON: %w", apt.ICAO, err)
	}

	if len(payload.Hourly.Time) == 0 {
		return nil, "", fmt.Errorf("forecast %s: empty hourly data", apt.ICAO)
	}

	return &payload, string(rawBytes), nil
}

// ═══════════════════════════════════════════════════════════════════════════
// ParseForecastJSON — parses cached or fresh JSON into FullForecast
// ═══════════════════════════════════════════════════════════════════════════

// ParseForecastJSON takes raw Open-Meteo JSON (from cache or API) and
// parses it relative to the airport's current local time.
func ParseForecastJSON(rawJSON string, apt Airport) (*FullForecast, error) {
	var payload OpenMeteoResponse
	if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil {
		return nil, fmt.Errorf("forecast %s: parse JSON: %w", apt.ICAO, err)
	}
	return parseFullForecast(apt, payload)
}

// parseFullForecast does the heavy lifting: matches current hour,
// collects next-6 upcoming points, and extracts daily extremes for
// today and tomorrow.
func parseFullForecast(apt Airport, p OpenMeteoResponse) (*FullForecast, error) {
	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		return nil, fmt.Errorf("forecast %s: timezone: %w", apt.ICAO, err)
	}

	now := time.Now().In(loc)
	currentHourKey := now.Format("2006-01-02T15:00")
	todayKey := now.Format("2006-01-02")
	tomorrowKey := now.AddDate(0, 0, 1).Format("2006-01-02")

	ff := &FullForecast{}

	nTimes := len(p.Hourly.Time)
	nTemps := len(p.Hourly.Temperature2m)
	nRad := len(p.Hourly.DirectRadiation)
	nHum := len(p.Hourly.RelativeHumidity2m)

	// ── Hourly: current-hour match + next 6 hours ───────────────────

	for i := 0; i < nTimes; i++ {
		ts := p.Hourly.Time[i]

		parsed, perr := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		if perr != nil {
			continue
		}

		var temp float64
		if i < nTemps {
			temp = p.Hourly.Temperature2m[i]
		}

		hp := HourlyForecast{Time: parsed, TempCelsius: temp}

		ext := HourlyExtended{
			Time:        parsed,
			TempCelsius: temp,
		}
		if i < nRad {
			ext.DirectRadiation = p.Hourly.DirectRadiation[i]
		}
		if i < nHum {
			ext.RelativeHumidity = p.Hourly.RelativeHumidity2m[i]
		}

		// Current-hour match.
		if ts == currentHourKey && ff.CurrentHour == nil {
			hpCopy := hp
			ff.CurrentHour = &hpCopy
			extCopy := ext
			ff.CurrentExtended = &extCopy
		}

		// Upcoming hours (from now onward, up to 6).
		if !parsed.Before(now) && len(ff.Upcoming) < 6 {
			ff.Upcoming = append(ff.Upcoming, hp)
			ff.UpcomingExtended = append(ff.UpcomingExtended, ext)
		}
	}

	// Fallback: use first upcoming as current if no exact key match.
	if ff.CurrentHour == nil && len(ff.Upcoming) > 0 {
		cpy := ff.Upcoming[0]
		ff.CurrentHour = &cpy
	}
	if ff.CurrentExtended == nil && len(ff.UpcomingExtended) > 0 {
		cpy := ff.UpcomingExtended[0]
		ff.CurrentExtended = &cpy
	}

	// ── Daily extremes: today (index 0) + tomorrow (index 1) ────────

	for i, ds := range p.Daily.Time {
		if ds == todayKey {
			if i < len(p.Daily.Temperature2mMax) && i < len(p.Daily.Temperature2mMin) {
				ff.DailyMax = p.Daily.Temperature2mMax[i]
				ff.DailyMin = p.Daily.Temperature2mMin[i]
				ff.HasDailyExtremes = true
			}
		}
		if ds == tomorrowKey {
			if i < len(p.Daily.Temperature2mMax) && i < len(p.Daily.Temperature2mMin) {
				ff.TomorrowMax = p.Daily.Temperature2mMax[i]
				ff.TomorrowMin = p.Daily.Temperature2mMin[i]
				ff.HasTomorrowExtremes = true
			}
		}
	}

	return ff, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// WeatherState management
// ═══════════════════════════════════════════════════════════════════════════

// Update ingests a new temperature reading and maintains daily extremes.
func (ws *WeatherState) Update(tempC float64, apt Airport) error {
	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		return fmt.Errorf("state %s: load timezone: %w", apt.ICAO, err)
	}

	now := time.Now()
	localNow := now.In(loc)
	todayMidnight := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		0, 0, 0, 0, loc,
	)

	ws.Mu.Lock()
	defer ws.Mu.Unlock()

	if !todayMidnight.Equal(ws.TrackingDay) {
		ws.DailyHigh = tempC
		ws.DailyLow = tempC
		ws.TrackingDay = todayMidnight
	} else {
		if tempC > ws.DailyHigh {
			ws.DailyHigh = tempC
		}
		if tempC < ws.DailyLow {
			ws.DailyLow = tempC
		}
	}

	ws.Current = tempC
	ws.LastUpdated = now.UTC()
	return nil
}

// Snapshot returns a point-in-time copy of the weather fields.
func (ws *WeatherState) Snapshot() WeatherSnapshot {
	ws.Mu.RLock()
	defer ws.Mu.RUnlock()
	return WeatherSnapshot{
		Current:     ws.Current,
		DailyHigh:   ws.DailyHigh,
		DailyLow:    ws.DailyLow,
		TrackingDay: ws.TrackingDay,
		LastUpdated: ws.LastUpdated,
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Pressure tracker
// ═══════════════════════════════════════════════════════════════════════════

// PressureReading is a timestamped barometric observation.
type PressureReading struct {
	Time time.Time
	Hpa  float64
}

// PressureTracker keeps pressure readings for trend calculation.
type PressureTracker struct {
	mu       sync.Mutex
	readings []PressureReading
	maxCap   int
}

// NewPressureTracker creates a tracker with given capacity.
func NewPressureTracker(cap int) *PressureTracker {
	if cap <= 0 {
		cap = 60
	}
	return &PressureTracker{
		readings: make([]PressureReading, 0, cap),
		maxCap:   cap,
	}
}

// NewPressureTrackers builds a map keyed by ICAO.
func NewPressureTrackers(airports []Airport, cap int) map[string]*PressureTracker {
	m := make(map[string]*PressureTracker, len(airports))
	for _, a := range airports {
		m[a.ICAO] = NewPressureTracker(cap)
	}
	return m
}

// Record appends a new reading.
func (pt *PressureTracker) Record(t time.Time, hpa float64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.readings) >= pt.maxCap {
		copy(pt.readings, pt.readings[1:])
		pt.readings = pt.readings[:len(pt.readings)-1]
	}
	pt.readings = append(pt.readings, PressureReading{Time: t, Hpa: hpa})
}

// Trend computes the 3-hour pressure change rate.
func (pt *PressureTracker) Trend() (float64, string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	n := len(pt.readings)
	if n < 2 {
		return 0, "Unknown"
	}
	oldest := pt.readings[0]
	newest := pt.readings[n-1]
	span := newest.Time.Sub(oldest.Time)
	if span < 20*time.Minute {
		return 0, "Unknown"
	}
	delta := newest.Hpa - oldest.Hpa
	rate := delta / span.Hours() * 3.0
	switch {
	case rate > 1.0:
		return rate, "Rising"
	case rate < -1.0:
		return rate, "Falling"
	default:
		return rate, "Steady"
	}
}

// Latest returns the most recent reading.
func (pt *PressureTracker) Latest() (PressureReading, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.readings) == 0 {
		return PressureReading{}, false
	}
	return pt.readings[len(pt.readings)-1], true
}

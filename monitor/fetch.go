package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Observation holds fields extracted from a raw METAR string.
type Observation struct {
	Raw         string
	TempCelsius float64
	DewPointC   float64
	WindDir     int
	WindSpeed   int
	WindGust    int
	Visibility  string
	VisMeters   float64
}

// HourlyForecast is a single data-point from Open-Meteo.
type HourlyForecast struct {
	Time        time.Time
	TempCelsius float64
}

// WeatherSnapshot is a read-only copy of WeatherState.
type WeatherSnapshot struct {
	Current     float64
	DailyHigh   float64
	DailyLow    float64
	TrackingDay time.Time
	LastUpdated time.Time
}

// CelsiusToFahrenheit converts Celsius to Fahrenheit.
func CelsiusToFahrenheit(c float64) float64 {
	return c*9.0/5.0 + 32.0
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	},
}

const noaaMetarEndpoint = "https://aviationweather.gov/api/data/metar"

// FetchMETAR retrieves the current METAR for an ICAO station.
func FetchMETAR(ctx context.Context, icao string) (*Observation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, noaaMetarEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("metar %s: build request: %w", icao, err)
	}

	q := req.URL.Query()
	q.Set("ids", strings.ToUpper(icao))
	q.Set("format", "raw")
	req.URL.RawQuery = q.Encode()

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metar %s: fetch: %w", icao, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metar %s: HTTP %d", icao, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return nil, fmt.Errorf("metar %s: read body: %w", icao, err)
	}

	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return nil, fmt.Errorf("metar %s: empty response", icao)
	}

	if idx := strings.IndexByte(raw, '\n'); idx > 0 {
		raw = strings.TrimSpace(raw[:idx])
	}

	return ParseMETAR(raw)
}

var (
	reWind       = regexp.MustCompile(`\b(VRB|\d{3})(\d{2,3})(G(\d{2,3}))?KT\b`)
	reTemp       = regexp.MustCompile(`\b(M?\d{2})/(M?\d{2})\b`)
	reWindVarDir = regexp.MustCompile(`^\d{3}V\d{3}$`)
)

// ParseMETAR extracts temperature, wind, and visibility from raw METAR.
func ParseMETAR(raw string) (*Observation, error) {
	body := raw
	if i := strings.Index(raw, " RMK "); i > 0 {
		body = raw[:i]
	}

	obs := &Observation{Raw: raw}

	tm := reTemp.FindStringSubmatch(body)
	if tm == nil {
		return nil, fmt.Errorf("metar parse: temperature not found in %q", raw)
	}
	obs.TempCelsius = decodeMETARTemp(tm[1])
	obs.DewPointC = decodeMETARTemp(tm[2])

	if wm := reWind.FindStringSubmatch(body); wm != nil {
		if wm[1] == "VRB" {
			obs.WindDir = -1
		} else {
			obs.WindDir, _ = strconv.Atoi(wm[1])
		}
		obs.WindSpeed, _ = strconv.Atoi(wm[2])
		if wm[4] != "" {
			obs.WindGust, _ = strconv.Atoi(wm[4])
		}
	}

	obs.Visibility, obs.VisMeters = extractVisibility(body)

	return obs, nil
}

func decodeMETARTemp(s string) float64 {
	neg := strings.HasPrefix(s, "M")
	s = strings.TrimPrefix(s, "M")
	v, _ := strconv.ParseFloat(s, 64)
	if neg {
		return -v
	}
	return v
}

func extractVisibility(body string) (string, float64) {
	tokens := strings.Fields(body)

	windIdx := -1
	for i, t := range tokens {
		if reWind.MatchString(t) {
			windIdx = i
			break
		}
	}
	if windIdx < 0 || windIdx >= len(tokens)-1 {
		return "N/A", 0
	}

	visIdx := windIdx + 1
	if visIdx < len(tokens) && reWindVarDir.MatchString(tokens[visIdx]) {
		visIdx++
	}
	if visIdx >= len(tokens) {
		return "N/A", 0
	}
	vis := tokens[visIdx]

	if vis == "CAVOK" {
		return "CAVOK", 10_000
	}

	if strings.HasSuffix(vis, "SM") {
		return decodeVisSM(vis, "")
	}

	if visIdx+1 < len(tokens) && strings.HasSuffix(tokens[visIdx+1], "SM") {
		return decodeVisSM(tokens[visIdx+1], vis)
	}

	if len(vis) == 4 {
		if v, err := strconv.Atoi(vis); err == nil {
			m := float64(v)
			if m >= 9999 {
				m = 10_000
			}
			return vis, m
		}
	}

	return "N/A", 0
}

func decodeVisSM(fracToken, wholeToken string) (string, float64) {
	display := fracToken
	if wholeToken != "" {
		display = wholeToken + " " + fracToken
	}

	s := display
	s = strings.TrimSuffix(s, "SM")
	s = strings.TrimLeft(s, "PM")

	var total float64
	for _, part := range strings.Fields(s) {
		if slash := strings.IndexByte(part, '/'); slash > 0 {
			num, _ := strconv.ParseFloat(part[:slash], 64)
			den, _ := strconv.ParseFloat(part[slash+1:], 64)
			if den > 0 {
				total += num / den
			}
		} else {
			v, _ := strconv.ParseFloat(part, 64)
			total += v
		}
	}

	const smToMeters = 1609.34
	return display, total * smToMeters
}

const openMeteoEndpoint = "https://api.open-meteo.com/v1/forecast"

type openMeteoResponse struct {
	Hourly struct {
		Time          []string  `json:"time"`
		Temperature2m []float64 `json:"temperature_2m"`
	} `json:"hourly"`
}

// FetchForecast retrieves hourly forecast from Open-Meteo.
func FetchForecast(ctx context.Context, apt Airport) (*HourlyForecast, error) {
	endpoint := fmt.Sprintf(
		"%s?latitude=%.4f&longitude=%.4f&hourly=temperature_2m&timezone=%s&forecast_days=1",
		openMeteoEndpoint,
		apt.Latitude,
		apt.Longitude,
		url.QueryEscape(apt.Timezone),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("forecast %s: build request: %w", apt.ICAO, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forecast %s: fetch: %w", apt.ICAO, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forecast %s: HTTP %d", apt.ICAO, resp.StatusCode)
	}

	var payload openMeteoResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("forecast %s: decode JSON: %w", apt.ICAO, err)
	}

	if len(payload.Hourly.Time) == 0 {
		return nil, fmt.Errorf("forecast %s: no hourly data", apt.ICAO)
	}

	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		return nil, fmt.Errorf("forecast %s: load timezone: %w", apt.ICAO, err)
	}

	now := time.Now().In(loc)
	targetKey := now.Format("2006-01-02T15:00")

	for i, ts := range payload.Hourly.Time {
		if ts != targetKey {
			continue
		}
		if i >= len(payload.Hourly.Temperature2m) {
			return nil, fmt.Errorf("forecast %s: array mismatch", apt.ICAO)
		}
		parsed, _ := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		return &HourlyForecast{
			Time:        parsed,
			TempCelsius: payload.Hourly.Temperature2m[i],
		}, nil
	}

	return nil, fmt.Errorf("forecast %s: no data for hour %s", apt.ICAO, targetKey)
}

// FetchExtendedForecast retrieves full day hourly data.
func FetchExtendedForecast(ctx context.Context, apt Airport) ([]string, []float64, error) {
	endpoint := fmt.Sprintf(
		"%s?latitude=%.4f&longitude=%.4f&hourly=temperature_2m&timezone=%s&forecast_days=2",
		openMeteoEndpoint,
		apt.Latitude,
		apt.Longitude,
		url.QueryEscape(apt.Timezone),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("forecast %s: build request: %w", apt.ICAO, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("forecast %s: fetch: %w", apt.ICAO, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("forecast %s: HTTP %d", apt.ICAO, resp.StatusCode)
	}

	var payload openMeteoResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("forecast %s: decode JSON: %w", apt.ICAO, err)
	}

	return payload.Hourly.Time, payload.Hourly.Temperature2m, nil
}

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
		0, 0, 0, 0,
		loc,
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
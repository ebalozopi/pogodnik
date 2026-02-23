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

// ═══════════════════════════════════════════════════════════════════════════
// Parsed result types
// ═══════════════════════════════════════════════════════════════════════════

// Observation holds fields extracted from a raw METAR/SPECI string.
type Observation struct {
	Raw         string
	TempCelsius float64
	DewPointC   float64
	WindDir     int
	WindSpeed   int // knots
	WindGust    int
	Visibility  string
	VisMeters   float64
	IsSpeci     bool

	// Weather phenomena tokens extracted from the METAR body
	// (e.g., "RA", "TSRA", "SN", "+SHRA", "-DZ").
	PresentWeather []string
}

// HourlyForecast is a single data-point from Open-Meteo.
type HourlyForecast struct {
	Time        time.Time
	TempCelsius float64
}

// HourlyExtended adds radiation and humidity to the hourly point.
type HourlyExtended struct {
	Time              time.Time
	TempCelsius       float64
	DirectRadiation   float64 // W/m²
	RelativeHumidity  float64 // %
}

// FullForecast holds everything we extract from Open-Meteo for one airport.
type FullForecast struct {
	CurrentHour      *HourlyForecast
	CurrentExtended  *HourlyExtended // includes radiation + humidity
	Upcoming         []HourlyForecast
	UpcomingExtended []HourlyExtended
	DailyMax         float64
	DailyMin         float64
	HasDailyExtremes bool
	FromCache        bool
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
// Unit conversion
// ═══════════════════════════════════════════════════════════════════════════

func CelsiusToFahrenheit(c float64) float64 {
	return c*9.0/5.0 + 32.0
}

// KnotsToMS converts knots to metres per second.
func KnotsToMS(kt int) float64 {
	return float64(kt) * 0.514444
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
// METAR / SPECI — fetch
// ═══════════════════════════════════════════════════════════════════════════

const noaaMetarEndpoint = "https://aviationweather.gov/api/data/metar"

// FetchMETAR retrieves the current METAR or SPECI for an ICAO station.
func FetchMETAR(ctx context.Context, icao string) (*Observation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, noaaMetarEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("metar %s: build request: %w", icao, err)
	}

	q := req.URL.Query()
	q.Set("ids", strings.ToUpper(icao))
	q.Set("format", "raw")
	q.Set("hours", "1")
	req.URL.RawQuery = q.Encode()

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metar %s: fetch: %w", icao, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metar %s: HTTP %d", icao, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return nil, fmt.Errorf("metar %s: read body: %w", icao, err)
	}

	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return nil, fmt.Errorf("metar %s: empty response", icao)
	}

	lines := strings.Split(raw, "\n")
	bestLine := ""
	isSpeci := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "SPECI") || strings.Contains(line, " SPECI ") {
			bestLine = line
			isSpeci = true
			break
		}
		if bestLine == "" {
			bestLine = line
		}
	}

	if bestLine == "" {
		return nil, fmt.Errorf("metar %s: no valid observation found", icao)
	}

	obs, err := ParseMETAR(bestLine)
	if err != nil {
		return nil, err
	}
	obs.IsSpeci = isSpeci
	return obs, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// METAR — parser
// ═══════════════════════════════════════════════════════════════════════════

var (
	reWind       = regexp.MustCompile(`\b(VRB|\d{3})(\d{2,3})(G(\d{2,3}))?KT\b`)
	reTemp       = regexp.MustCompile(`\b(M?\d{2})/(M?\d{2})\b`)
	reWindVarDir = regexp.MustCompile(`^\d{3}V\d{3}$`)

	// Weather phenomena: optional intensity (+/-), optional descriptor
	// (TS, SH, FZ, etc.), and one or more phenomena (RA, SN, DZ, etc.).
	reWeather = regexp.MustCompile(
		`\b([+-]?)(VC)?(MI|PR|BC|DR|BL|SH|TS|FZ)?` +
			`(DZ|RA|SN|SG|IC|PL|GR|GS|UP|BR|FG|FU|VA|DU|SA|HZ|PO|SQ|FC|SS|DS)+\b`,
	)
)

// ParseMETAR extracts temperature, wind, visibility, and present weather.
func ParseMETAR(raw string) (*Observation, error) {
	isSpeci := false
	parseBody := raw
	if strings.HasPrefix(raw, "SPECI ") {
		isSpeci = true
		parseBody = strings.TrimPrefix(raw, "SPECI ")
	}

	body := parseBody
	if i := strings.Index(parseBody, " RMK "); i > 0 {
		body = parseBody[:i]
	}

	obs := &Observation{Raw: raw, IsSpeci: isSpeci}

	// Temperature / dew-point.
	tm := reTemp.FindStringSubmatch(body)
	if tm == nil {
		return nil, fmt.Errorf("metar parse: temperature not found in %q", raw)
	}
	obs.TempCelsius = decodeMETARTemp(tm[1])
	obs.DewPointC = decodeMETARTemp(tm[2])

	// Wind.
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

	// Visibility.
	obs.Visibility, obs.VisMeters = extractVisibility(body)

	// Present weather phenomena.
	wxMatches := reWeather.FindAllString(body, -1)
	obs.PresentWeather = wxMatches

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

// ═══════════════════════════════════════════════════════════════════════════
// Forecast — Open-Meteo JSON mapping (with radiation + humidity)
// ═══════════════════════════════════════════════════════════════════════════

const openMeteoEndpoint = "https://api.open-meteo.com/v1/forecast"

// OpenMeteoResponse now includes direct_radiation and relative_humidity_2m.
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
// FetchFullForecastFromAPI — hits Open-Meteo directly
// ═══════════════════════════════════════════════════════════════════════════

func FetchFullForecastFromAPI(ctx context.Context, apt Airport) (*FullForecast, error) {
	_, rawJSON, err := fetchOpenMeteoRaw(ctx, apt)
	if err != nil {
		return nil, err
	}
	return ParseForecastJSON(rawJSON, apt)
}

// FetchOpenMeteoRawJSON returns the raw JSON for caching.
func FetchOpenMeteoRawJSON(ctx context.Context, apt Airport) (string, error) {
	_, rawJSON, err := fetchOpenMeteoRaw(ctx, apt)
	if err != nil {
		return "", err
	}
	return rawJSON, nil
}

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

func ParseForecastJSON(rawJSON string, apt Airport) (*FullForecast, error) {
	var payload OpenMeteoResponse
	if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil {
		return nil, fmt.Errorf("forecast %s: parse cached JSON: %w", apt.ICAO, err)
	}
	return parseFullForecast(apt, payload)
}

func parseFullForecast(apt Airport, p OpenMeteoResponse) (*FullForecast, error) {
	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		return nil, fmt.Errorf("forecast %s: timezone: %w", apt.ICAO, err)
	}

	now := time.Now().In(loc)
	currentHourKey := now.Format("2006-01-02T15:00")
	todayKey := now.Format("2006-01-02")

	ff := &FullForecast{}

	nTimes := len(p.Hourly.Time)
	nTemps := len(p.Hourly.Temperature2m)
	nRad := len(p.Hourly.DirectRadiation)
	nHum := len(p.Hourly.RelativeHumidity2m)

	for i := 0; i < nTimes; i++ {
		ts := p.Hourly.Time[i]

		parsed, perr := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		if perr != nil {
			continue
		}

		// Basic hourly point.
		var temp float64
		if i < nTemps {
			temp = p.Hourly.Temperature2m[i]
		}

		hp := HourlyForecast{Time: parsed, TempCelsius: temp}

		// Extended hourly point (with radiation + humidity).
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

		// Upcoming hours.
		if !parsed.Before(now) && len(ff.Upcoming) < 6 {
			ff.Upcoming = append(ff.Upcoming, hp)
			ff.UpcomingExtended = append(ff.UpcomingExtended, ext)
		}
	}

	// Fallback: use first upcoming as current.
	if ff.CurrentHour == nil && len(ff.Upcoming) > 0 {
		cpy := ff.Upcoming[0]
		ff.CurrentHour = &cpy
	}
	if ff.CurrentExtended == nil && len(ff.UpcomingExtended) > 0 {
		cpy := ff.UpcomingExtended[0]
		ff.CurrentExtended = &cpy
	}

	// Daily extremes.
	for i, ds := range p.Daily.Time {
		if ds != todayKey {
			continue
		}
		if i < len(p.Daily.Temperature2mMax) && i < len(p.Daily.Temperature2mMin) {
			ff.DailyMax = p.Daily.Temperature2mMax[i]
			ff.DailyMin = p.Daily.Temperature2mMin[i]
			ff.HasDailyExtremes = true
		}
		break
	}

	return ff, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// State management
// ═══════════════════════════════════════════════════════════════════════════

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

type PressureReading struct {
	Time time.Time
	Hpa  float64
}

type PressureTracker struct {
	mu       sync.Mutex
	readings []PressureReading
	maxCap   int
}

func NewPressureTracker(cap int) *PressureTracker {
	if cap <= 0 {
		cap = 60
	}
	return &PressureTracker{
		readings: make([]PressureReading, 0, cap),
		maxCap:   cap,
	}
}

func NewPressureTrackers(airports []Airport, cap int) map[string]*PressureTracker {
	m := make(map[string]*PressureTracker, len(airports))
	for _, a := range airports {
		m[a.ICAO] = NewPressureTracker(cap)
	}
	return m
}

func (pt *PressureTracker) Record(t time.Time, hpa float64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.readings) >= pt.maxCap {
		copy(pt.readings, pt.readings[1:])
		pt.readings = pt.readings[:len(pt.readings)-1]
	}
	pt.readings = append(pt.readings, PressureReading{Time: t, Hpa: hpa})
}

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

func (pt *PressureTracker) Latest() (PressureReading, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.readings) == 0 {
		return PressureReading{}, false
	}
	return pt.readings[len(pt.readings)-1], true
}
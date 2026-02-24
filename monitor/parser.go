package monitor

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ═══════════════════════════════════════════════════════════════════════════
// Parsed observation
// ═══════════════════════════════════════════════════════════════════════════

// Observation holds every field extracted from a raw METAR/SPECI string.
type Observation struct {
	Raw         string
	TempCelsius float64
	DewPointC   float64
	IsPrecise   bool // true when temperature came from T-Group (0.1°C resolution)
	WindDir     int  // degrees; -1 for VRB
	WindSpeed   int  // knots
	WindGust    int  // knots; 0 if no gust
	Visibility  string
	VisMeters   float64
	PressureHpa float64 // QNH in hPa (parsed from Q or A group)
	HasPressure bool
	IsSpeci     bool

	// Weather phenomena tokens extracted from the METAR body.
	PresentWeather []string
}

// ═══════════════════════════════════════════════════════════════════════════
// Compiled patterns
// ═══════════════════════════════════════════════════════════════════════════

var (
	// Wind: 36010KT, 36010G20KT, VRB05KT, 00000KT
	reWind = regexp.MustCompile(`\b(VRB|\d{3})(\d{2,3})(G(\d{2,3}))?KT\b`)

	// Standard temperature block: 02/M01, M05/M10, 15/12
	reTemp = regexp.MustCompile(`\b(M?\d{2})/(M?\d{2})\b`)

	// T-Group in RMK section: T01230045, T10241102
	// Format: T + sign_temp(0|1) + temp(3 digits) + sign_dew(0|1) + dew(3 digits)
	reTGroup = regexp.MustCompile(`\bT([01])(\d{3})([01])(\d{3})\b`)

	// Variable wind direction: 350V020
	reWindVarDir = regexp.MustCompile(`^\d{3}V\d{3}$`)

	// Altimeter setting (US): A2992
	reAltimeterUS = regexp.MustCompile(`\bA(\d{4})\b`)

	// Altimeter setting (ICAO QNH): Q1013
	reAltimeterQNH = regexp.MustCompile(`\bQ(\d{3,4})\b`)

	// Weather phenomena: optional intensity, optional proximity,
	// optional descriptor, one or more phenomenon codes.
	reWeather = regexp.MustCompile(
		`\b([+-]?)(VC)?(MI|PR|BC|DR|BL|SH|TS|FZ)?` +
			`(DZ|RA|SN|SG|IC|PL|GR|GS|UP|BR|FG|FU|VA|DU|SA|HZ|PO|SQ|FC|SS|DS)+\b`,
	)
)

const inHgToHpa = 33.8639

// ═══════════════════════════════════════════════════════════════════════════
// ParseMETAR — main entry point
// ═══════════════════════════════════════════════════════════════════════════

// ParseMETAR extracts all relevant fields from a raw METAR or SPECI string.
//
// Temperature resolution priority:
//  1. T-Group in RMK section → 0.1°C precision (IsPrecise = true)
//  2. Standard temp/dew block → 1°C resolution (IsPrecise = false)
//
// Pressure is parsed from both Q (hPa) and A (inHg→hPa) groups.
func ParseMETAR(raw string) (*Observation, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("metar parse: empty input")
	}

	obs := &Observation{Raw: raw}

	// ── Detect SPECI ────────────────────────────────────────────────────

	if strings.HasPrefix(raw, "SPECI ") {
		obs.IsSpeci = true
	}

	// ── Split body vs RMK ───────────────────────────────────────────────

	body := raw
	rmk := ""
	if i := strings.Index(raw, " RMK "); i > 0 {
		body = raw[:i]
		rmk = raw[i+5:] // everything after " RMK "
	}

	// ── Temperature (priority: T-Group > standard block) ────────────────

	tempParsed := false

	if rmk != "" {
		if m := reTGroup.FindStringSubmatch(rmk); m != nil {
			tSign, tVal := m[1], m[2]
			dSign, dVal := m[3], m[4]

			obs.TempCelsius = decodeTGroupValue(tSign, tVal)
			obs.DewPointC = decodeTGroupValue(dSign, dVal)
			obs.IsPrecise = true
			tempParsed = true
		}
	}

	if !tempParsed {
		if m := reTemp.FindStringSubmatch(body); m != nil {
			obs.TempCelsius = decodeStandardTemp(m[1])
			obs.DewPointC = decodeStandardTemp(m[2])
			obs.IsPrecise = false
			tempParsed = true
		}
	}

	if !tempParsed {
		return nil, fmt.Errorf("metar parse: temperature not found in %q", raw)
	}

	// ── Wind ────────────────────────────────────────────────────────────

	if m := reWind.FindStringSubmatch(body); m != nil {
		if m[1] == "VRB" {
			obs.WindDir = -1
		} else {
			obs.WindDir, _ = strconv.Atoi(m[1])
		}
		obs.WindSpeed, _ = strconv.Atoi(m[2])
		if m[4] != "" {
			obs.WindGust, _ = strconv.Atoi(m[4])
		}
	}

	// ── Visibility ──────────────────────────────────────────────────────

	obs.Visibility, obs.VisMeters = extractVisibility(body)

	// ── Pressure (check full raw — A group is sometimes in RMK) ────────

	obs.PressureHpa, obs.HasPressure = parsePressure(raw)

	// ── Present weather phenomena (body only, not RMK) ──────────────────

	obs.PresentWeather = reWeather.FindAllString(body, -1)

	return obs, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// T-Group decoder
// ═══════════════════════════════════════════════════════════════════════════

// decodeTGroupValue converts T-Group sign+digits to a float64.
//
//	sign "0" = positive, "1" = negative
//	digits "024" = 2.4, "123" = 12.3, "004" = 0.4
func decodeTGroupValue(sign, digits string) float64 {
	v, _ := strconv.ParseFloat(digits, 64)
	v /= 10.0 // "024" → 2.4
	if sign == "1" {
		v = -v
	}
	return v
}

// ═══════════════════════════════════════════════════════════════════════════
// Standard temp/dew decoder
// ═══════════════════════════════════════════════════════════════════════════

// decodeStandardTemp converts METAR temp like "M05" → -5.0, "12" → 12.0.
func decodeStandardTemp(s string) float64 {
	neg := strings.HasPrefix(s, "M")
	s = strings.TrimPrefix(s, "M")
	v, _ := strconv.ParseFloat(s, 64)
	if neg {
		return -v
	}
	return v
}

// ═══════════════════════════════════════════════════════════════════════════
// Pressure parser
// ═══════════════════════════════════════════════════════════════════════════

// parsePressure extracts QNH from Q (hPa) or A (inHg) altimeter groups.
// Q is checked first (ICAO standard), then A (US/Canada).
func parsePressure(raw string) (float64, bool) {
	// Q group: Q1013
	if m := reAltimeterQNH.FindStringSubmatch(raw); m != nil {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil && v > 100 {
			return v, true
		}
	}
	// A group: A2992
	if m := reAltimeterUS.FindStringSubmatch(raw); m != nil {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil && v > 100 {
			return (v / 100.0) * inHgToHpa, true
		}
	}
	return 0, false
}

// ParsePressure is the public convenience wrapper used by engine.go.
func ParsePressure(raw string) (float64, bool) {
	return parsePressure(raw)
}

// ═══════════════════════════════════════════════════════════════════════════
// Visibility parser
// ═══════════════════════════════════════════════════════════════════════════

func extractVisibility(body string) (string, float64) {
	tokens := strings.Fields(body)

	// Find the wind token; visibility typically follows it.
	windIdx := -1
	for i, t := range tokens {
		if reWind.MatchString(t) {
			windIdx = i
			break
		}
	}
	if windIdx < 0 || windIdx >= len(tokens)-1 {
		// No wind token found — scan for known patterns anyway.
		return scanVisibilityTokens(tokens)
	}

	visIdx := windIdx + 1

	// Skip variable wind direction token (e.g., 350V020).
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

	// Statute miles (US): 10SM, 3SM, P6SM, 1/2SM
	if strings.HasSuffix(vis, "SM") {
		return decodeVisSM(vis, "")
	}
	// Fractional SM with whole number preceding: "1 1/2SM"
	if visIdx+1 < len(tokens) && strings.HasSuffix(tokens[visIdx+1], "SM") {
		return decodeVisSM(tokens[visIdx+1], vis)
	}

	// 4-digit metres (ICAO): 9999, 0800, 3000
	if len(vis) == 4 {
		if v, err := strconv.Atoi(vis); err == nil {
			m := float64(v)
			if m >= 9999 {
				m = 10_000
			}
			return vis, m
		}
	}

	return vis, 0
}

// scanVisibilityTokens is a fallback when wind token isn't found.
func scanVisibilityTokens(tokens []string) (string, float64) {
	for _, t := range tokens {
		if t == "CAVOK" {
			return "CAVOK", 10_000
		}
		if strings.HasSuffix(t, "SM") {
			return decodeVisSM(t, "")
		}
		if len(t) == 4 {
			if v, err := strconv.Atoi(t); err == nil && v >= 0 && v <= 9999 {
				m := float64(v)
				if m >= 9999 {
					m = 10_000
				}
				return t, m
			}
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
	s = strings.TrimLeft(s, "PM") // P6SM → 6

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
// Unit conversions
// ═══════════════════════════════════════════════════════════════════════════

// CelsiusToFahrenheit converts °C to °F.
func CelsiusToFahrenheit(c float64) float64 {
	return c*9.0/5.0 + 32.0
}

// KnotsToMS converts knots to metres per second.
func KnotsToMS(kt int) float64 {
	return float64(kt) * 0.514444
}

// ═══════════════════════════════════════════════════════════════════════════
// Weather phenomenon helpers
// ═══════════════════════════════════════════════════════════════════════════

// hasRainOrShowers checks for any precipitation token.
func hasRainOrShowers(wx []string) bool {
	for _, w := range wx {
		u := strings.ToUpper(w)
		if strings.Contains(u, "RA") ||
			strings.Contains(u, "DZ") ||
			strings.Contains(u, "SH") {
			return true
		}
	}
	return false
}

// hasFreezing checks for freezing precipitation (FZRA, FZDZ).
func hasFreezing(wx []string) bool {
	for _, w := range wx {
		if strings.Contains(strings.ToUpper(w), "FZ") {
			return true
		}
	}
	return false
}

// hasFogOrMist checks for FG or BR tokens.
func hasFogOrMist(wx []string) bool {
	for _, w := range wx {
		u := strings.ToUpper(w)
		if strings.Contains(u, "FG") || strings.Contains(u, "BR") {
			return true
		}
	}
	return false
}

// ═══════════════════════════════════════════════════════════════════════════
// Humidity estimation (Magnus formula)
// ═══════════════════════════════════════════════════════════════════════════

// estimateHumidity approximates relative humidity from temp and dew-point.
//
//	RH ≈ 100 × exp((17.625·Td)/(243.04+Td) − (17.625·T)/(243.04+T))
func estimateHumidity(tempC, dewPointC float64) float64 {
	const a = 17.625
	const b = 243.04

	gamma := (a*dewPointC)/(b+dewPointC) - (a*tempC)/(b+tempC)
	rh := 100.0 * math.Exp(gamma)

	if rh > 100 {
		rh = 100
	}
	if rh < 0 {
		rh = 0
	}
	return rh
}

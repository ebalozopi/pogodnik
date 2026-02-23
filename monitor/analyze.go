package monitor

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// Constants
// ═══════════════════════════════════════════════════════════════════════════

const (
	AccuracyAccurate = "Accurate"
	AccuracyClose    = "Close"
	AccuracyOff      = "Off"
	AccuracyPoor     = "Poor"
)

// Sensor bias thresholds.
const (
	solarRadiationThreshold = 600.0 // W/m²
	solarWindThreshold      = 3.0   // m/s
	evapHumidityThreshold   = 80.0  // %
	icingTempRange          = 2.0   // ±°C around 0
	icingHumidityThreshold  = 90.0  // %
)

// ═══════════════════════════════════════════════════════════════════════════
// Pressure parsing
// ═══════════════════════════════════════════════════════════════════════════

var (
	reAltimeterUS  = regexp.MustCompile(`\bA(\d{4})\b`)
	reAltimeterQNH = regexp.MustCompile(`\bQ(\d{3,4})\b`)
)

const inHgToHpa = 33.8639

func ParsePressure(raw string) (float64, bool) {
	if m := reAltimeterQNH.FindStringSubmatch(raw); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v, true
	}
	if m := reAltimeterUS.FindStringSubmatch(raw); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return (v / 100.0) * inHgToHpa, true
	}
	return 0, false
}

// ═══════════════════════════════════════════════════════════════════════════
// Temperature formatting
// ═══════════════════════════════════════════════════════════════════════════

func tempSign(v float64) string {
	if v < 0 {
		return ""
	}
	return "+"
}

func FormatTemp(c float64) string {
	if math.IsInf(c, 0) || math.IsNaN(c) {
		return "N/A"
	}
	f := CelsiusToFahrenheit(c)
	return fmt.Sprintf("%s%.1f°C / %s%.1f°F",
		tempSign(c), c, tempSign(f), f)
}

func FormatDelta(deltaC float64) string {
	deltaF := deltaC * 9.0 / 5.0
	return fmt.Sprintf("%s%.1f°C / %s%.1f°F",
		tempSign(deltaC), deltaC,
		tempSign(deltaF), deltaF)
}

// ═══════════════════════════════════════════════════════════════════════════
// Report sub-structures
// ═══════════════════════════════════════════════════════════════════════════

type ObservationReport struct {
	TempC       float64
	TempDisplay string
	Wind        string
	Visibility  string
	RawMETAR    string
}

type ForecastReport struct {
	Available   bool
	TempC       float64
	TempDisplay string
	ValidTime   time.Time
}

type ComparisonReport struct {
	Available    bool
	DeltaC       float64
	DeltaF       float64
	DeltaDisplay string
	Narrative    string
	Accuracy     string
}

type ExtremesReport struct {
	HighC       float64
	HighDisplay string
	LowC        float64
	LowDisplay  string
	TrackingDay time.Time
	DayDisplay  string
}

type PressureReport struct {
	Available       bool
	CurrentHpa      float64
	Trend           string
	RatePerThreeHrs float64
	Display         string
}

// SensorWarning represents a single sensor bias warning.
type SensorWarning struct {
	Icon    string // emoji
	Title   string // short label
	Detail  string // explanation
}

type Report struct {
	Airport     Airport
	GeneratedAt time.Time
	LocalTime   time.Time

	Observation    ObservationReport
	Forecast       ForecastReport
	Comparison     ComparisonReport
	Extremes       ExtremesReport
	Pressure       PressureReport
	SensorWarnings []SensorWarning
}

// ═══════════════════════════════════════════════════════════════════════════
// AnalyzeWeather
// ═══════════════════════════════════════════════════════════════════════════

func AnalyzeWeather(
	apt Airport,
	obs *Observation,
	fc *HourlyForecast,
	state WeatherSnapshot,
	pt *PressureTracker,
) *Report {
	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now()
	localNow := now.In(loc)

	r := &Report{
		Airport:     apt,
		GeneratedAt: now.UTC(),
		LocalTime:   localNow,
	}

	r.Observation = ObservationReport{
		TempC:       obs.TempCelsius,
		TempDisplay: FormatTemp(obs.TempCelsius),
		Wind:        formatWindAnalyze(obs),
		Visibility:  obs.Visibility,
		RawMETAR:    obs.Raw,
	}

	if fc != nil {
		r.Forecast = ForecastReport{
			Available:   true,
			TempC:       fc.TempCelsius,
			TempDisplay: FormatTemp(fc.TempCelsius),
			ValidTime:   fc.Time,
		}

		deltaC := obs.TempCelsius - fc.TempCelsius
		r.Comparison = ComparisonReport{
			Available:    true,
			DeltaC:       deltaC,
			DeltaF:       deltaC * 9.0 / 5.0,
			DeltaDisplay: FormatDelta(deltaC),
			Narrative:    deltaNarrativeAnalyze(deltaC),
			Accuracy:     classifyAccuracy(math.Abs(deltaC)),
		}
	}

	r.Extremes = ExtremesReport{
		HighC:       state.DailyHigh,
		HighDisplay: FormatTemp(state.DailyHigh),
		LowC:       state.DailyLow,
		LowDisplay:  FormatTemp(state.DailyLow),
		TrackingDay: state.TrackingDay,
		DayDisplay: fmt.Sprintf("%s (%s)",
			state.TrackingDay.Format("2006-01-02"), apt.Timezone),
	}

	if pt != nil {
		latest, ok := pt.Latest()
		if ok {
			rate, trend := pt.Trend()
			var display string
			if trend == "Unknown" {
				display = fmt.Sprintf("%.1f hPa — trend data insufficient", latest.Hpa)
			} else {
				display = fmt.Sprintf("%.1f hPa — %s (%+.1f hPa/3hr)",
					latest.Hpa, trend, rate)
			}
			r.Pressure = PressureReport{
				Available:       true,
				CurrentHpa:      latest.Hpa,
				Trend:           trend,
				RatePerThreeHrs: rate,
				Display:         display,
			}
		}
	}

	return r
}

func formatWindAnalyze(obs *Observation) string {
	if obs.WindSpeed == 0 && obs.WindGust == 0 {
		return "Calm"
	}
	var dir string
	if obs.WindDir < 0 {
		dir = "VRB"
	} else {
		dir = fmt.Sprintf("%03d°", obs.WindDir)
	}
	s := fmt.Sprintf("%s @ %dkt", dir, obs.WindSpeed)
	if obs.WindGust > 0 {
		s += fmt.Sprintf(", gusting %dkt", obs.WindGust)
	}
	return s
}

func classifyAccuracy(absDelta float64) string {
	switch {
	case absDelta < 1.0:
		return AccuracyAccurate
	case absDelta < 2.5:
		return AccuracyClose
	case absDelta < 5.0:
		return AccuracyOff
	default:
		return AccuracyPoor
	}
}

func deltaNarrativeAnalyze(deltaC float64) string {
	switch {
	case deltaC > 0.05:
		return "warmer than forecast"
	case deltaC < -0.05:
		return "cooler than forecast"
	default:
		return "matches forecast"
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Sensor Bias Detection
// ═══════════════════════════════════════════════════════════════════════════

// CheckSensorBias analyses the METAR observation and forecast extended
// data for conditions that can bias the temperature sensor reading.
//
// Returns a slice of SensorWarning (empty if no issues detected).
func CheckSensorBias(obs *Observation, ext *HourlyExtended) []SensorWarning {
	var warnings []SensorWarning

	// ── 1. Solar Heating Bias ───────────────────────────────────────────
	// High direct radiation + low wind = sensor housing absorbs heat.
	// Typical bias: +1 to +3 °C above true air temperature.
	if ext != nil {
		windMS := KnotsToMS(obs.WindSpeed)

		if ext.DirectRadiation > solarRadiationThreshold && windMS < solarWindThreshold {
			severity := "moderate"
			bias := "+1..2°C"
			if ext.DirectRadiation > 800 && windMS < 1.5 {
				severity = "high"
				bias = "+2..3°C"
			}

			warnings = append(warnings, SensorWarning{
				Icon:  "🔥",
				Title: "Solar Heating Risk",
				Detail: fmt.Sprintf(
					"Radiation %.0f W/m², wind %.1f m/s (%s, est. %s bias)",
					ext.DirectRadiation, windMS, severity, bias,
				),
			})
		}
	}

	// ── 2. Evaporative (Wet Bulb) Cooling ───────────────────────────────
	// Rain or showers present + relatively low humidity = evaporation
	// cools the sensor below true air temperature.
	// Typical bias: -0.5 to -1.5 °C.
	if hasRainOrShowers(obs.PresentWeather) {
		humidity := float64(0)
		if ext != nil {
			humidity = ext.RelativeHumidity
		}

		// Also compute humidity from dew-point depression if no ext data.
		if humidity == 0 {
			humidity = estimateHumidity(obs.TempCelsius, obs.DewPointC)
		}

		if humidity > 0 && humidity < evapHumidityThreshold {
			bias := "-0.5..1°C"
			if humidity < 50 {
				bias = "-1..1.5°C"
			}

			warnings = append(warnings, SensorWarning{
				Icon:  "💧",
				Title: "Wet Bulb Effect",
				Detail: fmt.Sprintf(
					"Precip detected, humidity %.0f%% (est. %s bias)",
					humidity, bias,
				),
			})
		}
	}

	// ── 3. Sensor Icing ─────────────────────────────────────────────────
	// Temp near 0°C + high humidity = moisture freezes on the sensor,
	// insulating it and causing it to read stale/incorrect values.
	{
		humidity := float64(0)
		if ext != nil {
			humidity = ext.RelativeHumidity
		}
		if humidity == 0 {
			humidity = estimateHumidity(obs.TempCelsius, obs.DewPointC)
		}

		tempNearZero := math.Abs(obs.TempCelsius) <= icingTempRange
		highHumidity := humidity >= icingHumidityThreshold

		if tempNearZero && highHumidity {
			detail := fmt.Sprintf(
				"Temp %.1f°C, humidity %.0f%% — sensor may freeze over",
				obs.TempCelsius, humidity,
			)

			// Additional risk factors.
			if hasFreezing(obs.PresentWeather) {
				detail += " (freezing precip reported)"
			}

			warnings = append(warnings, SensorWarning{
				Icon:   "❄️",
				Title:  "Sensor Icing Risk",
				Detail: detail,
			})
		}
	}

	// ── 4. Infrared Radiation Cooling (bonus check) ─────────────────────
	// Clear night + calm wind = sensor radiates heat to sky faster than
	// surrounding air, reading slightly colder.
	if ext != nil {
		windMS := KnotsToMS(obs.WindSpeed)
		isNight := ext.DirectRadiation == 0
		isClear := obs.Visibility == "CAVOK" ||
			obs.VisMeters >= 9999

		if isNight && isClear && windMS < 2.0 {
			warnings = append(warnings, SensorWarning{
				Icon:  "🌙",
				Title: "Radiative Cooling",
				Detail: fmt.Sprintf(
					"Clear night, wind %.1f m/s — sensor may read -0.5..1°C low",
					windMS,
				),
			})
		}
	}

	return warnings
}

// ═══════════════════════════════════════════════════════════════════════════
// Sensor bias helper functions
// ═══════════════════════════════════════════════════════════════════════════

// hasRainOrShowers checks if any precipitation phenomenon is present.
func hasRainOrShowers(wx []string) bool {
	for _, w := range wx {
		upper := strings.ToUpper(w)
		if strings.Contains(upper, "RA") ||
			strings.Contains(upper, "DZ") ||
			strings.Contains(upper, "SH") {
			return true
		}
	}
	return false
}

// hasFreezing checks for freezing precipitation (FZRA, FZDZ).
func hasFreezing(wx []string) bool {
	for _, w := range wx {
		upper := strings.ToUpper(w)
		if strings.Contains(upper, "FZ") {
			return true
		}
	}
	return false
}

// estimateHumidity approximates relative humidity from temperature and
// dew-point using the Magnus formula.
//
//	RH ≈ 100 × exp( (17.625 × Td) / (243.04 + Td) − (17.625 × T) / (243.04 + T) )
func estimateHumidity(tempC, dewPointC float64) float64 {
	const a = 17.625
	const b = 243.04

	gamma := (a * dewPointC) / (b + dewPointC) -
		(a * tempC) / (b + tempC)

	rh := 100.0 * math.Exp(gamma)

	if rh > 100 {
		rh = 100
	}
	if rh < 0 {
		rh = 0
	}

	return rh
}

// ═══════════════════════════════════════════════════════════════════════════
// Report.String — terminal rendering
// ═══════════════════════════════════════════════════════════════════════════

func (r *Report) String() string {
	var b strings.Builder
	b.Grow(1200)

	divider := "═══════════════════════════════════════════════════════════"

	b.WriteString(divider)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  %s — %s\n", r.Airport.ICAO, r.Airport.City)
	fmt.Fprintf(&b, "  Local : %s\n", r.LocalTime.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "  UTC   : %s\n", r.GeneratedAt.Format("2006-01-02 15:04:05Z"))
	b.WriteString(divider)
	b.WriteByte('\n')

	b.WriteString("\n  Observation (METAR)\n")
	fmt.Fprintf(&b, "    Temperature : %s\n", r.Observation.TempDisplay)
	fmt.Fprintf(&b, "    Wind        : %s\n", r.Observation.Wind)
	fmt.Fprintf(&b, "    Visibility  : %s\n", r.Observation.Visibility)

	b.WriteString("\n  Forecast (Open-Meteo)\n")
	if r.Forecast.Available {
		fmt.Fprintf(&b, "    Temperature : %s\n", r.Forecast.TempDisplay)
	} else {
		b.WriteString("    (not available)\n")
	}

	b.WriteString("\n  Forecast vs Reality\n")
	if r.Comparison.Available {
		fmt.Fprintf(&b, "    Delta       : %s (%s)\n",
			r.Comparison.DeltaDisplay, r.Comparison.Narrative)
		fmt.Fprintf(&b, "    Accuracy    : %s\n", r.Comparison.Accuracy)
	} else {
		b.WriteString("    (no forecast to compare)\n")
	}

	fmt.Fprintf(&b, "\n  Daily Extremes — %s\n", r.Extremes.DayDisplay)
	if math.IsInf(r.Extremes.HighC, 0) {
		b.WriteString("    (awaiting first observation)\n")
	} else {
		fmt.Fprintf(&b, "    High (ATH)  : %s\n", r.Extremes.HighDisplay)
		fmt.Fprintf(&b, "    Low  (ATL)  : %s\n", r.Extremes.LowDisplay)
	}

	b.WriteString("\n  Pressure\n")
	if r.Pressure.Available {
		fmt.Fprintf(&b, "    %s\n", r.Pressure.Display)
	} else {
		b.WriteString("    (no pressure data)\n")
	}

	if len(r.SensorWarnings) > 0 {
		b.WriteString("\n  ⚠️  Sensor QA\n")
		for _, w := range r.SensorWarnings {
			fmt.Fprintf(&b, "    %s %s: %s\n", w.Icon, w.Title, w.Detail)
		}
	}

	b.WriteString(divider)
	b.WriteByte('\n')
	return b.String()
}
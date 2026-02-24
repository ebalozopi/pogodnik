package monitor

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// Constants
// ═══════════════════════════════════════════════════════════════════════════

const (
	AccuracyExcellent = "Excellent"
	AccuracyGood      = "Good"
	AccuracyOff       = "Off"
	AccuracyPoor      = "Poor"
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
// Temperature formatting — precision-aware
// ═══════════════════════════════════════════════════════════════════════════

func tempSign(v float64) string {
	if v < 0 {
		return "" // negative sign is already included by %f
	}
	return "+"
}

// FormatTemp renders a temperature in both °C and °F.
func FormatTemp(c float64) string {
	if math.IsInf(c, 0) || math.IsNaN(c) {
		return "N/A"
	}
	f := CelsiusToFahrenheit(c)
	return fmt.Sprintf("%s%.1f°C / %s%.1f°F",
		tempSign(c), c, tempSign(f), f)
}

// FormatTempWithPrecision renders a temperature and appends a tilde (~)
// prefix when the reading is NOT from a T-Group (integer-only precision).
//
//	IsPrecise=true  → "+12.3°C / +54.1°F"
//	IsPrecise=false → "~+12.0°C / ~+54.0°F"
func FormatTempWithPrecision(c float64, isPrecise bool) string {
	if math.IsInf(c, 0) || math.IsNaN(c) {
		return "N/A"
	}
	f := CelsiusToFahrenheit(c)
	prefix := ""
	if !isPrecise {
		prefix = "~"
	}
	return fmt.Sprintf("%s%s%.1f°C / %s%s%.1f°F",
		prefix, tempSign(c), c, prefix, tempSign(f), f)
}

// FormatDelta renders a delta value with an explicit sign.
//
// Strict formula: Delta = Forecast − Reality
//
//	Positive → forecast was warmer than reality (warm bias)
//	Negative → forecast was cooler than reality (cool bias)
func FormatDelta(deltaC float64) string {
	deltaF := deltaC * 9.0 / 5.0
	return fmt.Sprintf("%+.1f°C / %+.1f°F", deltaC, deltaF)
}

// ═══════════════════════════════════════════════════════════════════════════
// Delta classification
// ═══════════════════════════════════════════════════════════════════════════

// ClassifyDelta returns a human-readable accuracy label based on |delta|.
func ClassifyDelta(delta float64) string {
	ad := math.Abs(delta)
	switch {
	case ad < 0.5:
		return AccuracyExcellent
	case ad < 1.0:
		return AccuracyGood
	case ad < 2.0:
		return AccuracyOff
	default:
		return AccuracyPoor
	}
}

// BiasNarrative returns the bias direction as a short label.
//
//	delta > 0  → "warm bias"  (forecast was too warm)
//	delta < 0  → "cool bias"  (forecast was too cool)
//	delta ≈ 0  → "neutral"
func BiasNarrative(delta float64) string {
	switch {
	case delta > 0.05:
		return "warm bias"
	case delta < -0.05:
		return "cool bias"
	default:
		return "neutral"
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Report sub-structures
// ═══════════════════════════════════════════════════════════════════════════

// ObservationReport holds the formatted observation data.
type ObservationReport struct {
	TempC       float64
	TempDisplay string
	IsPrecise   bool
	Wind        string
	Visibility  string
	RawMETAR    string
}

// ForecastReport holds the formatted forecast data.
type ForecastReport struct {
	Available   bool
	TempC       float64
	TempDisplay string
	ValidTime   time.Time
}

// ComparisonReport holds the delta analysis.
// Delta = Forecast − Reality (strict formula).
type ComparisonReport struct {
	Available    bool
	DeltaC       float64
	DeltaF       float64
	DeltaDisplay string // always shows sign: "+1.2°C / +2.2°F"
	Narrative    string // "warm bias", "cool bias", "neutral"
	Accuracy     string // "Excellent", "Good", "Off", "Poor"
}

// ExtremesReport holds daily high/low tracking.
type ExtremesReport struct {
	HighC       float64
	HighDisplay string
	LowC        float64
	LowDisplay  string
	TrackingDay time.Time
	DayDisplay  string
}

// PressureReport holds the barometric analysis.
type PressureReport struct {
	Available       bool
	CurrentHpa      float64
	Trend           string
	RatePerThreeHrs float64
	Display         string
}

// SensorWarning represents a single sensor bias warning.
type SensorWarning struct {
	Icon   string // emoji
	Title  string // short label
	Detail string // explanation
}

// Report is the fully assembled analysis for one airport.
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
// AnalyzeWeather — builds a Report from raw components
// ═══════════════════════════════════════════════════════════════════════════

// AnalyzeWeather constructs a full Report from an observation, optional
// forecast, state snapshot, and pressure tracker.
//
// Delta formula: Forecast.Temp − Reality.Temp
//
//	+2.0 means forecast predicted warmer than actual (warm bias)
//	-2.0 means forecast predicted cooler than actual (cool bias)
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

	// ── Observation ─────────────────────────────────────────────────────

	r.Observation = ObservationReport{
		TempC:       obs.TempCelsius,
		TempDisplay: FormatTempWithPrecision(obs.TempCelsius, obs.IsPrecise),
		IsPrecise:   obs.IsPrecise,
		Wind:        formatWindForReport(obs),
		Visibility:  obs.Visibility,
		RawMETAR:    obs.Raw,
	}

	// ── Forecast & Comparison ───────────────────────────────────────────

	if fc != nil {
		r.Forecast = ForecastReport{
			Available:   true,
			TempC:       fc.TempCelsius,
			TempDisplay: FormatTemp(fc.TempCelsius),
			ValidTime:   fc.Time,
		}

		// *** STRICT FORMULA: Delta = Forecast − Reality ***
		deltaC := fc.TempCelsius - obs.TempCelsius

		r.Comparison = ComparisonReport{
			Available:    true,
			DeltaC:       deltaC,
			DeltaF:       deltaC * 9.0 / 5.0,
			DeltaDisplay: FormatDelta(deltaC),
			Narrative:    BiasNarrative(deltaC),
			Accuracy:     ClassifyDelta(deltaC),
		}
	}

	// ── Daily Extremes ──────────────────────────────────────────────────

	r.Extremes = ExtremesReport{
		HighC:       state.DailyHigh,
		HighDisplay: FormatTemp(state.DailyHigh),
		LowC:       state.DailyLow,
		LowDisplay:  FormatTemp(state.DailyLow),
		TrackingDay: state.TrackingDay,
		DayDisplay: fmt.Sprintf("%s (%s)",
			state.TrackingDay.Format("2006-01-02"), apt.Timezone),
	}

	// ── Pressure ────────────────────────────────────────────────────────

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
	// Rain/showers + relatively low humidity = evaporation cools the
	// sensor below true air temperature. Typical bias: -0.5 to -1.5 °C.
	if hasRainOrShowers(obs.PresentWeather) {
		humidity := float64(0)
		if ext != nil {
			humidity = ext.RelativeHumidity
		}
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
	// Temp near 0°C + high humidity = moisture freezes on the sensor.
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

	// ── 4. Infrared Radiation Cooling ───────────────────────────────────
	// Clear night + calm wind = sensor radiates heat faster than
	// surrounding air, reading slightly colder.
	if ext != nil {
		windMS := KnotsToMS(obs.WindSpeed)
		isNight := ext.DirectRadiation == 0
		isClear := obs.Visibility == "CAVOK" || obs.VisMeters >= 9999

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
// Report helpers
// ═══════════════════════════════════════════════════════════════════════════

func formatWindForReport(obs *Observation) string {
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

// ═══════════════════════════════════════════════════════════════════════════
// Report.String — terminal/log rendering
// ═══════════════════════════════════════════════════════════════════════════

func (r *Report) String() string {
	var b strings.Builder
	b.Grow(1400)

	divider := "═══════════════════════════════════════════════════════════"

	b.WriteString(divider)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  %s — %s\n", r.Airport.ICAO, r.Airport.City)
	fmt.Fprintf(&b, "  Local : %s\n", r.LocalTime.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "  UTC   : %s\n", r.GeneratedAt.Format("2006-01-02 15:04:05Z"))
	b.WriteString(divider)
	b.WriteByte('\n')

	b.WriteString("\n  Observation (METAR)\n")
	precLabel := "T-Group 0.1°C"
	if !r.Observation.IsPrecise {
		precLabel = "standard ±1°C"
	}
	fmt.Fprintf(&b, "    Temperature : %s [%s]\n", r.Observation.TempDisplay, precLabel)
	fmt.Fprintf(&b, "    Wind        : %s\n", r.Observation.Wind)
	fmt.Fprintf(&b, "    Visibility  : %s\n", r.Observation.Visibility)

	b.WriteString("\n  Forecast (Open-Meteo)\n")
	if r.Forecast.Available {
		fmt.Fprintf(&b, "    Temperature : %s\n", r.Forecast.TempDisplay)
	} else {
		b.WriteString("    (not available)\n")
	}

	b.WriteString("\n  Forecast vs Reality [Delta = Forecast − Reality]\n")
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

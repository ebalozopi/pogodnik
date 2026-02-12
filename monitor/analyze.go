package monitor

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	AccuracyAccurate = "Accurate"
	AccuracyClose    = "Close"
	AccuracyOff      = "Off"
	AccuracyPoor     = "Poor"
)

var (
	reAltimeterUS  = regexp.MustCompile(`\bA(\d{4})\b`)
	reAltimeterQNH = regexp.MustCompile(`\bQ(\d{3,4})\b`)
)

const inHgToHpa = 33.8639

// ParsePressure extracts altimeter/QNH from raw METAR.
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

func tempSign(v float64) string {
	if v < 0 {
		return ""
	}
	return "+"
}

// FormatTemp renders temperature in both C and F.
func FormatTemp(c float64) string {
	if math.IsInf(c, 0) || math.IsNaN(c) {
		return "N/A"
	}
	f := CelsiusToFahrenheit(c)
	return fmt.Sprintf("%s%.1f°C / %s%.1f°F", tempSign(c), c, tempSign(f), f)
}

// FormatDelta renders a temperature difference.
func FormatDelta(deltaC float64) string {
	deltaF := deltaC * 9.0 / 5.0
	return fmt.Sprintf("%s%.1f°C / %s%.1f°F",
		tempSign(deltaC), deltaC,
		tempSign(deltaF), deltaF,
	)
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

func deltaNarrative(deltaC float64) string {
	switch {
	case deltaC > 0.05:
		return "warmer than forecast"
	case deltaC < -0.05:
		return "cooler than forecast"
	default:
		return "matches forecast"
	}
}

func formatWind(obs *Observation) string {
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

// ObservationReport summarizes current observation.
type ObservationReport struct {
	TempC       float64
	TempDisplay string
	Wind        string
	Visibility  string
	RawMETAR    string
}

// ForecastReport summarizes predicted temperature.
type ForecastReport struct {
	Available   bool
	TempC       float64
	TempDisplay string
	ValidTime   time.Time
}

// ComparisonReport is forecast-vs-reality analysis.
type ComparisonReport struct {
	Available    bool
	DeltaC       float64
	DeltaF       float64
	DeltaDisplay string
	Narrative    string
	Accuracy     string
}

// ExtremesReport captures daily high/low.
type ExtremesReport struct {
	HighC       float64
	HighDisplay string
	LowC        float64
	LowDisplay  string
	TrackingDay time.Time
	DayDisplay  string
}

// PressureReport describes current QNH and trend.
type PressureReport struct {
	Available       bool
	CurrentHpa      float64
	Trend           string
	RatePerThreeHrs float64
	Display         string
}

// Report is the output of AnalyzeWeather.
type Report struct {
	Airport     Airport
	GeneratedAt time.Time
	LocalTime   time.Time

	Observation ObservationReport
	Forecast    ForecastReport
	Comparison  ComparisonReport
	Extremes    ExtremesReport
	Pressure    PressureReport
}

// AnalyzeWeather combines observation, forecast, state, and pressure.
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
		Wind:        formatWind(obs),
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
			Narrative:    deltaNarrative(deltaC),
			Accuracy:     classifyAccuracy(math.Abs(deltaC)),
		}
	}

	r.Extremes = ExtremesReport{
		HighC:       state.DailyHigh,
		HighDisplay: FormatTemp(state.DailyHigh),
		LowC:        state.DailyLow,
		LowDisplay:  FormatTemp(state.DailyLow),
		TrackingDay: state.TrackingDay,
		DayDisplay: fmt.Sprintf(
			"%s (%s)",
			state.TrackingDay.Format("2006-01-02"),
			apt.Timezone,
		),
	}

	if pt != nil {
		latest, ok := pt.Latest()
		if ok {
			rate, trend := pt.Trend()
			var display string
			if trend == "Unknown" {
				display = fmt.Sprintf("%.1f hPa — trend data insufficient", latest.Hpa)
			} else {
				display = fmt.Sprintf("%.1f hPa — %s (%+.1f hPa/3hr)", latest.Hpa, trend, rate)
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

// String renders the report as text.
func (r *Report) String() string {
	var b strings.Builder
	b.Grow(1024)

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
		fmt.Fprintf(&b, "    Valid for   : %s\n", r.Forecast.ValidTime.Format("15:04 MST"))
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
	if math.IsInf(r.Extremes.HighC, 0) || math.IsInf(r.Extremes.LowC, 0) {
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

	b.WriteString(divider)
	b.WriteByte('\n')
	return b.String()
}
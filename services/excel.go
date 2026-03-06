package services

import (
	"fmt"
	"math"
	"sort"
	"time"

	"pogodnik/storage"

	"github.com/xuri/excelize/v2"
)

// ═══════════════════════════════════════════════════════════════════════════
// GenerateReport — multi-sheet Excel with bias analytics
//
// Accepts ALL logs for the reporting window (30 days, no row limit)
// and the full airport list for timezone lookups.
// ═══════════════════════════════════════════════════════════════════════════

func GenerateReport(logs []storage.WeatherLog, airports []storage.Airport) (*excelize.File, error) {
	f := excelize.NewFile()

	styles, err := createStyles(f)
	if err != nil {
		return nil, err
	}

	tzMap := buildTimezoneMap(airports)

	if err := writeRawDataSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: raw data: %w", err)
	}

	if err := writeBiasSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: bias: %w", err)
	}

	if err := writeDailySheet(f, styles, logs, tzMap); err != nil {
		return nil, fmt.Errorf("excel: daily: %w", err)
	}

	if err := writeATHSheet(f, styles, logs, tzMap); err != nil {
		return nil, fmt.Errorf("excel: ATH: %w", err)
	}

	f.DeleteSheet("Sheet1")
	f.SetActiveSheet(0)

	return f, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Timezone helpers
// ═══════════════════════════════════════════════════════════════════════════

// buildTimezoneMap creates a lookup from ICAO → *time.Location.
func buildTimezoneMap(airports []storage.Airport) map[string]*time.Location {
	m := make(map[string]*time.Location, len(airports))
	for _, a := range airports {
		loc, err := time.LoadLocation(a.Timezone)
		if err != nil {
			loc = time.UTC
		}
		m[a.ICAO] = loc
	}
	return m
}

// localDateForLog converts a log's UTC timestamp to the airport's local
// date string (YYYY-MM-DD).
func localDateForLog(l storage.WeatherLog, tzMap map[string]*time.Location) string {
	loc, ok := tzMap[l.AirportICAO]
	if !ok {
		loc = time.UTC
	}
	return l.Timestamp.In(loc).Format("2006-01-02")
}

// ═══════════════════════════════════════════════════════════════════════════
// Styles
// ═══════════════════════════════════════════════════════════════════════════

type reportStyles struct {
	header    int
	number    int
	pct       int
	green     int
	yellow    int
	red       int
	bold      int
	dateCell  int
	boolTrue  int
	boolFalse int
}

func createStyles(f *excelize.File) (*reportStyles, error) {
	s := &reportStyles{}
	var err error

	s.header, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 11, Color: "#FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#2B5797"}},
		Alignment: &excelize.Alignment{
			Horizontal: "center", Vertical: "center", WrapText: true,
		},
		Border: []excelize.Border{{Type: "bottom", Color: "#000000", Style: 2}},
	})
	if err != nil {
		return nil, err
	}

	s.number, err = f.NewStyle(&excelize.Style{
		NumFmt:    2,
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.pct, err = f.NewStyle(&excelize.Style{
		NumFmt:    10,
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.green, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#006100"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#C6EFCE"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.yellow, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#9C6500"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#FFEB9C"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.red, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#9C0006", Bold: true},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#FFC7CE"}},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.bold, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 11},
	})
	if err != nil {
		return nil, err
	}

	s.dateCell, err = f.NewStyle(&excelize.Style{
		NumFmt:    22,
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.boolTrue, err = f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Color: "#0000FF"},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.boolFalse, err = f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Color: "#808080"},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 1: Raw Data (all logs, no limit)
// ═══════════════════════════════════════════════════════════════════════════

func writeRawDataSheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Raw Data (30 Days)"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	headers := []string{
		"Date", "Time (UTC)", "Airport",
		"Forecast (°C)", "Reality (°C)", "Delta (°C)",
		"|Delta|", "Verdict",
		"Wind (m/s)", "Solar (W/m²)",
		"Rain", "Fog", "SPECI",
		"Fc Daily High", "Fc Tomorrow High",
	}
	widths := []float64{12, 10, 8, 14, 14, 12, 10, 12, 12, 14, 6, 6, 6, 14, 16}

	for i, h := range headers {
		c := cell(i+1, 1)
		_ = f.SetCellValue(sheet, c, h)
		_ = f.SetCellStyle(sheet, c, c, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	for rowIdx, l := range logs {
		row := rowIdx + 2

		ad := math.Abs(l.Delta)
		verdict := classifyAbsDelta(ad)

		vals := []interface{}{
			l.Timestamp.UTC().Format("2006-01-02"),
			l.Timestamp.UTC().Format("15:04:05"),
			l.AirportICAO,
			l.ForecastTemp,
			l.RealTemp,
			l.Delta,
			ad,
			verdict,
			l.WindSpeed,
			l.DirectRadiation,
			boolStr(l.IsRaining),
			boolStr(l.IsFoggy),
			boolStr(l.IsSpeci),
			l.ForecastDailyHigh,
			l.ForecastNextHigh,
		}

		for col, v := range vals {
			c := cell(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			// Number formatting.
			if (col >= 3 && col <= 6) || col == 8 || col == 9 || col == 13 || col == 14 {
				_ = f.SetCellStyle(sheet, c, c, s.number)
			}

			// Boolean formatting.
			if col >= 10 && col <= 12 {
				if v == "YES" {
					_ = f.SetCellStyle(sheet, c, c, s.boolTrue)
				} else {
					_ = f.SetCellStyle(sheet, c, c, s.boolFalse)
				}
			}
		}

		if ad > 1.0 {
			for col := 0; col < len(vals); col++ {
				c := cell(col+1, row)
				_ = f.SetCellStyle(sheet, c, c, s.red)
			}
		}
	}

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 2: Bias Analysis (per airport)
// ═══════════════════════════════════════════════════════════════════════════

func writeBiasSheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Bias Analysis"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	byAirport := make(map[string][]storage.WeatherLog)
	var icaoOrder []string

	for _, l := range logs {
		if _, exists := byAirport[l.AirportICAO]; !exists {
			icaoOrder = append(icaoOrder, l.AirportICAO)
		}
		byAirport[l.AirportICAO] = append(byAirport[l.AirportICAO], l)
	}

	headers := []string{
		"Airport",
		"Observations",
		"Avg Bias (°C)",
		"Excellent\n(< 0.5°C)",
		"Good\n(< 1.0°C)",
		"Critical\n(> 1.0°C)",
		"Extreme\n(> 2.0°C)",
		"Solar Avg Δ\n(rad>400)",
		"Solar N",
		"Rain Avg Δ",
		"Rain N",
		"Assessment",
	}
	widths := []float64{10, 14, 14, 14, 14, 14, 14, 14, 10, 14, 10, 16}

	for i, h := range headers {
		c := cell(i+1, 1)
		_ = f.SetCellValue(sheet, c, h)
		_ = f.SetCellStyle(sheet, c, c, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	row := 2
	for _, icao := range icaoOrder {
		stats := storage.ComputeBiasStats(icao, byAirport[icao])
		assessment := assessBias(stats)

		vals := []interface{}{
			stats.ICAO,
			stats.Count,
			stats.AvgBias,
			stats.Excellent / 100,
			stats.Good / 100,
			stats.Critical / 100,
			stats.Extreme / 100,
			stats.SolarAvg,
			stats.SolarN,
			stats.RainAvg,
			stats.RainN,
			assessment,
		}

		for col, v := range vals {
			c := cell(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			switch col {
			case 2, 7, 9:
				_ = f.SetCellStyle(sheet, c, c, s.number)
			case 3, 4, 5, 6:
				_ = f.SetCellStyle(sheet, c, c, s.pct)
			}
		}

		assessCell := cell(12, row)
		switch assessment {
		case "EXCELLENT":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.green)
		case "GOOD":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.red)
		}

		biasCell := cell(3, row)
		switch ab := math.Abs(stats.AvgBias); {
		case ab > 1.0:
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.red)
		case ab > 0.5:
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.green)
		}

		row++
	}

	// ── Legend ───────────────────────────────────────────────────────────

	legendRow := row + 2
	legends := []struct{ label, desc string }{
		{"Avg Bias", "Mean(Reality − Forecast). Positive = Reality warmer than forecast."},
		{"Excellent", "% of observations within ±0.5°C."},
		{"Good", "% of observations within ±1.0°C."},
		{"Critical", "% of observations with |error| > 1.0°C."},
		{"Extreme", "% of observations with |error| > 2.0°C."},
		{"Solar Avg Δ", "Avg delta when Direct Radiation > 400 W/m². Tests sensor heating."},
		{"Rain Avg Δ", "Avg delta when rain detected. Tests evaporative cooling."},
	}

	_ = f.SetCellValue(sheet, cell(1, legendRow), "LEGEND")
	_ = f.SetCellStyle(sheet, cell(1, legendRow), cell(1, legendRow), s.bold)

	for i, lg := range legends {
		r := legendRow + i + 1
		_ = f.SetCellValue(sheet, cell(1, r), lg.label)
		_ = f.SetCellStyle(sheet, cell(1, r), cell(1, r), s.bold)
		_ = f.SetCellValue(sheet, cell(2, r), lg.desc)
	}

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 3: Daily Summary (grouped by Local Date + Airport)
// ═══════════════════════════════════════════════════════════════════════════
//
// Timestamps are converted to each airport's local timezone before
// extracting the date. This ensures a log at 2024-01-15 23:30 UTC for
// RKSI (Asia/Seoul, UTC+9) is grouped under 2024-01-16 (local).

func writeDailySheet(
	f *excelize.File,
	s *reportStyles,
	logs []storage.WeatherLog,
	tzMap map[string]*time.Location,
) error {
	sheet := "Daily Summary"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	summaries := computeDailyAirportSummaries(logs, tzMap)

	headers := []string{
		"Local Date",
		"Airport",
		"Avg Delta (°C)",
		"ATH (°C)",
		"ATL (°C)",
		"Range (ATH−ATL)",
		"Log Count",
	}
	widths := []float64{14, 10, 16, 12, 12, 16, 12}

	for i, h := range headers {
		c := cell(i+1, 1)
		_ = f.SetCellValue(sheet, c, h)
		_ = f.SetCellStyle(sheet, c, c, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	for rowIdx, ds := range summaries {
		row := rowIdx + 2

		vals := []interface{}{
			ds.Date,
			ds.ICAO,
			ds.AvgDelta,
			ds.ATH,
			ds.ATL,
			ds.DiurnalRange,
			ds.Count,
		}

		for col, v := range vals {
			c := cell(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			if col >= 2 && col <= 5 {
				_ = f.SetCellStyle(sheet, c, c, s.number)
			}
		}

		// Colour-code Avg Delta.
		deltaCell := cell(3, row)
		switch ad := math.Abs(ds.AvgDelta); {
		case ad > 1.0:
			_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.red)
		case ad > 0.5:
			_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.green)
		}

		// Colour-code large diurnal range.
		if ds.DiurnalRange > 15.0 {
			rangeCell := cell(6, row)
			_ = f.SetCellStyle(sheet, rangeCell, rangeCell, s.yellow)
		}

		// Highlight row red when avg |delta| > 1.0.
		if math.Abs(ds.AvgDelta) > 1.0 {
			for col := 0; col < len(vals); col++ {
				c := cell(col+1, row)
				_ = f.SetCellStyle(sheet, c, c, s.red)
			}
		}
	}

	// ── 30-day per-airport aggregate footer ──────────────────────────────

	if len(summaries) > 0 {
		aggRow := len(summaries) + 3

		type airportAgg struct {
			sumDelta float64
			maxTemp  float64
			minTemp  float64
			count    int
		}

		byAirport := make(map[string]*airportAgg)
		var airportOrder []string

		for _, ds := range summaries {
			aa, exists := byAirport[ds.ICAO]
			if !exists {
				aa = &airportAgg{maxTemp: ds.ATH, minTemp: ds.ATL}
				byAirport[ds.ICAO] = aa
				airportOrder = append(airportOrder, ds.ICAO)
			}
			aa.sumDelta += ds.AvgDelta * float64(ds.Count)
			aa.count += ds.Count
			if ds.ATH > aa.maxTemp {
				aa.maxTemp = ds.ATH
			}
			if ds.ATL < aa.minTemp {
				aa.minTemp = ds.ATL
			}
		}

		_ = f.SetCellValue(sheet, cell(1, aggRow), "30-DAY TOTALS")
		_ = f.SetCellStyle(sheet, cell(1, aggRow), cell(1, aggRow), s.bold)
		aggRow++

		for _, icao := range airportOrder {
			aa := byAirport[icao]
			avgDelta := aa.sumDelta / float64(aa.count)
			diurnal := aa.maxTemp - aa.minTemp

			_ = f.SetCellValue(sheet, cell(1, aggRow), "TOTAL")
			_ = f.SetCellStyle(sheet, cell(1, aggRow), cell(1, aggRow), s.bold)
			_ = f.SetCellValue(sheet, cell(2, aggRow), icao)
			_ = f.SetCellStyle(sheet, cell(2, aggRow), cell(2, aggRow), s.bold)
			_ = f.SetCellValue(sheet, cell(3, aggRow), avgDelta)
			_ = f.SetCellStyle(sheet, cell(3, aggRow), cell(3, aggRow), s.number)
			_ = f.SetCellValue(sheet, cell(4, aggRow), aa.maxTemp)
			_ = f.SetCellStyle(sheet, cell(4, aggRow), cell(4, aggRow), s.number)
			_ = f.SetCellValue(sheet, cell(5, aggRow), aa.minTemp)
			_ = f.SetCellStyle(sheet, cell(5, aggRow), cell(5, aggRow), s.number)
			_ = f.SetCellValue(sheet, cell(6, aggRow), diurnal)
			_ = f.SetCellStyle(sheet, cell(6, aggRow), cell(6, aggRow), s.number)
			_ = f.SetCellValue(sheet, cell(7, aggRow), aa.count)

			switch ad := math.Abs(avgDelta); {
			case ad > 1.0:
				_ = f.SetCellStyle(sheet, cell(3, aggRow), cell(3, aggRow), s.red)
			case ad > 0.5:
				_ = f.SetCellStyle(sheet, cell(3, aggRow), cell(3, aggRow), s.yellow)
			default:
				_ = f.SetCellStyle(sheet, cell(3, aggRow), cell(3, aggRow), s.green)
			}

			aggRow++
		}
	}

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 4: ATH Analysis (Daily High Forecast Accuracy)
// ═══════════════════════════════════════════════════════════════════════════
//
// Compares the real observed daily high (max RealTemp) against the
// forecast daily high (ForecastDailyHigh from Open-Meteo Daily arrays).
//
// Grouped by Local Date + Airport with per-airport bias summary.

func writeATHSheet(
	f *excelize.File,
	s *reportStyles,
	logs []storage.WeatherLog,
	tzMap map[string]*time.Location,
) error {
	sheet := "ATH Analysis"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	summaries := computeATHSummaries(logs, tzMap)

	headers := []string{
		"Local Date",
		"Airport",
		"Real ATH (°C)",
		"Forecast ATH (°C)",
		"Delta (°C)",
		"Fc Tomorrow ATH (°C)",
	}
	widths := []float64{14, 10, 14, 16, 12, 18}

	for i, h := range headers {
		c := cell(i+1, 1)
		_ = f.SetCellValue(sheet, c, h)
		_ = f.SetCellStyle(sheet, c, c, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	for rowIdx, as := range summaries {
		row := rowIdx + 2

		fcATHDisplay := as.ForecastATH
		deltaDisplay := as.Delta
		fcTomorrowDisplay := as.ForecastTomorrow

		vals := []interface{}{
			as.LocalDate,
			as.ICAO,
			as.RealATH,
			fcATHDisplay,
			deltaDisplay,
			fcTomorrowDisplay,
		}

		for col, v := range vals {
			c := cell(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			if col >= 2 && col <= 5 {
				_ = f.SetCellStyle(sheet, c, c, s.number)
			}
		}

		// Colour-code delta.
		if as.HasForecast {
			deltaCell := cell(5, row)
			switch ad := math.Abs(as.Delta); {
			case ad > 2.0:
				_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.red)
			case ad > 1.0:
				_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.yellow)
			default:
				_ = f.SetCellStyle(sheet, deltaCell, deltaCell, s.green)
			}
		}

		// Mark rows without forecast data.
		if !as.HasForecast {
			for col := 3; col <= 5; col++ {
				c := cell(col+1, row)
				_ = f.SetCellValue(sheet, c, "N/A")
				_ = f.SetCellStyle(sheet, c, c, s.boolFalse)
			}
		}
	}

	// ── ATH Bias Summary (per airport) ──────────────────────────────────

	if len(summaries) > 0 {
		biasRow := len(summaries) + 3

		_ = f.SetCellValue(sheet, cell(1, biasRow), "ATH BIAS SUMMARY")
		_ = f.SetCellStyle(sheet, cell(1, biasRow), cell(1, biasRow), s.bold)
		biasRow++

		biasHeaders := []string{"Airport", "Days Analyzed", "Avg ATH Delta (°C)", "Assessment"}
		for i, h := range biasHeaders {
			c := cell(i+1, biasRow)
			_ = f.SetCellValue(sheet, c, h)
			_ = f.SetCellStyle(sheet, c, c, s.header)
		}
		biasRow++

		// Compute per-airport ATH bias.
		type athBiasAcc struct {
			sumDelta float64
			count    int
		}

		biasMap := make(map[string]*athBiasAcc)
		var biasOrder []string

		for _, as := range summaries {
			if !as.HasForecast {
				continue
			}
			acc, exists := biasMap[as.ICAO]
			if !exists {
				acc = &athBiasAcc{}
				biasMap[as.ICAO] = acc
				biasOrder = append(biasOrder, as.ICAO)
			}
			acc.sumDelta += as.Delta
			acc.count++
		}

		sort.Strings(biasOrder)

		for _, icao := range biasOrder {
			acc := biasMap[icao]
			avgDelta := acc.sumDelta / float64(acc.count)

			assessment := "EXCELLENT"
			switch ad := math.Abs(avgDelta); {
			case ad > 2.0:
				assessment = "NEEDS REVIEW"
			case ad > 1.0:
				assessment = "ACCEPTABLE"
			case ad > 0.5:
				assessment = "GOOD"
			}

			_ = f.SetCellValue(sheet, cell(1, biasRow), icao)
			_ = f.SetCellStyle(sheet, cell(1, biasRow), cell(1, biasRow), s.bold)
			_ = f.SetCellValue(sheet, cell(2, biasRow), acc.count)
			_ = f.SetCellValue(sheet, cell(3, biasRow), avgDelta)
			_ = f.SetCellStyle(sheet, cell(3, biasRow), cell(3, biasRow), s.number)
			_ = f.SetCellValue(sheet, cell(4, biasRow), assessment)

			assessCell := cell(4, biasRow)
			switch assessment {
			case "EXCELLENT":
				_ = f.SetCellStyle(sheet, assessCell, assessCell, s.green)
			case "GOOD":
				_ = f.SetCellStyle(sheet, assessCell, assessCell, s.yellow)
			default:
				_ = f.SetCellStyle(sheet, assessCell, assessCell, s.red)
			}

			// Colour-code the avg delta.
			switch ad := math.Abs(avgDelta); {
			case ad > 1.0:
				_ = f.SetCellStyle(sheet, cell(3, biasRow), cell(3, biasRow), s.red)
			case ad > 0.5:
				_ = f.SetCellStyle(sheet, cell(3, biasRow), cell(3, biasRow), s.yellow)
			default:
				_ = f.SetCellStyle(sheet, cell(3, biasRow), cell(3, biasRow), s.green)
			}

			biasRow++
		}

		// Legend.
		biasRow++
		_ = f.SetCellValue(sheet, cell(1, biasRow), "LEGEND")
		_ = f.SetCellStyle(sheet, cell(1, biasRow), cell(1, biasRow), s.bold)
		biasRow++
		athLegends := []struct{ label, desc string }{
			{"Real ATH", "Maximum RealTemp observed on that local day."},
			{"Forecast ATH", "Predicted daily high from Open-Meteo (Daily.Temp2mMax[0])."},
			{"Delta", "Real ATH − Forecast ATH. Positive = reality warmer than predicted."},
			{"Fc Tomorrow ATH", "Predicted next-day high (Daily.Temp2mMax[1])."},
			{"Avg ATH Delta", "Mean of daily (Real ATH − Forecast ATH). Shows systematic bias."},
		}

		for _, lg := range athLegends {
			_ = f.SetCellValue(sheet, cell(1, biasRow), lg.label)
			_ = f.SetCellStyle(sheet, cell(1, biasRow), cell(1, biasRow), s.bold)
			_ = f.SetCellValue(sheet, cell(2, biasRow), lg.desc)
			biasRow++
		}
	}

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Daily airport summary computation (timezone-aware)
// ═══════════════════════════════════════════════════════════════════════════

type dailyAirportSummary struct {
	Date         string  // local date YYYY-MM-DD
	ICAO         string
	AvgDelta     float64 // mean(Reality − Forecast)
	ATH          float64 // max RealTemp that local day
	ATL          float64 // min RealTemp that local day
	DiurnalRange float64 // ATH − ATL
	Count        int
}

func computeDailyAirportSummaries(
	logs []storage.WeatherLog,
	tzMap map[string]*time.Location,
) []dailyAirportSummary {

	type groupKey struct {
		date string
		icao string
	}

	type accumulator struct {
		sumDelta float64
		maxTemp  float64
		minTemp  float64
		count    int
	}

	groups := make(map[groupKey]*accumulator)
	var keyOrder []groupKey

	for _, l := range logs {
		localDay := localDateForLog(l, tzMap)
		k := groupKey{date: localDay, icao: l.AirportICAO}

		acc, exists := groups[k]
		if !exists {
			acc = &accumulator{
				maxTemp: l.RealTemp,
				minTemp: l.RealTemp,
			}
			groups[k] = acc
			keyOrder = append(keyOrder, k)
		}

		acc.sumDelta += l.Delta
		acc.count++

		if l.RealTemp > acc.maxTemp {
			acc.maxTemp = l.RealTemp
		}
		if l.RealTemp < acc.minTemp {
			acc.minTemp = l.RealTemp
		}
	}

	sort.Slice(keyOrder, func(i, j int) bool {
		if keyOrder[i].date != keyOrder[j].date {
			return keyOrder[i].date < keyOrder[j].date
		}
		return keyOrder[i].icao < keyOrder[j].icao
	})

	result := make([]dailyAirportSummary, 0, len(keyOrder))
	for _, k := range keyOrder {
		acc := groups[k]
		avg := acc.sumDelta / float64(acc.count)
		result = append(result, dailyAirportSummary{
			Date:         k.date,
			ICAO:         k.icao,
			AvgDelta:     avg,
			ATH:          acc.maxTemp,
			ATL:          acc.minTemp,
			DiurnalRange: acc.maxTemp - acc.minTemp,
			Count:        acc.count,
		})
	}

	return result
}

// ═══════════════════════════════════════════════════════════════════════════
// ATH summary computation (timezone-aware)
// ═══════════════════════════════════════════════════════════════════════════

type athSummary struct {
	LocalDate        string
	ICAO             string
	RealATH          float64 // max RealTemp on that local day
	ForecastATH      float64 // from ForecastDailyHigh
	Delta            float64 // RealATH − ForecastATH
	ForecastTomorrow float64 // from ForecastNextHigh
	HasForecast      bool    // true if ForecastDailyHigh was populated
}

func computeATHSummaries(
	logs []storage.WeatherLog,
	tzMap map[string]*time.Location,
) []athSummary {

	type groupKey struct {
		date string
		icao string
	}

	type accumulator struct {
		maxTemp float64

		// We collect forecast values and pick the best non-zero one.
		// Values should be consistent within a day but may be zero
		// if the forecast wasn't available for a particular log.
		bestFcHigh    float64
		bestFcNext    float64
		hasFcHigh     bool
		hasFcNext     bool
	}

	groups := make(map[groupKey]*accumulator)
	var keyOrder []groupKey

	for _, l := range logs {
		localDay := localDateForLog(l, tzMap)
		k := groupKey{date: localDay, icao: l.AirportICAO}

		acc, exists := groups[k]
		if !exists {
			acc = &accumulator{
				maxTemp: l.RealTemp,
			}
			groups[k] = acc
			keyOrder = append(keyOrder, k)
		}

		if l.RealTemp > acc.maxTemp {
			acc.maxTemp = l.RealTemp
		}

		// Pick the latest non-zero forecast daily high.
		// Within a day these should be consistent, but if cache
		// refreshes mid-day the forecast might update slightly.
		if l.ForecastDailyHigh != 0 || l.Condition != "NoForecast" {
			acc.bestFcHigh = l.ForecastDailyHigh
			acc.hasFcHigh = true
		}

		if l.ForecastNextHigh != 0 || l.Condition != "NoForecast" {
			acc.bestFcNext = l.ForecastNextHigh
			acc.hasFcNext = true
		}
	}

	sort.Slice(keyOrder, func(i, j int) bool {
		if keyOrder[i].date != keyOrder[j].date {
			return keyOrder[i].date < keyOrder[j].date
		}
		return keyOrder[i].icao < keyOrder[j].icao
	})

	result := make([]athSummary, 0, len(keyOrder))
	for _, k := range keyOrder {
		acc := groups[k]

		var delta float64
		if acc.hasFcHigh {
			delta = acc.maxTemp - acc.bestFcHigh
		}

		result = append(result, athSummary{
			LocalDate:        k.date,
			ICAO:             k.icao,
			RealATH:          acc.maxTemp,
			ForecastATH:      acc.bestFcHigh,
			Delta:            delta,
			ForecastTomorrow: acc.bestFcNext,
			HasForecast:      acc.hasFcHigh,
		})
	}

	return result
}

// ═══════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════

func cell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}

func boolStr(v bool) string {
	if v {
		return "YES"
	}
	return "—"
}

func classifyAbsDelta(ad float64) string {
	switch {
	case ad < 0.5:
		return "Excellent"
	case ad < 1.0:
		return "Good"
	case ad < 2.0:
		return "Off"
	default:
		return "Poor"
	}
}

func assessBias(s storage.BiasStats) string {
	switch {
	case s.Excellent >= 70 && math.Abs(s.AvgBias) < 0.3:
		return "EXCELLENT"
	case s.Good >= 70 && math.Abs(s.AvgBias) < 0.7:
		return "GOOD"
	case s.Critical > 30:
		return "NEEDS REVIEW"
	default:
		return "ACCEPTABLE"
	}
}

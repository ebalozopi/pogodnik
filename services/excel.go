package services

import (
	"fmt"
	"math"
	"sort"

	"pogodnik/storage"

	"github.com/xuri/excelize/v2"
)

// ═══════════════════════════════════════════════════════════════════════════
// GenerateReport — multi-sheet Excel with bias analytics
//
// Expects ALL logs for the reporting window (typically 30 days).
// The caller must NOT apply any row limits.
// ═══════════════════════════════════════════════════════════════════════════

func GenerateReport(logs []storage.WeatherLog) (*excelize.File, error) {
	f := excelize.NewFile()

	styles, err := createStyles(f)
	if err != nil {
		return nil, err
	}

	if err := writeRawDataSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: raw data: %w", err)
	}

	if err := writeBiasSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: bias: %w", err)
	}

	if err := writeDailySheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: daily: %w", err)
	}

	f.DeleteSheet("Sheet1")
	f.SetActiveSheet(0)

	return f, nil
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
	}
	widths := []float64{12, 10, 8, 14, 14, 12, 10, 12, 12, 14, 6, 6, 6}

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
			l.Timestamp.UTC().Format("2006-01-02"),  // Date
			l.Timestamp.UTC().Format("15:04:05"),    // Time
			l.AirportICAO,                           // Airport
			l.ForecastTemp,                          // Forecast
			l.RealTemp,                              // Reality
			l.Delta,                                 // Delta (Reality − Forecast)
			ad,                                      // |Delta|
			verdict,                                 // Verdict
			l.WindSpeed,                             // Wind m/s
			l.DirectRadiation,                       // Solar W/m²
			boolStr(l.IsRaining),                    // Rain
			boolStr(l.IsFoggy),                      // Fog
			boolStr(l.IsSpeci),                      // SPECI
		}

		for col, v := range vals {
			c := cell(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			// Number formatting for numeric columns.
			if col >= 3 && col <= 6 || col == 8 || col == 9 {
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

		// Highlight entire row red when |delta| > 1.0.
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

	// Group logs by airport, preserving encounter order.
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

		// Colour-code assessment.
		assessCell := cell(12, row)
		switch assessment {
		case "EXCELLENT":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.green)
		case "GOOD":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.red)
		}

		// Colour-code bias magnitude.
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
// Sheet 3: Daily Summary (grouped by Date + Airport)
// ═══════════════════════════════════════════════════════════════════════════
//
// Each row = one airport on one day.
// Columns: Date | Airport | Avg Delta | ATH | ATL | Range (ATH−ATL) | Log Count

func writeDailySheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Daily Summary"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	summaries := computeDailyAirportSummaries(logs)

	headers := []string{
		"Date",
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

	// Track previous date to add visual separation via alternating styling.
	prevDate := ""

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

			// Number formatting for temp columns.
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

		// Colour-code large diurnal range (>15°C is notable).
		rangeCell := cell(6, row)
		if ds.DiurnalRange > 15.0 {
			_ = f.SetCellStyle(sheet, rangeCell, rangeCell, s.yellow)
		}

		// Highlight row red when avg |delta| > 1.0.
		if math.Abs(ds.AvgDelta) > 1.0 {
			for col := 0; col < len(vals); col++ {
				c := cell(col+1, row)
				_ = f.SetCellStyle(sheet, c, c, s.red)
			}
		}

		// Track date change for the aggregate footer.
		if ds.Date != prevDate {
			prevDate = ds.Date
		}
	}

	// ── 30-day aggregate footer ─────────────────────────────────────────

	if len(summaries) > 0 {
		aggRow := len(summaries) + 3

		// Collect unique airports for per-airport totals.
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
				aa = &airportAgg{
					maxTemp: ds.ATH,
					minTemp: ds.ATL,
				}
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

			// Colour-code the aggregate delta.
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
// Daily airport summary computation
// ═══════════════════════════════════════════════════════════════════════════

// dailyAirportSummary holds one airport's stats for one UTC day.
type dailyAirportSummary struct {
	Date         string
	ICAO         string
	AvgDelta     float64 // mean(Reality − Forecast)
	ATH          float64 // max RealTemp that day
	ATL          float64 // min RealTemp that day
	DiurnalRange float64 // ATH − ATL
	Count        int
}

// computeDailyAirportSummaries groups logs by (Date, ICAO) and calculates
// per-group metrics. Results are sorted by date ascending then ICAO.
func computeDailyAirportSummaries(logs []storage.WeatherLog) []dailyAirportSummary {
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
		day := l.Timestamp.UTC().Format("2006-01-02")
		k := groupKey{date: day, icao: l.AirportICAO}

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

	// Sort by date ascending, then by ICAO alphabetically.
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

// classifyAbsDelta labels an absolute delta value.
// Thresholds match monitor.ClassifyDelta.
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

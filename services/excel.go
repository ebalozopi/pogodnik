package services

import (
	"fmt"
	"math"
	"time"

	"pogodnik/storage"

	"github.com/xuri/excelize/v2"
)

// ═══════════════════════════════════════════════════════════════════════════
// GenerateReport — multi-sheet Excel with bias analytics
// ═══════════════════════════════════════════════════════════════════════════

func GenerateReport(logs []storage.WeatherLog) (*excelize.File, error) {
	f := excelize.NewFile()

	// ── Styles ──────────────────────────────────────────────────────────

	styles, err := createStyles(f)
	if err != nil {
		return nil, err
	}

	// ── Sheet 1: Raw Data ───────────────────────────────────────────────

	if err := writeRawDataSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: raw data: %w", err)
	}

	// ── Sheet 2: Bias Analysis ──────────────────────────────────────────

	if err := writeBiasSheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: bias: %w", err)
	}

	// ── Sheet 3: Daily Summary ──────────────────────────────────────────

	if err := writeDailySheet(f, styles, logs); err != nil {
		return nil, fmt.Errorf("excel: daily: %w", err)
	}

	// Remove default Sheet1.
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
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border:    []excelize.Border{{Type: "bottom", Color: "#000000", Style: 2}},
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
		NumFmt:    10, // 0.00%
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
		NumFmt:    22, // date+time
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.boolTrue, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#0000FF"},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	s.boolFalse, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#808080"},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 1: Raw Data (30 Days)
// ═══════════════════════════════════════════════════════════════════════════

func writeRawDataSheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Raw Data (30 Days)"
	_, err := f.NewSheet(sheet)
	if err != nil {
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

	// Write headers.
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, h)
		_ = f.SetCellStyle(sheet, cell, cell, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	// Write data.
	for rowIdx, l := range logs {
		row := rowIdx + 2

		ad := math.Abs(l.Delta)
		verdict := classifyAbsDelta(ad)

		vals := []interface{}{
			l.Timestamp.UTC().Format("2006-01-02"),         // A: Date
			l.Timestamp.UTC().Format("15:04:05"),           // B: Time
			l.AirportICAO,                                  // C: Airport
			l.ForecastTemp,                                 // D: Forecast
			l.RealTemp,                                     // E: Reality
			l.Delta,                                        // F: Delta
			ad,                                             // G: |Delta|
			verdict,                                        // H: Verdict
			l.WindSpeed,                                    // I: Wind
			l.DirectRadiation,                              // J: Solar
			boolStr(l.IsRaining),                           // K: Rain
			boolStr(l.IsFoggy),                             // L: Fog
			boolStr(l.IsSpeci),                             // M: SPECI
		}

		for col, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			_ = f.SetCellValue(sheet, cell, v)

			// Style numeric columns.
			if col >= 3 && col <= 6 || col == 8 || col == 9 {
				_ = f.SetCellStyle(sheet, cell, cell, s.number)
			}

			// Bool columns.
			if col >= 10 && col <= 12 {
				if v == "YES" {
					_ = f.SetCellStyle(sheet, cell, cell, s.boolTrue)
				} else {
					_ = f.SetCellStyle(sheet, cell, cell, s.boolFalse)
				}
			}
		}

		// Highlight entire row if |Delta| > 1.0°C.
		if ad > 1.0 {
			for col := 0; col < len(vals); col++ {
				cell, _ := excelize.CoordinatesToCellName(col+1, row)
				_ = f.SetCellStyle(sheet, cell, cell, s.red)
			}
		}
	}

	// Freeze header.
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Sheet 2: Bias Analysis
// ═══════════════════════════════════════════════════════════════════════════

func writeBiasSheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Bias Analysis"
	_, err := f.NewSheet(sheet)
	if err != nil {
		return err
	}

	// Group logs by airport.
	byAirport := make(map[string][]storage.WeatherLog)
	var icaoOrder []string

	for _, l := range logs {
		if _, exists := byAirport[l.AirportICAO]; !exists {
			icaoOrder = append(icaoOrder, l.AirportICAO)
		}
		byAirport[l.AirportICAO] = append(byAirport[l.AirportICAO], l)
	}

	// Headers.
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
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, h)
		_ = f.SetCellStyle(sheet, cell, cell, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	// Data rows.
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
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			_ = f.SetCellValue(sheet, cell, v)

			switch col {
			case 2, 7, 9:
				_ = f.SetCellStyle(sheet, cell, cell, s.number)
			case 3, 4, 5, 6:
				_ = f.SetCellStyle(sheet, cell, cell, s.pct)
			}
		}

		// Colour the assessment cell.
		assessCell, _ := excelize.CoordinatesToCellName(12, row)
		switch assessment {
		case "EXCELLENT":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.green)
		case "GOOD":
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, assessCell, assessCell, s.red)
		}

		// Colour bias direction.
		biasCell, _ := excelize.CoordinatesToCellName(3, row)
		if math.Abs(stats.AvgBias) > 1.0 {
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.red)
		} else if math.Abs(stats.AvgBias) > 0.5 {
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.yellow)
		} else {
			_ = f.SetCellStyle(sheet, biasCell, biasCell, s.green)
		}

		row++
	}

	// ── Legend ───────────────────────────────────────────────────────────

	legendRow := row + 2
	legends := []struct{ label, desc string }{
		{"Avg Bias", "Mean(Forecast − Reality). Positive = Warm Bias in forecast."},
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
// Sheet 3: Daily Summary
// ═══════════════════════════════════════════════════════════════════════════

func writeDailySheet(f *excelize.File, s *reportStyles, logs []storage.WeatherLog) error {
	sheet := "Daily Summary"
	_, err := f.NewSheet(sheet)
	if err != nil {
		return err
	}

	summaries := storage.ComputeDailySummaries(logs)

	headers := []string{
		"Date",
		"Observations",
		"Avg |Error| (°C)",
		"Max |Error| (°C)",
		"Avg Bias (°C)",
		"Quality",
	}
	widths := []float64{14, 14, 16, 16, 14, 14}

	for i, h := range headers {
		c, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, c, h)
		_ = f.SetCellStyle(sheet, c, c, s.header)
	}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, w)
	}

	for rowIdx, ds := range summaries {
		row := rowIdx + 2
		quality := dailyQuality(ds.AvgAbsError)

		vals := []interface{}{
			ds.Date,
			ds.Count,
			ds.AvgAbsError,
			ds.MaxAbsError,
			ds.AvgBias,
			quality,
		}

		for col, v := range vals {
			c, _ := excelize.CoordinatesToCellName(col+1, row)
			_ = f.SetCellValue(sheet, c, v)

			if col >= 2 && col <= 4 {
				_ = f.SetCellStyle(sheet, c, c, s.number)
			}
		}

		// Colour quality cell.
		qCell, _ := excelize.CoordinatesToCellName(6, row)
		switch quality {
		case "EXCELLENT":
			_ = f.SetCellStyle(sheet, qCell, qCell, s.green)
		case "GOOD":
			_ = f.SetCellStyle(sheet, qCell, qCell, s.yellow)
		default:
			_ = f.SetCellStyle(sheet, qCell, qCell, s.red)
		}

		// Highlight bad days.
		if ds.AvgAbsError > 1.0 {
			for col := 0; col < len(vals); col++ {
				c, _ := excelize.CoordinatesToCellName(col+1, row)
				_ = f.SetCellStyle(sheet, c, c, s.red)
			}
		}
	}

	// ── 30-day aggregate at the bottom ──────────────────────────────────

	if len(summaries) > 0 {
		aggRow := len(summaries) + 3

		var totalObs int
		var totalAbsErr, totalBias, maxErr float64

		for _, ds := range summaries {
			totalObs += ds.Count
			totalAbsErr += ds.AvgAbsError * float64(ds.Count)
			totalBias += ds.AvgBias * float64(ds.Count)
			if ds.MaxAbsError > maxErr {
				maxErr = ds.MaxAbsError
			}
		}

		n := float64(totalObs)

		_ = f.SetCellValue(sheet, cell(1, aggRow), "30-DAY TOTAL")
		_ = f.SetCellStyle(sheet, cell(1, aggRow), cell(1, aggRow), s.bold)
		_ = f.SetCellValue(sheet, cell(2, aggRow), totalObs)
		_ = f.SetCellValue(sheet, cell(3, aggRow), totalAbsErr/n)
		_ = f.SetCellStyle(sheet, cell(3, aggRow), cell(3, aggRow), s.number)
		_ = f.SetCellValue(sheet, cell(4, aggRow), maxErr)
		_ = f.SetCellStyle(sheet, cell(4, aggRow), cell(4, aggRow), s.number)
		_ = f.SetCellValue(sheet, cell(5, aggRow), totalBias/n)
		_ = f.SetCellStyle(sheet, cell(5, aggRow), cell(5, aggRow), s.number)
	}

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return nil
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

func dailyQuality(avgAbsErr float64) string {
	switch {
	case avgAbsErr < 0.5:
		return "EXCELLENT"
	case avgAbsErr < 1.0:
		return "GOOD"
	default:
		return "POOR"
	}
}

func celsiusToF(c float64) float64 {
	return c*9.0/5.0 + 32.0
}
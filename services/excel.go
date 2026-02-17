package services

import (
	"fmt"
	"math"
	"time"

	"pogodnik/storage"

	"github.com/xuri/excelize/v2"
)

// ═══════════════════════════════════════════════════════════════════════════
// Column layout
// ═══════════════════════════════════════════════════════════════════════════

// columns defines the header row; order matters — cell references below
// depend on it.
var columns = []string{
	"Date/Time (UTC)",
	"Airport",
	"Forecast (°C)",
	"Forecast (°F)",
	"Reality (°C)",
	"Reality (°F)",
	"Delta (°C)",
	"Delta (°F)",
	"Condition",
}

// ═══════════════════════════════════════════════════════════════════════════
// Report generation
// ═══════════════════════════════════════════════════════════════════════════

// GenerateReport builds an Excel workbook from a slice of WeatherLog rows.
// The returned *excelize.File can be saved to disk or streamed to Telegram.
//
//	f, err := services.GenerateReport(logs)
//	if err != nil { ... }
//	_ = f.SaveAs("report.xlsx")
func GenerateReport(logs []storage.WeatherLog) (*excelize.File, error) {
	f := excelize.NewFile()
	defer func() {
		// excelize keeps temp files; close only on error path.
		// On success the caller owns the file.
	}()

	sheet := "Weather Report"

	// Rename the default "Sheet1" to our name.
	idx, err := f.NewSheet(sheet)
	if err != nil {
		return nil, fmt.Errorf("excel: new sheet: %w", err)
	}
	f.SetActiveSheet(idx)
	f.DeleteSheet("Sheet1")

	// ── Styles ──────────────────────────────────────────────────────────

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{
			Bold:  true,
			Size:  11,
			Color: "#FFFFFF",
		},
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"#2B5797"},
		},
		Alignment: &excelize.Alignment{
			Horizontal: "center",
			Vertical:   "center",
		},
		Border: []excelize.Border{
			{Type: "bottom", Color: "#000000", Style: 2},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("excel: header style: %w", err)
	}

	greenStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#006100"},
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"#C6EFCE"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("excel: green style: %w", err)
	}

	yellowStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#9C6500"},
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"#FFEB9C"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("excel: yellow style: %w", err)
	}

	redStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#9C0006"},
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"#FFC7CE"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("excel: red style: %w", err)
	}

	numberStyle, err := f.NewStyle(&excelize.Style{
		NumFmt: 2, // 0.00
		Alignment: &excelize.Alignment{
			Horizontal: "center",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("excel: number style: %w", err)
	}

	// ── Header row ──────────────────────────────────────────────────────

	for i, col := range columns {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, col)
		_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// ── Column widths ───────────────────────────────────────────────────

	widths := []float64{22, 10, 14, 14, 14, 14, 12, 12, 14}
	for i, w := range widths {
		colLetter, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, colLetter, colLetter, w)
	}

	// ── Data rows ───────────────────────────────────────────────────────

	for rowIdx, log := range logs {
		row := rowIdx + 2 // 1-based, skip header

		forecastF := celsiusToF(log.ForecastTemp)
		realF := celsiusToF(log.RealTemp)
		deltaF := log.Delta * 9.0 / 5.0

		values := []interface{}{
			log.Timestamp.UTC().Format(time.DateTime), // A
			log.AirportICAO,                           // B
			log.ForecastTemp,                          // C
			forecastF,                                 // D
			log.RealTemp,                              // E
			realF,                                     // F
			log.Delta,                                 // G
			deltaF,                                    // H
			log.Condition,                             // I
		}

		for col, val := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			_ = f.SetCellValue(sheet, cell, val)

			// Apply number format to temperature columns (C-H).
			if col >= 2 && col <= 7 {
				_ = f.SetCellStyle(sheet, cell, cell, numberStyle)
			}
		}

		// Colour-code the Condition cell (column I).
		condCell, _ := excelize.CoordinatesToCellName(9, row)
		switch {
		case log.Condition == "Accurate":
			_ = f.SetCellStyle(sheet, condCell, condCell, greenStyle)
		case log.Condition == "Close":
			_ = f.SetCellStyle(sheet, condCell, condCell, yellowStyle)
		default: // "Off", "Poor", anything else
			_ = f.SetCellStyle(sheet, condCell, condCell, redStyle)
		}
	}

	// ── Summary section ─────────────────────────────────────────────────

	if len(logs) > 0 {
		if err := writeSummary(f, sheet, logs); err != nil {
			return nil, err
		}
	}

	// ── Freeze header row ───────────────────────────────────────────────

	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	return f, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Summary block (appended two rows below the data)
// ═══════════════════════════════════════════════════════════════════════════

func writeSummary(f *excelize.File, sheet string, logs []storage.WeatherLog) error {
	summaryRow := len(logs) + 3 // one blank row gap

	boldStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 11},
	})
	if err != nil {
		return fmt.Errorf("excel: bold style: %w", err)
	}

	valStyle, err := f.NewStyle(&excelize.Style{
		NumFmt: 2,
		Font:   &excelize.Font{Size: 11},
	})
	if err != nil {
		return fmt.Errorf("excel: val style: %w", err)
	}

	// Compute stats.
	var sumDelta, minDelta, maxDelta float64
	minDelta = math.MaxFloat64
	maxDelta = -math.MaxFloat64

	accurate, close_, off, poor := 0, 0, 0, 0

	for _, l := range logs {
		d := math.Abs(l.Delta)
		sumDelta += d
		if d < minDelta {
			minDelta = d
		}
		if d > maxDelta {
			maxDelta = d
		}
		switch l.Condition {
		case "Accurate":
			accurate++
		case "Close":
			close_++
		case "Off":
			off++
		default:
			poor++
		}
	}

	avgDelta := sumDelta / float64(len(logs))
	total := len(logs)

	labels := []struct {
		label string
		value interface{}
	}{
		{"Total Observations", total},
		{"Average |Δ| (°C)", avgDelta},
		{"Min |Δ| (°C)", minDelta},
		{"Max |Δ| (°C)", maxDelta},
		{"Accurate (< 1°C)", fmt.Sprintf("%d (%.0f%%)", accurate, pct(accurate, total))},
		{"Close    (< 2.5°C)", fmt.Sprintf("%d (%.0f%%)", close_, pct(close_, total))},
		{"Off      (< 5°C)", fmt.Sprintf("%d (%.0f%%)", off, pct(off, total))},
		{"Poor     (≥ 5°C)", fmt.Sprintf("%d (%.0f%%)", poor, pct(poor, total))},
	}

	for i, item := range labels {
		r := summaryRow + i
		labelCell, _ := excelize.CoordinatesToCellName(1, r)
		valueCell, _ := excelize.CoordinatesToCellName(2, r)

		_ = f.SetCellValue(sheet, labelCell, item.label)
		_ = f.SetCellStyle(sheet, labelCell, labelCell, boldStyle)

		_ = f.SetCellValue(sheet, valueCell, item.value)
		_ = f.SetCellStyle(sheet, valueCell, valueCell, valStyle)
	}

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════

func celsiusToF(c float64) float64 {
	return c*9.0/5.0 + 32.0
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
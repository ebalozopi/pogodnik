package storage

import (
	"fmt"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ═══════════════════════════════════════════════════════════════════════════
// Models
// ═══════════════════════════════════════════════════════════════════════════

type Airport struct {
	ICAO     string  `gorm:"column:icao;primaryKey;size:4"  json:"icao"`
	City     string  `gorm:"column:city;not null"           json:"city"`
	Lat      float64 `gorm:"column:lat;not null"            json:"lat"`
	Lon      float64 `gorm:"column:lon;not null"            json:"lon"`
	Timezone string  `gorm:"column:timezone;not null"       json:"timezone"`
	IsMuted  bool    `gorm:"column:is_muted;default:false"  json:"is_muted"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Airport) TableName() string { return "airports" }

// WeatherLog is the core analytics record. Each row represents a single
// event: the moment a new METAR/SPECI was detected and a notification
// was triggered.
//
// Delta semantics: Forecast − Reality.
//
//	Positive delta → forecast was warmer than reality (warm bias).
//	Negative delta → forecast was cooler than reality (cool bias).
type WeatherLog struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"                    json:"id"`
	AirportICAO  string    `gorm:"column:airport_icao;size:4;not null;index"  json:"airport_icao"`
	Timestamp    time.Time `gorm:"column:timestamp;not null;index"            json:"timestamp"`
	ForecastTemp float64   `gorm:"column:forecast_temp"                       json:"forecast_temp"`
	RealTemp     float64   `gorm:"column:real_temp;not null"                  json:"real_temp"`
	Delta        float64   `gorm:"column:delta"                               json:"delta"`
	Condition    string    `gorm:"column:condition;size:255"                   json:"condition"`
	IsSpeci      bool      `gorm:"column:is_speci;default:false"              json:"is_speci"`

	// Contextual fields for bias analysis.
	WindSpeed       float64 `gorm:"column:wind_speed"                          json:"wind_speed"`        // m/s (converted from kt)
	DirectRadiation float64 `gorm:"column:direct_radiation"                    json:"direct_radiation"`  // W/m²
	IsRaining       bool    `gorm:"column:is_raining;default:false"            json:"is_raining"`
	IsFoggy         bool    `gorm:"column:is_foggy;default:false"              json:"is_foggy"`

	// Raw observation for audit trail.
	RawMETAR string `gorm:"column:raw_metar;type:TEXT"                  json:"raw_metar"`

	Airport   Airport   `gorm:"foreignKey:AirportICAO;references:ICAO;constraint:OnDelete:CASCADE" json:"-"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

func (WeatherLog) TableName() string { return "weather_logs" }

// ForecastCache stores raw Open-Meteo JSON for the 3-hour caching strategy.
type ForecastCache struct {
	AirportICAO  string    `gorm:"column:airport_icao;primaryKey;size:4" json:"airport_icao"`
	ResponseJSON string    `gorm:"column:response_json;type:TEXT"        json:"response_json"`
	FetchedAt    time.Time `gorm:"column:fetched_at;not null;index"      json:"fetched_at"`

	Airport   Airport   `gorm:"foreignKey:AirportICAO;references:ICAO;constraint:OnDelete:CASCADE" json:"-"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (ForecastCache) TableName() string { return "forecast_cache" }

// ═══════════════════════════════════════════════════════════════════════════
// Initialisation
// ═══════════════════════════════════════════════════════════════════════════

func InitDB(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	if err := db.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("storage: enable WAL: %w", err)
	}
	if err := db.Exec("PRAGMA foreign_keys=ON").Error; err != nil {
		return nil, fmt.Errorf("storage: enable FK: %w", err)
	}

	if err := db.AutoMigrate(&Airport{}, &WeatherLog{}, &ForecastCache{}); err != nil {
		return nil, fmt.Errorf("storage: migrate: %w", err)
	}

	// Composite index for bias-analysis queries.
	db.Exec("CREATE INDEX IF NOT EXISTS idx_wl_icao_ts ON weather_logs(airport_icao, timestamp)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_wl_radiation ON weather_logs(direct_radiation)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_wl_raining ON weather_logs(is_raining)")

	return db, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Seed
// ═══════════════════════════════════════════════════════════════════════════

func SeedAirports(db *gorm.DB, airports []Airport) error {
	for _, apt := range airports {
		result := db.
			Where(Airport{ICAO: apt.ICAO}).
			Assign(Airport{
				City: apt.City, Lat: apt.Lat,
				Lon: apt.Lon, Timezone: apt.Timezone,
			}).
			FirstOrCreate(&Airport{})
		if result.Error != nil {
			return fmt.Errorf("storage: seed %s: %w", apt.ICAO, result.Error)
		}
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Airport queries
// ═══════════════════════════════════════════════════════════════════════════

func GetAirport(db *gorm.DB, icao string) (*Airport, error) {
	var apt Airport
	if err := db.First(&apt, "icao = ?", icao).Error; err != nil {
		return nil, fmt.Errorf("storage: get %s: %w", icao, err)
	}
	return &apt, nil
}

func ListAirports(db *gorm.DB) ([]Airport, error) {
	var list []Airport
	if err := db.Order("icao").Find(&list).Error; err != nil {
		return nil, fmt.Errorf("storage: list: %w", err)
	}
	return list, nil
}

func SetMuted(db *gorm.DB, icao string, muted bool) error {
	r := db.Model(&Airport{}).Where("icao = ?", icao).Update("is_muted", muted)
	if r.Error != nil {
		return fmt.Errorf("storage: mute %s: %w", icao, r.Error)
	}
	if r.RowsAffected == 0 {
		return fmt.Errorf("storage: %s not found", icao)
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Weather log queries
// ═══════════════════════════════════════════════════════════════════════════

func InsertWeatherLog(db *gorm.DB, l *WeatherLog) error {
	if err := db.Create(l).Error; err != nil {
		return fmt.Errorf("storage: insert log: %w", err)
	}
	return nil
}

func RecentLogs(db *gorm.DB, icao string, limit int) ([]WeatherLog, error) {
	var logs []WeatherLog
	err := db.Where("airport_icao = ?", icao).
		Order("timestamp DESC").Limit(limit).Find(&logs).Error
	return logs, err
}

// Last30DaysLogs returns all logs within the last 30 days, ordered by
// timestamp ascending (oldest first) — ideal for reports.
func Last30DaysLogs(db *gorm.DB) ([]WeatherLog, error) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var logs []WeatherLog
	err := db.Where("timestamp >= ?", cutoff).
		Order("timestamp ASC").Find(&logs).Error
	if err != nil {
		return nil, fmt.Errorf("storage: 30d logs: %w", err)
	}
	return logs, nil
}

// Last30DaysLogsByAirport returns logs for a specific airport.
func Last30DaysLogsByAirport(db *gorm.DB, icao string) ([]WeatherLog, error) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var logs []WeatherLog
	err := db.Where("airport_icao = ? AND timestamp >= ?", icao, cutoff).
		Order("timestamp ASC").Find(&logs).Error
	return logs, err
}

func PurgeOlderThan(db *gorm.DB, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	r := db.Where("timestamp < ?", cutoff).Delete(&WeatherLog{})
	if r.Error != nil {
		return 0, fmt.Errorf("storage: purge: %w", r.Error)
	}
	return r.RowsAffected, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Forecast cache
// ═══════════════════════════════════════════════════════════════════════════

func GetForecastCache(db *gorm.DB, icao string) (*ForecastCache, error) {
	var fc ForecastCache
	err := db.First(&fc, "airport_icao = ?", icao).Error
	if err != nil {
		return nil, err
	}
	return &fc, nil
}

func UpsertForecastCache(db *gorm.DB, icao, jsonData string) error {
	now := time.Now().UTC()
	r := db.Where(ForecastCache{AirportICAO: icao}).
		Assign(ForecastCache{ResponseJSON: jsonData, FetchedAt: now}).
		FirstOrCreate(&ForecastCache{})
	if r.Error != nil {
		return fmt.Errorf("storage: upsert cache %s: %w", icao, r.Error)
	}
	return nil
}

func IsForecastCacheFresh(c *ForecastCache, ttl time.Duration) bool {
	if c == nil {
		return false
	}
	return time.Since(c.FetchedAt) < ttl
}

func PurgeForecastCache(db *gorm.DB, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	r := db.Where("fetched_at < ?", cutoff).Delete(&ForecastCache{})
	return r.RowsAffected, r.Error
}

// ═══════════════════════════════════════════════════════════════════════════
// Aggregation helpers (used by excel.go and /status)
// ═══════════════════════════════════════════════════════════════════════════

// BiasStats holds pre-computed bias metrics for one airport.
type BiasStats struct {
	ICAO      string
	Count     int
	AvgBias   float64 // mean(forecast − reality)
	Excellent float64 // % within ±0.5°C
	Good      float64 // % within ±1.0°C
	Critical  float64 // % > 1.0°C error
	Extreme   float64 // % > 2.0°C error
	SolarAvg  float64 // avg delta when radiation > 400
	SolarN    int
	RainAvg   float64 // avg delta when raining
	RainN     int
}

// ComputeBiasStats calculates all bias metrics from a slice of logs
// belonging to a single airport.
func ComputeBiasStats(icao string, logs []WeatherLog) BiasStats {
	s := BiasStats{ICAO: icao}
	if len(logs) == 0 {
		return s
	}

	s.Count = len(logs)

	var sumDelta float64
	var excellent, good, critical, extreme int
	var solarSum float64
	var rainSum float64

	for _, l := range logs {
		d := l.Delta          // forecast − reality
		ad := abs64(d)
		sumDelta += d

		if ad <= 0.5 {
			excellent++
		}
		if ad <= 1.0 {
			good++
		}
		if ad > 1.0 {
			critical++
		}
		if ad > 2.0 {
			extreme++
		}

		if l.DirectRadiation > 400 {
			solarSum += d
			s.SolarN++
		}
		if l.IsRaining {
			rainSum += d
			s.RainN++
		}
	}

	n := float64(s.Count)
	s.AvgBias = sumDelta / n
	s.Excellent = float64(excellent) / n * 100
	s.Good = float64(good) / n * 100
	s.Critical = float64(critical) / n * 100
	s.Extreme = float64(extreme) / n * 100

	if s.SolarN > 0 {
		s.SolarAvg = solarSum / float64(s.SolarN)
	}
	if s.RainN > 0 {
		s.RainAvg = rainSum / float64(s.RainN)
	}

	return s
}

// DailySummary holds one day's aggregate error across all airports.
type DailySummary struct {
	Date        string
	Count       int
	AvgAbsError float64
	MaxAbsError float64
	AvgBias     float64
}

// ComputeDailySummaries groups logs by UTC date and calculates daily stats.
func ComputeDailySummaries(logs []WeatherLog) []DailySummary {
	type accumulator struct {
		sumAbs  float64
		sumBias float64
		maxAbs  float64
		count   int
	}

	byDate := make(map[string]*accumulator)
	var dateOrder []string

	for _, l := range logs {
		day := l.Timestamp.UTC().Format("2006-01-02")

		acc, exists := byDate[day]
		if !exists {
			acc = &accumulator{}
			byDate[day] = acc
			dateOrder = append(dateOrder, day)
		}

		ad := abs64(l.Delta)
		acc.sumAbs += ad
		acc.sumBias += l.Delta
		if ad > acc.maxAbs {
			acc.maxAbs = ad
		}
		acc.count++
	}

	summaries := make([]DailySummary, 0, len(dateOrder))
	for _, day := range dateOrder {
		acc := byDate[day]
		n := float64(acc.count)
		summaries = append(summaries, DailySummary{
			Date:        day,
			Count:       acc.count,
			AvgAbsError: acc.sumAbs / n,
			MaxAbsError: acc.maxAbs,
			AvgBias:     acc.sumBias / n,
		})
	}

	return summaries
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
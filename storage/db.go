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

// Airport is the persistent representation of a monitored station.
// ICAO is the natural primary key — it is unique, immutable, and
// already used as the lookup key everywhere else in the codebase.
type Airport struct {
	ICAO     string  `gorm:"column:icao;primaryKey;size:4"  json:"icao"`
	City     string  `gorm:"column:city;not null"           json:"city"`
	Lat      float64 `gorm:"column:lat;not null"            json:"lat"`
	Lon      float64 `gorm:"column:lon;not null"            json:"lon"`
	Timezone string  `gorm:"column:timezone;not null"       json:"timezone"`
	IsMuted  bool    `gorm:"column:is_muted;default:false"  json:"is_muted"`

	CreatedAt time.Time `gorm:"autoCreateTime"  json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"  json:"updated_at"`
}

// TableName overrides the default pluralised table name.
func (Airport) TableName() string {
	return "airports"
}

// WeatherLog is an append-only ledger of every observation the bot
// processes.  Each row captures the forecast-vs-reality snapshot at
// a single point in time for one airport.
type WeatherLog struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"                    json:"id"`
	AirportICAO  string    `gorm:"column:airport_icao;size:4;not null;index"  json:"airport_icao"`
	Timestamp    time.Time `gorm:"column:timestamp;not null;index"            json:"timestamp"`
	ForecastTemp float64   `gorm:"column:forecast_temp"                       json:"forecast_temp"`
	RealTemp     float64   `gorm:"column:real_temp;not null"                  json:"real_temp"`
	Delta        float64   `gorm:"column:delta"                               json:"delta"`
	Condition    string    `gorm:"column:condition;size:255"                   json:"condition"`

	// Foreign key back to airports table.
	Airport Airport `gorm:"foreignKey:AirportICAO;references:ICAO;constraint:OnDelete:CASCADE" json:"-"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// TableName overrides the default pluralised table name.
func (WeatherLog) TableName() string {
	return "weather_logs"
}

// ═══════════════════════════════════════════════════════════════════════════
// Initialisation
// ═══════════════════════════════════════════════════════════════════════════

// InitDB opens (or creates) the SQLite database at path, applies
// WAL mode for better concurrent-read performance, and runs
// AutoMigrate so the schema is always up-to-date.
//
//	db, err := storage.InitDB("pogodnik.db")
func InitDB(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	// Enable WAL so readers never block the single writer.
	if err := db.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("storage: enable WAL: %w", err)
	}

	// Enforce foreign-key constraints (off by default in SQLite).
	if err := db.Exec("PRAGMA foreign_keys=ON").Error; err != nil {
		return nil, fmt.Errorf("storage: enable foreign keys: %w", err)
	}

	// Run migrations.
	if err := db.AutoMigrate(&Airport{}, &WeatherLog{}); err != nil {
		return nil, fmt.Errorf("storage: auto-migrate: %w", err)
	}

	return db, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Seed helper
// ═══════════════════════════════════════════════════════════════════════════

// SeedAirports upserts the provided airport list so that the database
// always contains at least the hardcoded stations from config.go.
// Existing rows are updated (city, coords, tz); IsMuted is left
// untouched so user preferences survive restarts.
func SeedAirports(db *gorm.DB, airports []Airport) error {
	for _, apt := range airports {
		result := db.
			Where(Airport{ICAO: apt.ICAO}).
			Assign(Airport{
				City:     apt.City,
				Lat:      apt.Lat,
				Lon:      apt.Lon,
				Timezone: apt.Timezone,
			}).
			FirstOrCreate(&Airport{})

		if result.Error != nil {
			return fmt.Errorf("storage: seed %s: %w", apt.ICAO, result.Error)
		}
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Convenience queries
// ═══════════════════════════════════════════════════════════════════════════

// GetAirport loads a single airport by ICAO.
func GetAirport(db *gorm.DB, icao string) (*Airport, error) {
	var apt Airport
	if err := db.First(&apt, "icao = ?", icao).Error; err != nil {
		return nil, fmt.Errorf("storage: get airport %s: %w", icao, err)
	}
	return &apt, nil
}

// ListAirports returns every airport row, ordered by ICAO.
func ListAirports(db *gorm.DB) ([]Airport, error) {
	var airports []Airport
	if err := db.Order("icao").Find(&airports).Error; err != nil {
		return nil, fmt.Errorf("storage: list airports: %w", err)
	}
	return airports, nil
}

// ListUnmutedAirports returns only airports where is_muted = false.
func ListUnmutedAirports(db *gorm.DB) ([]Airport, error) {
	var airports []Airport
	if err := db.Where("is_muted = ?", false).Order("icao").Find(&airports).Error; err != nil {
		return nil, fmt.Errorf("storage: list unmuted airports: %w", err)
	}
	return airports, nil
}

// SetMuted toggles the mute flag for an airport.
func SetMuted(db *gorm.DB, icao string, muted bool) error {
	result := db.Model(&Airport{}).Where("icao = ?", icao).Update("is_muted", muted)
	if result.Error != nil {
		return fmt.Errorf("storage: set muted %s: %w", icao, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("storage: airport %s not found", icao)
	}
	return nil
}

// InsertWeatherLog appends a new observation to the ledger.
func InsertWeatherLog(db *gorm.DB, log *WeatherLog) error {
	if err := db.Create(log).Error; err != nil {
		return fmt.Errorf("storage: insert weather log: %w", err)
	}
	return nil
}

// RecentLogs returns the last `limit` observations for an airport,
// newest first.
func RecentLogs(db *gorm.DB, icao string, limit int) ([]WeatherLog, error) {
	var logs []WeatherLog
	err := db.
		Where("airport_icao = ?", icao).
		Order("timestamp DESC").
		Limit(limit).
		Find(&logs).Error
	if err != nil {
		return nil, fmt.Errorf("storage: recent logs %s: %w", icao, err)
	}
	return logs, nil
}

// LogsInRange returns observations for an airport within [from, to].
func LogsInRange(db *gorm.DB, icao string, from, to time.Time) ([]WeatherLog, error) {
	var logs []WeatherLog
	err := db.
		Where("airport_icao = ? AND timestamp BETWEEN ? AND ?", icao, from, to).
		Order("timestamp ASC").
		Find(&logs).Error
	if err != nil {
		return nil, fmt.Errorf("storage: logs in range %s: %w", icao, err)
	}
	return logs, nil
}

// PurgeOlderThan deletes weather logs older than the given duration.
// Call periodically (e.g., daily) to keep the database size bounded.
func PurgeOlderThan(db *gorm.DB, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	result := db.Where("timestamp < ?", cutoff).Delete(&WeatherLog{})
	if result.Error != nil {
		return 0, fmt.Errorf("storage: purge: %w", result.Error)
	}
	return result.RowsAffected, nil
}
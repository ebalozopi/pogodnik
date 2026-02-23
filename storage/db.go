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

func (Airport) TableName() string { return "airports" }

// WeatherLog is an append-only ledger of every observation.
type WeatherLog struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"                    json:"id"`
	AirportICAO  string    `gorm:"column:airport_icao;size:4;not null;index"  json:"airport_icao"`
	Timestamp    time.Time `gorm:"column:timestamp;not null;index"            json:"timestamp"`
	ForecastTemp float64   `gorm:"column:forecast_temp"                       json:"forecast_temp"`
	RealTemp     float64   `gorm:"column:real_temp;not null"                  json:"real_temp"`
	Delta        float64   `gorm:"column:delta"                               json:"delta"`
	Condition    string    `gorm:"column:condition;size:255"                   json:"condition"`
	IsSpeci      bool      `gorm:"column:is_speci;default:false"              json:"is_speci"`

	Airport   Airport   `gorm:"foreignKey:AirportICAO;references:ICAO;constraint:OnDelete:CASCADE" json:"-"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

func (WeatherLog) TableName() string { return "weather_logs" }

// ForecastCache stores the raw Open-Meteo JSON response per airport
// so we only hit the API every 3 hours instead of every 60 seconds.
type ForecastCache struct {
	AirportICAO string    `gorm:"column:airport_icao;primaryKey;size:4" json:"airport_icao"`
	ResponseJSON string   `gorm:"column:response_json;type:TEXT"        json:"response_json"`
	FetchedAt   time.Time `gorm:"column:fetched_at;not null;index"      json:"fetched_at"`

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
		return nil, fmt.Errorf("storage: enable foreign keys: %w", err)
	}

	if err := db.AutoMigrate(&Airport{}, &WeatherLog{}, &ForecastCache{}); err != nil {
		return nil, fmt.Errorf("storage: auto-migrate: %w", err)
	}

	return db, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Seed helper
// ═══════════════════════════════════════════════════════════════════════════

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
// Airport queries
// ═══════════════════════════════════════════════════════════════════════════

func GetAirport(db *gorm.DB, icao string) (*Airport, error) {
	var apt Airport
	if err := db.First(&apt, "icao = ?", icao).Error; err != nil {
		return nil, fmt.Errorf("storage: get airport %s: %w", icao, err)
	}
	return &apt, nil
}

func ListAirports(db *gorm.DB) ([]Airport, error) {
	var airports []Airport
	if err := db.Order("icao").Find(&airports).Error; err != nil {
		return nil, fmt.Errorf("storage: list airports: %w", err)
	}
	return airports, nil
}

func ListUnmutedAirports(db *gorm.DB) ([]Airport, error) {
	var airports []Airport
	if err := db.Where("is_muted = ?", false).Order("icao").Find(&airports).Error; err != nil {
		return nil, fmt.Errorf("storage: list unmuted airports: %w", err)
	}
	return airports, nil
}

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

// ═══════════════════════════════════════════════════════════════════════════
// Weather log queries
// ═══════════════════════════════════════════════════════════════════════════

func InsertWeatherLog(db *gorm.DB, log *WeatherLog) error {
	if err := db.Create(log).Error; err != nil {
		return fmt.Errorf("storage: insert weather log: %w", err)
	}
	return nil
}

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

func PurgeOlderThan(db *gorm.DB, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	result := db.Where("timestamp < ?", cutoff).Delete(&WeatherLog{})
	if result.Error != nil {
		return 0, fmt.Errorf("storage: purge: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Forecast cache queries
// ═══════════════════════════════════════════════════════════════════════════

// GetForecastCache returns the cached forecast for an airport, or nil
// if no cache entry exists.
func GetForecastCache(db *gorm.DB, icao string) (*ForecastCache, error) {
	var fc ForecastCache
	err := db.First(&fc, "airport_icao = ?", icao).Error
	if err != nil {
		return nil, err // may be gorm.ErrRecordNotFound
	}
	return &fc, nil
}

// UpsertForecastCache inserts or updates the cached forecast JSON.
func UpsertForecastCache(db *gorm.DB, icao string, jsonData string) error {
	now := time.Now().UTC()

	result := db.Where(ForecastCache{AirportICAO: icao}).
		Assign(ForecastCache{
			ResponseJSON: jsonData,
			FetchedAt:    now,
		}).
		FirstOrCreate(&ForecastCache{})

	if result.Error != nil {
		return fmt.Errorf("storage: upsert forecast cache %s: %w", icao, result.Error)
	}
	return nil
}

// IsForecastCacheFresh returns true if the cached forecast is younger
// than the given TTL.
func IsForecastCacheFresh(cache *ForecastCache, ttl time.Duration) bool {
	if cache == nil {
		return false
	}
	return time.Since(cache.FetchedAt) < ttl
}

// PurgeForecastCache removes stale cache entries older than the given age.
func PurgeForecastCache(db *gorm.DB, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	result := db.Where("fetched_at < ?", cutoff).Delete(&ForecastCache{})
	if result.Error != nil {
		return 0, fmt.Errorf("storage: purge forecast cache: %w", result.Error)
	}
	return result.RowsAffected, nil
}
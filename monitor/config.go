package monitor

import (
	"math"
	"sync"
	"time"
)

// Airport holds the immutable reference data for a single station.
type Airport struct {
	ICAO      string
	City      string
	Latitude  float64
	Longitude float64
	Timezone  string
}

// WeatherState stores the latest observation and running daily extremes.
type WeatherState struct {
	Mu sync.RWMutex

	Current     float64
	DailyHigh   float64
	DailyLow    float64
	TrackingDay time.Time
	LastUpdated time.Time
}

// Airports returns the full list of monitored stations.
func Airports() []Airport {
	return []Airport{
		{
			ICAO:      "NZWN",
			City:      "Wellington",
			Latitude:  -41.3272,
			Longitude: 174.8053,
			Timezone:  "Pacific/Auckland",
		},
		{
			ICAO:      "KORD",
			City:      "Chicago",
			Latitude:  41.9742,
			Longitude: -87.9073,
			Timezone:  "America/Chicago",
		},
		{
			ICAO:      "KMIA",
			City:      "Miami",
			Latitude:  25.7959,
			Longitude: -80.2870,
			Timezone:  "America/New_York",
		},
		{
			ICAO:      "RKSI",
			City:      "Seoul",
			Latitude:  37.4602,
			Longitude: 126.4407,
			Timezone:  "Asia/Seoul",
		},
		{
			ICAO:      "KDAL",
			City:      "Dallas",
			Latitude:  32.8471,
			Longitude: -96.8518,
			Timezone:  "America/Chicago",
		},
		{
			ICAO:      "KSEA",
			City:      "Seattle",
			Latitude:  47.4502,
			Longitude: -122.3088,
			Timezone:  "America/Los_Angeles",
		},
		{
			ICAO:      "KLGA",
			City:      "New York",
			Latitude:  40.7772,
			Longitude: -73.8726,
			Timezone:  "America/New_York",
		},
		{
			ICAO:      "KATL",
			City:      "Atlanta",
			Latitude:  33.6407,
			Longitude: -84.4277,
			Timezone:  "America/New_York",
		},
		{
			ICAO:      "EGLC",
			City:      "London",
			Latitude:  51.5053,
			Longitude: 0.0553,
			Timezone:  "Europe/London",
		},
		{
			ICAO:      "SAEZ",
			City:      "Buenos Aires",
			Latitude:  -34.8222,
			Longitude: -58.5358,
			Timezone:  "America/Argentina/Buenos_Aires",
		},
		{
			ICAO:      "LTAC",
			City:      "Ankara",
			Latitude:  40.1281,
			Longitude: 32.9951,
			Timezone:  "Europe/Istanbul",
		},
	}
}

// AirportMap returns stations indexed by ICAO code.
func AirportMap() map[string]Airport {
	list := Airports()
	m := make(map[string]Airport, len(list))
	for _, a := range list {
		m[a.ICAO] = a
	}
	return m
}

// NewWeatherStates builds a map of WeatherState pointers keyed by ICAO.
func NewWeatherStates(airports []Airport) map[string]*WeatherState {
	states := make(map[string]*WeatherState, len(airports))

	for _, apt := range airports {
		loc, err := time.LoadLocation(apt.Timezone)
		if err != nil {
			panic("monitor: invalid timezone for " + apt.ICAO + ": " + err.Error())
		}

		now := time.Now().In(loc)
		midnight := time.Date(
			now.Year(), now.Month(), now.Day(),
			0, 0, 0, 0,
			loc,
		)

		states[apt.ICAO] = &WeatherState{
			DailyHigh:   math.Inf(-1),
			DailyLow:    math.Inf(1),
			TrackingDay: midnight,
		}
	}

	return states
}

// InitConfig bootstraps all static and mutable data.
func InitConfig() (map[string]Airport, map[string]*WeatherState) {
	airports := AirportMap()

	list := make([]Airport, 0, len(airports))
	for _, a := range airports {
		list = append(list, a)
	}

	return airports, NewWeatherStates(list)
}
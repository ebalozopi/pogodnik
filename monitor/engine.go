package monitor

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"pogodnik/storage"

	"gorm.io/gorm"
	tele "gopkg.in/telebot.v3"
)

// ═══════════════════════════════════════════════════════════════════════════
// Engine configuration
// ═══════════════════════════════════════════════════════════════════════════

const ForecastCacheTTL = 3 * time.Hour

type EngineConfig struct {
	PollInterval time.Duration
	FetchTimeout time.Duration
	ChatIDs      []int64
	PurgeAge     time.Duration
}

func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		PollInterval: 60 * time.Second,
		FetchTimeout: 30 * time.Second,
		ChatIDs:      nil,
		PurgeAge:     30 * 24 * time.Hour,
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Engine
// ═══════════════════════════════════════════════════════════════════════════

type Engine struct {
	db       *gorm.DB
	bot      *tele.Bot
	cfg      EngineConfig
	states   map[string]*WeatherState
	pressure map[string]*PressureTracker

	lastRaw   map[string]string
	lastRawMu sync.RWMutex

	stopChan chan struct{}
	wg       sync.WaitGroup
}

func NewEngine(db *gorm.DB, bot *tele.Bot, cfg EngineConfig) *Engine {
	return &Engine{
		db:       db,
		bot:      bot,
		cfg:      cfg,
		states:   make(map[string]*WeatherState),
		pressure: make(map[string]*PressureTracker),
		lastRaw:  make(map[string]string),
		stopChan: make(chan struct{}),
	}
}

func StartMonitoring(db *gorm.DB, bot *tele.Bot, chatIDs []int64) *Engine {
	cfg := DefaultEngineConfig()
	cfg.ChatIDs = chatIDs
	engine := NewEngine(db, bot, cfg)
	engine.Start()
	return engine
}

func (e *Engine) Start() {
	e.wg.Add(1)
	go e.pollLoop()

	e.wg.Add(1)
	go e.purgeLoop()

	log.Printf("[engine] started (poll: %s, forecast TTL: %s)",
		e.cfg.PollInterval, ForecastCacheTTL)
}

func (e *Engine) Stop() {
	close(e.stopChan)
	e.wg.Wait()
	log.Println("[engine] stopped")
}

func (e *Engine) RunOnce() { e.tick() }

// ═══════════════════════════════════════════════════════════════════════════
// Poll loop
// ═══════════════════════════════════════════════════════════════════════════

func (e *Engine) pollLoop() {
	defer e.wg.Done()
	e.tick()

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.tick()
		case <-e.stopChan:
			return
		}
	}
}

func (e *Engine) tick() {
	airports, err := storage.ListAirports(e.db)
	if err != nil {
		log.Printf("[engine] list airports: %v", err)
		return
	}
	if len(airports) == 0 {
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for _, apt := range airports {
		wg.Add(1)
		sem <- struct{}{}
		go func(a storage.Airport) {
			defer wg.Done()
			defer func() { <-sem }()
			e.processAirport(a)
		}(apt)
	}
	wg.Wait()
}

// ═══════════════════════════════════════════════════════════════════════════
// Per-airport processing
// ═══════════════════════════════════════════════════════════════════════════

func (e *Engine) processAirport(apt storage.Airport) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.FetchTimeout)
	defer cancel()

	monitorApt := storageToMonitorAirport(apt)

	// ── Step A (ALWAYS): Fetch METAR / SPECI ────────────────────────────

	obs, err := FetchMETAR(ctx, apt.ICAO)
	if err != nil {
		log.Printf("[engine][%s] METAR: %v", apt.ICAO, err)
		return
	}

	// ── Step B (CONDITIONAL): Smart Forecast Fetch ──────────────────────

	ff, forecastSource := e.getOrFetchForecast(ctx, apt.ICAO, monitorApt)

	// ── Step C: Calculate delta ─────────────────────────────────────────

	var forecastTemp, delta float64
	var condition string

	if ff != nil && ff.CurrentHour != nil {
		forecastTemp = ff.CurrentHour.TempCelsius
		delta = obs.TempCelsius - forecastTemp
		condition = classifyDelta(delta)
	} else {
		condition = "NoForecast"
	}

	// ── Step D: Sensor bias check ───────────────────────────────────────

	var sensorWarnings []SensorWarning
	var currentExt *HourlyExtended
	if ff != nil {
		currentExt = ff.CurrentExtended
	}
	sensorWarnings = CheckSensorBias(obs, currentExt)

	// ── Step E: Update in-memory state ──────────────────────────────────

	e.ensureState(apt.ICAO, monitorApt)

	if hpa, ok := ParsePressure(obs.Raw); ok {
		e.pressure[apt.ICAO].Record(time.Now(), hpa)
	}

	_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)

	// ── Step F (ALWAYS): Save to DB ─────────────────────────────────────

	wlog := &storage.WeatherLog{
		AirportICAO:  apt.ICAO,
		Timestamp:    time.Now().UTC(),
		ForecastTemp: forecastTemp,
		RealTemp:     obs.TempCelsius,
		Delta:        delta,
		Condition:    condition,
		IsSpeci:      obs.IsSpeci,
	}
	if err := storage.InsertWeatherLog(e.db, wlog); err != nil {
		log.Printf("[engine][%s] DB insert: %v", apt.ICAO, err)
	}

	// ── Step G (CONDITIONAL): Notify ────────────────────────────────────

	if apt.IsMuted {
		return
	}
	if !e.hasRawChanged(apt.ICAO, obs.Raw) {
		return
	}
	e.setLastRaw(apt.ICAO, obs.Raw)

	snapshot := e.states[apt.ICAO].Snapshot()
	msg := buildFullMessage(apt, obs, ff, snapshot, e.pressure[apt.ICAO],
		sensorWarnings, forecastSource)

	for _, chatID := range e.cfg.ChatIDs {
		e.sendToChat(chatID, msg)
	}

	label := "METAR"
	if obs.IsSpeci {
		label = "⚡ SPECI"
	}
	log.Printf("[engine][%s] %s sent (%.1f°C, Δ%.1f°C, %s, src:%s, warnings:%d)",
		apt.ICAO, label, obs.TempCelsius, delta, condition,
		forecastSource, len(sensorWarnings))
}

// ═══════════════════════════════════════════════════════════════════════════
// Smart forecast caching
// ═══════════════════════════════════════════════════════════════════════════

func (e *Engine) getOrFetchForecast(
	ctx context.Context,
	icao string,
	apt Airport,
) (*FullForecast, string) {
	cached, err := storage.GetForecastCache(e.db, icao)
	if err == nil && storage.IsForecastCacheFresh(cached, ForecastCacheTTL) {
		ff, parseErr := ParseForecastJSON(cached.ResponseJSON, apt)
		if parseErr == nil {
			ff.FromCache = true
			return ff, "cache"
		}
		log.Printf("[engine][%s] cache parse error: %v", icao, parseErr)
	}

	rawJSON, fetchErr := FetchOpenMeteoRawJSON(ctx, apt)
	if fetchErr != nil {
		log.Printf("[engine][%s] forecast API: %v", icao, fetchErr)
		if cached != nil && cached.ResponseJSON != "" {
			ff, parseErr := ParseForecastJSON(cached.ResponseJSON, apt)
			if parseErr == nil {
				ff.FromCache = true
				return ff, "stale-cache"
			}
		}
		return nil, "unavailable"
	}

	if cacheErr := storage.UpsertForecastCache(e.db, icao, rawJSON); cacheErr != nil {
		log.Printf("[engine][%s] cache save: %v", icao, cacheErr)
	}

	ff, parseErr := ParseForecastJSON(rawJSON, apt)
	if parseErr != nil {
		log.Printf("[engine][%s] fresh parse error: %v", icao, parseErr)
		return nil, "parse-error"
	}

	return ff, "api"
}

// ═══════════════════════════════════════════════════════════════════════════
// Full message builder
// ═══════════════════════════════════════════════════════════════════════════

func buildFullMessage(
	apt storage.Airport,
	obs *Observation,
	ff *FullForecast,
	snap WeatherSnapshot,
	pt *PressureTracker,
	sensorWarnings []SensorWarning,
	forecastSource string,
) string {
	var b strings.Builder
	b.Grow(2000)

	loc, _ := time.LoadLocation(apt.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	localNow := time.Now().In(loc)

	// ── HEADER ──────────────────────────────────────────────────────────

	if obs.IsSpeci {
		b.WriteString("⚡️ *URGENT (SPECI)*\n")
	}

	b.WriteString("🛫 *")
	b.WriteString(apt.City)
	b.WriteString(" (")
	b.WriteString(apt.ICAO)
	b.WriteString(")*\n")
	b.WriteString("🕐 ")
	b.WriteString(localNow.Format("Mon, 02 Jan 2006 15:04 MST"))
	if obs.IsSpeci {
		b.WriteString("  ⚡️")
	}
	b.WriteString("\n\n")

	// ── REAL-TIME CONDITIONS ────────────────────────────────────────────

	b.WriteString("📡 *REAL-TIME CONDITIONS*\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "Temperature : %s\n", FormatTemp(obs.TempCelsius))
	fmt.Fprintf(&b, "Wind        : %s\n", fmtWind(obs))
	fmt.Fprintf(&b, "Visibility  : %s\n", obs.Visibility)

	if len(obs.PresentWeather) > 0 {
		fmt.Fprintf(&b, "Weather     : %s\n", strings.Join(obs.PresentWeather, " "))
	}

	if pt != nil {
		if latest, ok := pt.Latest(); ok {
			rate, trend := pt.Trend()
			if trend == "Unknown" {
				fmt.Fprintf(&b, "Pressure    : %.1f hPa\n", latest.Hpa)
			} else {
				fmt.Fprintf(&b, "Pressure    : %.1f hPa — %s (%+.1f/3hr)\n",
					latest.Hpa, trend, rate)
			}
		}
	}

	if obs.IsSpeci {
		b.WriteString("⚡ Type       : SPECI (Special Obs)\n")
	}

	b.WriteString("```\n\n")

	// ── DAY STATISTICS ──────────────────────────────────────────────────

	b.WriteString("📊 *DAY STATISTICS* (")
	b.WriteString(snap.TrackingDay.Format("02 Jan"))
	b.WriteString(")\n")
	b.WriteString("```\n")
	if math.IsInf(snap.DailyHigh, 0) {
		b.WriteString("Awaiting first observation...\n")
	} else {
		fmt.Fprintf(&b, "High (ATH)  : %s\n", FormatTemp(snap.DailyHigh))
		fmt.Fprintf(&b, "Low  (ATL)  : %s\n", FormatTemp(snap.DailyLow))
	}
	b.WriteString("```\n\n")

	// ── FORECAST vs REALITY ─────────────────────────────────────────────

	b.WriteString("🎯 *FORECAST vs REALITY*\n")
	if ff != nil && ff.CurrentHour != nil {
		delta := obs.TempCelsius - ff.CurrentHour.TempCelsius
		cacheTag := ""
		if ff.FromCache {
			cacheTag = fmt.Sprintf(" (%s)", forecastSource)
		}
		b.WriteString("```\n")
		fmt.Fprintf(&b, "Forecast    : %s%s\n", FormatTemp(ff.CurrentHour.TempCelsius), cacheTag)
		fmt.Fprintf(&b, "Reality     : %s\n", FormatTemp(obs.TempCelsius))
		fmt.Fprintf(&b, "Delta       : %s\n", FormatDelta(delta))
		fmt.Fprintf(&b, "Verdict     : %s (%s)\n",
			classifyDelta(delta), narrative(delta))
		b.WriteString("```\n\n")
	} else {
		b.WriteString("_Forecast data not available_\n\n")
	}

	// ── DAILY OUTLOOK ───────────────────────────────────────────────────

	b.WriteString("🌤 *DAILY OUTLOOK*\n")
	if ff != nil && len(ff.Upcoming) > 0 {
		b.WriteString("```\n")
		for _, hp := range ff.Upcoming {
			localHour := hp.Time.In(loc)
			fmt.Fprintf(&b, "%s : %s\n",
				localHour.Format("15:04"),
				FormatTemp(hp.TempCelsius),
			)
		}
		if ff.HasDailyExtremes {
			b.WriteString("\n")
			fmt.Fprintf(&b, "Exp. High   : %s\n", FormatTemp(ff.DailyMax))
			fmt.Fprintf(&b, "Exp. Low    : %s\n", FormatTemp(ff.DailyMin))
		}
		b.WriteString("```\n")
	} else {
		b.WriteString("_Outlook not available_\n")
	}

	// ── SENSOR QA (new section) ─────────────────────────────────────────

	if len(sensorWarnings) > 0 {
		b.WriteString("\n⚠️ *SENSOR QA*\n")
		b.WriteString("```\n")
		for _, w := range sensorWarnings {
			fmt.Fprintf(&b, "%s %s\n  %s\n", w.Icon, w.Title, w.Detail)
		}
		b.WriteString("```\n")
	}

	// ── RAW METAR ───────────────────────────────────────────────────────

	b.WriteString("\n📋 `")
	b.WriteString(obs.Raw)
	b.WriteString("`")

	return b.String()
}

// ═══════════════════════════════════════════════════════════════════════════
// Engine-local helpers
//
// FormatTemp, FormatDelta, tempSign → analyze.go
// ═══════════════════════════════════════════════════════════════════════════

func fmtWind(obs *Observation) string {
	if obs.WindSpeed == 0 && obs.WindGust == 0 {
		return "Calm"
	}
	dir := "VRB"
	if obs.WindDir >= 0 {
		dir = fmt.Sprintf("%03d°", obs.WindDir)
	}
	s := fmt.Sprintf("%s @ %dkt", dir, obs.WindSpeed)
	if obs.WindGust > 0 {
		s += fmt.Sprintf(", gusting %dkt", obs.WindGust)
	}
	return s
}

func classifyDelta(delta float64) string {
	abs := math.Abs(delta)
	switch {
	case abs < 1.0:
		return "Accurate"
	case abs < 2.5:
		return "Close"
	case abs < 5.0:
		return "Off"
	default:
		return "Poor"
	}
}

func narrative(delta float64) string {
	switch {
	case delta > 0.05:
		return "warmer than forecast"
	case delta < -0.05:
		return "cooler than forecast"
	default:
		return "matches forecast"
	}
}

func storageToMonitorAirport(a storage.Airport) Airport {
	return Airport{
		ICAO:      a.ICAO,
		City:      a.City,
		Latitude:  a.Lat,
		Longitude: a.Lon,
		Timezone:  a.Timezone,
	}
}

func (e *Engine) ensureState(icao string, apt Airport) {
	if _, ok := e.states[icao]; !ok {
		loc, err := time.LoadLocation(apt.Timezone)
		if err != nil {
			loc = time.UTC
		}
		now := time.Now().In(loc)
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		e.states[icao] = &WeatherState{
			DailyHigh:   math.Inf(-1),
			DailyLow:    math.Inf(1),
			TrackingDay: midnight,
		}
	}
	if _, ok := e.pressure[icao]; !ok {
		e.pressure[icao] = NewPressureTracker(60)
	}
}

func (e *Engine) hasRawChanged(icao, raw string) bool {
	e.lastRawMu.RLock()
	defer e.lastRawMu.RUnlock()
	prev, exists := e.lastRaw[icao]
	if !exists {
		return true
	}
	return prev != raw
}

func (e *Engine) setLastRaw(icao, raw string) {
	e.lastRawMu.Lock()
	defer e.lastRawMu.Unlock()
	e.lastRaw[icao] = raw
}

func (e *Engine) sendToChat(chatID int64, msg string) {
	chat := &tele.Chat{ID: chatID}
	_, err := e.bot.Send(chat, msg, &tele.SendOptions{
		ParseMode:             tele.ModeMarkdown,
		DisableWebPagePreview: true,
	})
	if err != nil {
		log.Printf("[engine] send to %d: %v", chatID, err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Purge loop
// ═══════════════════════════════════════════════════════════════════════════

func (e *Engine) purgeLoop() {
	defer e.wg.Done()
	e.purge()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.purge()
		case <-e.stopChan:
			return
		}
	}
}

func (e *Engine) purge() {
	deleted, err := storage.PurgeOlderThan(e.db, e.cfg.PurgeAge)
	if err != nil {
		log.Printf("[engine] purge logs: %v", err)
	} else if deleted > 0 {
		log.Printf("[engine] purged %d old weather logs", deleted)
	}

	fcDeleted, fcErr := storage.PurgeForecastCache(e.db, 24*time.Hour)
	if fcErr != nil {
		log.Printf("[engine] purge cache: %v", fcErr)
	} else if fcDeleted > 0 {
		log.Printf("[engine] purged %d stale cache entries", fcDeleted)
	}
}
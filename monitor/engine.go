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
// Configuration
// ═══════════════════════════════════════════════════════════════════════════

const (
	ForecastCacheTTL = 3 * time.Hour
	RetentionPeriod  = 30 * 24 * time.Hour // 30 days
)

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
		PurgeAge:     RetentionPeriod,
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
	e := NewEngine(db, bot, cfg)
	e.Start()
	return e
}

func (e *Engine) Start() {
	e.wg.Add(1)
	go e.pollLoop()

	e.wg.Add(1)
	go e.purgeLoop()

	log.Printf("[engine] started (poll:%s, retention:%s, cache TTL:%s)",
		e.cfg.PollInterval, e.cfg.PurgeAge, ForecastCacheTTL)
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
// Per-airport processing — EVENT-DRIVEN LOGGING
//
// The DB log is written ONLY when the METAR text changes (i.e., when
// a notification would be triggered). This gives us one record per
// actual weather observation rather than one per poll cycle.
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

	// ── Early exit: has the raw METAR actually changed? ─────────────────

	metarChanged := e.hasRawChanged(apt.ICAO, obs.Raw)
	if !metarChanged {
		// Nothing new — update in-memory state quietly and return.
		e.ensureState(apt.ICAO, monitorApt)
		_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)
		if hpa, ok := ParsePressure(obs.Raw); ok {
			e.pressure[apt.ICAO].Record(time.Now(), hpa)
		}
		return
	}

	// METAR changed — proceed with full processing.
	e.setLastRaw(apt.ICAO, obs.Raw)

	// ── Step B (CONDITIONAL): Smart Forecast Fetch ──────────────────────

	ff, forecastSource := e.getOrFetchForecast(ctx, apt.ICAO, monitorApt)

	// ── Step C: Calculate delta (Forecast − Reality) ────────────────────

	var forecastTemp, delta float64
	var condition string

	if ff != nil && ff.CurrentHour != nil {
		forecastTemp = ff.CurrentHour.TempCelsius
		delta = forecastTemp - obs.TempCelsius // positive = warm bias
		condition = classifyDelta(delta)
	} else {
		condition = "NoForecast"
	}

	// ── Step D: Derive contextual flags from METAR ──────────────────────

	windSpeedMS := KnotsToMS(obs.WindSpeed)
	isRaining := hasRainOrShowers(obs.PresentWeather)
	isFoggy := hasFogOrMist(obs.PresentWeather)

	var directRadiation float64
	if ff != nil && ff.CurrentExtended != nil {
		directRadiation = ff.CurrentExtended.DirectRadiation
	}

	// ── Step E: Sensor bias check ───────────────────────────────────────

	var sensorWarnings []SensorWarning
	var currentExt *HourlyExtended
	if ff != nil {
		currentExt = ff.CurrentExtended
	}
	sensorWarnings = CheckSensorBias(obs, currentExt)

	// ── Step F: Update in-memory state ──────────────────────────────────

	e.ensureState(apt.ICAO, monitorApt)

	if hpa, ok := ParsePressure(obs.Raw); ok {
		e.pressure[apt.ICAO].Record(time.Now(), hpa)
	}

	_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)

	// ── Step G (EVENT-DRIVEN): Save to DB only on change ────────────────

	wlog := &storage.WeatherLog{
		AirportICAO:     apt.ICAO,
		Timestamp:       time.Now().UTC(),
		ForecastTemp:    forecastTemp,
		RealTemp:        obs.TempCelsius,
		Delta:           delta,
		Condition:       condition,
		IsSpeci:         obs.IsSpeci,
		WindSpeed:       windSpeedMS,
		DirectRadiation: directRadiation,
		IsRaining:       isRaining,
		IsFoggy:         isFoggy,
		RawMETAR:        obs.Raw,
	}

	if err := storage.InsertWeatherLog(e.db, wlog); err != nil {
		log.Printf("[engine][%s] DB insert: %v", apt.ICAO, err)
	}

	// ── Step H (CONDITIONAL): Telegram notification ─────────────────────

	if apt.IsMuted {
		label := "METAR"
		if obs.IsSpeci {
			label = "⚡SPECI"
		}
		log.Printf("[engine][%s] %s logged (muted, no notify)", apt.ICAO, label)
		return
	}

	snapshot := e.states[apt.ICAO].Snapshot()
	msg := buildFullMessage(apt, obs, ff, snapshot, e.pressure[apt.ICAO],
		sensorWarnings, forecastSource)

	for _, chatID := range e.cfg.ChatIDs {
		e.sendToChat(chatID, msg)
	}

	label := "METAR"
	if obs.IsSpeci {
		label = "⚡SPECI"
	}
	log.Printf("[engine][%s] %s logged+notified (%.1f°C, Δ%+.1f°C, %s, src:%s, warn:%d)",
		apt.ICAO, label, obs.TempCelsius, delta, condition,
		forecastSource, len(sensorWarnings))
}

// ═══════════════════════════════════════════════════════════════════════════
// Smart forecast caching
// ═══════════════════════════════════════════════════════════════════════════

func (e *Engine) getOrFetchForecast(
	ctx context.Context, icao string, apt Airport,
) (*FullForecast, string) {

	cached, err := storage.GetForecastCache(e.db, icao)
	if err == nil && storage.IsForecastCacheFresh(cached, ForecastCacheTTL) {
		ff, parseErr := ParseForecastJSON(cached.ResponseJSON, apt)
		if parseErr == nil {
			ff.FromCache = true
			return ff, "cache"
		}
		log.Printf("[engine][%s] cache parse: %v", icao, parseErr)
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

	_ = storage.UpsertForecastCache(e.db, icao, rawJSON)

	ff, parseErr := ParseForecastJSON(rawJSON, apt)
	if parseErr != nil {
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
	b.WriteString(")*\n🕐 ")
	b.WriteString(localNow.Format("Mon, 02 Jan 2006 15:04 MST"))
	if obs.IsSpeci {
		b.WriteString("  ⚡️")
	}
	b.WriteString("\n\n")

	// ── REAL-TIME CONDITIONS ────────────────────────────────────────────

	b.WriteString("📡 *REAL-TIME CONDITIONS*\n```\n")
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
	b.WriteString(")\n```\n")
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
		delta := ff.CurrentHour.TempCelsius - obs.TempCelsius
		cacheTag := ""
		if ff.FromCache {
			cacheTag = fmt.Sprintf(" (%s)", forecastSource)
		}
		biasLabel := "neutral"
		if delta > 0.05 {
			biasLabel = "warm bias"
		} else if delta < -0.05 {
			biasLabel = "cool bias"
		}
		b.WriteString("```\n")
		fmt.Fprintf(&b, "Forecast    : %s%s\n", FormatTemp(ff.CurrentHour.TempCelsius), cacheTag)
		fmt.Fprintf(&b, "Reality     : %s\n", FormatTemp(obs.TempCelsius))
		fmt.Fprintf(&b, "Delta       : %s\n", FormatDelta(delta))
		fmt.Fprintf(&b, "Verdict     : %s (%s)\n", classifyDelta(delta), biasLabel)
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
				localHour.Format("15:04"), FormatTemp(hp.TempCelsius))
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

	// ── SENSOR QA ───────────────────────────────────────────────────────

	if len(sensorWarnings) > 0 {
		b.WriteString("\n⚠️ *SENSOR QA*\n```\n")
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
// Engine-local helpers (FormatTemp, FormatDelta, tempSign → analyze.go)
// ═══════════════════════════════════════════════════════════════════════════

func fmtWind(obs *Observation) string {
	if obs.WindSpeed == 0 && obs.WindGust == 0 {
		return "Calm"
	}
	dir := "VRB"
	if obs.WindDir >= 0 {
		dir = fmt.Sprintf("%03d°", obs.WindDir)
	}
	s := fmt.Sprintf("%s @ %dkt (%.1f m/s)", dir, obs.WindSpeed, KnotsToMS(obs.WindSpeed))
	if obs.WindGust > 0 {
		s += fmt.Sprintf(", gusting %dkt", obs.WindGust)
	}
	return s
}

func classifyDelta(delta float64) string {
	ad := math.Abs(delta)
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

func narrative(delta float64) string {
	switch {
	case delta > 0.05:
		return "warm bias"
	case delta < -0.05:
		return "cool bias"
	default:
		return "neutral"
	}
}

// hasFogOrMist checks for FG or BR in present weather.
func hasFogOrMist(wx []string) bool {
	for _, w := range wx {
		u := strings.ToUpper(w)
		if strings.Contains(u, "FG") || strings.Contains(u, "BR") {
			return true
		}
	}
	return false
}

func storageToMonitorAirport(a storage.Airport) Airport {
	return Airport{
		ICAO: a.ICAO, City: a.City,
		Latitude: a.Lat, Longitude: a.Lon,
		Timezone: a.Timezone,
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
			DailyHigh: math.Inf(-1), DailyLow: math.Inf(1),
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
	return !exists || prev != raw
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
// Purge loop — daily cleanup, 30-day retention
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
		log.Printf("[engine] purged %d logs older than %s", deleted, e.cfg.PurgeAge)
	}

	fc, fcErr := storage.PurgeForecastCache(e.db, 24*time.Hour)
	if fcErr != nil {
		log.Printf("[engine] purge cache: %v", fcErr)
	} else if fc > 0 {
		log.Printf("[engine] purged %d stale forecast cache entries", fc)
	}
}
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

	log.Println("[engine] monitoring started")
}

func (e *Engine) Stop() {
	close(e.stopChan)
	e.wg.Wait()
	log.Println("[engine] monitoring stopped")
}

func (e *Engine) RunOnce() {
	e.tick()
}

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

	// ── 1. Fetch METAR ──────────────────────────────────────────────────

	obs, err := FetchMETAR(ctx, apt.ICAO)
	if err != nil {
		log.Printf("[engine][%s] METAR: %v", apt.ICAO, err)
		return
	}

	// ── 2. Fetch FULL forecast (hourly array + daily max/min) ───────────

	monitorApt := storageToMonitorAirport(apt)

	var ff *FullForecast
	ff, err = FetchFullForecast(ctx, monitorApt)
	if err != nil {
		log.Printf("[engine][%s] forecast (non-fatal): %v", apt.ICAO, err)
	}

	// ── 3. Calculate delta ──────────────────────────────────────────────

	var forecastTemp, delta float64
	var condition string

	if ff != nil && ff.CurrentHour != nil {
		forecastTemp = ff.CurrentHour.TempCelsius
		delta = obs.TempCelsius - forecastTemp
		condition = classifyDelta(delta)
	} else {
		condition = "NoForecast"
	}

	// ── 4. Update in-memory state ───────────────────────────────────────

	e.ensureState(apt.ICAO, monitorApt)

	if hpa, ok := ParsePressure(obs.Raw); ok {
		e.pressure[apt.ICAO].Record(time.Now(), hpa)
	}

	_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)

	// ── 5. ALWAYS save to DB ────────────────────────────────────────────

	wlog := &storage.WeatherLog{
		AirportICAO:  apt.ICAO,
		Timestamp:    time.Now().UTC(),
		ForecastTemp: forecastTemp,
		RealTemp:     obs.TempCelsius,
		Delta:        delta,
		Condition:    condition,
	}

	if err := storage.InsertWeatherLog(e.db, wlog); err != nil {
		log.Printf("[engine][%s] DB insert: %v", apt.ICAO, err)
	}

	// ── 6. CONDITIONALLY notify ─────────────────────────────────────────

	if apt.IsMuted {
		return
	}

	if !e.hasRawChanged(apt.ICAO, obs.Raw) {
		return
	}

	e.setLastRaw(apt.ICAO, obs.Raw)

	snapshot := e.states[apt.ICAO].Snapshot()
	msg := buildFullMessage(apt, obs, ff, snapshot, e.pressure[apt.ICAO])

	for _, chatID := range e.cfg.ChatIDs {
		e.sendToChat(chatID, msg)
	}

	log.Printf("[engine][%s] notified (%.1f°C, Δ %.1f°C, %s)",
		apt.ICAO, obs.TempCelsius, delta, condition)
}

// ═══════════════════════════════════════════════════════════════════════════
// Full message builder (with Daily Outlook)
//
// NOTE: FormatTemp, FormatDelta, tempSign live in analyze.go
// ═══════════════════════════════════════════════════════════════════════════

func buildFullMessage(
	apt storage.Airport,
	obs *Observation,
	ff *FullForecast,
	snap WeatherSnapshot,
	pt *PressureTracker,
) string {
	var b strings.Builder
	b.Grow(1600)

	loc, _ := time.LoadLocation(apt.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	localNow := time.Now().In(loc)

	// ── HEADER ──────────────────────────────────────────────────────────

	b.WriteString("🛫 *")
	b.WriteString(apt.City)
	b.WriteString(" (")
	b.WriteString(apt.ICAO)
	b.WriteString(")*\n")
	b.WriteString("🕐 ")
	b.WriteString(localNow.Format("Mon, 02 Jan 2006 15:04 MST"))
	b.WriteString("\n\n")

	// ── REAL-TIME CONDITIONS ────────────────────────────────────────────

	b.WriteString("📡 *REAL-TIME CONDITIONS*\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "Temperature : %s\n", FormatTemp(obs.TempCelsius))
	fmt.Fprintf(&b, "Wind        : %s\n", fmtWind(obs))
	fmt.Fprintf(&b, "Visibility  : %s\n", obs.Visibility)

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
		b.WriteString("```\n")
		fmt.Fprintf(&b, "Forecast    : %s\n", FormatTemp(ff.CurrentHour.TempCelsius))
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

	// ── RAW METAR ───────────────────────────────────────────────────────

	b.WriteString("\n📋 `")
	b.WriteString(obs.Raw)
	b.WriteString("`")

	return b.String()
}

// ═══════════════════════════════════════════════════════════════════════════
// Engine-local helpers (NOT duplicated from analyze.go)
// ═══════════════════════════════════════════════════════════════════════════

// fmtWind formats wind for the message block.
// Named differently from formatWind in analyze.go to avoid collision
// if both files evolve independently.
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

// classifyDelta maps abs(delta) to an accuracy label.
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

// narrative returns a directional description.
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

// ═══════════════════════════════════════════════════════════════════════════
// Conversion helpers
// ═══════════════════════════════════════════════════════════════════════════

func storageToMonitorAirport(a storage.Airport) Airport {
	return Airport{
		ICAO:      a.ICAO,
		City:      a.City,
		Latitude:  a.Lat,
		Longitude: a.Lon,
		Timezone:  a.Timezone,
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// State helpers
// ═══════════════════════════════════════════════════════════════════════════

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
		log.Printf("[engine] purge: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[engine] purged %d old logs", deleted)
	}
}
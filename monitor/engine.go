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
// Constants
// ═══════════════════════════════════════════════════════════════════════════

const (
	ForecastCacheTTL = 3 * time.Hour
	RetentionPeriod  = 30 * 24 * time.Hour
)

// ═══════════════════════════════════════════════════════════════════════════
// Configuration
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

	// lastRaw stores the most recently seen raw METAR string per airport.
	// A new WeatherLog is only created when this value changes, which
	// prevents duplicate rows when the poll interval is shorter than the
	// METAR publication cadence (~30-60 min for routine, instant for SPECI).
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
// Per-airport processing — event-driven (METAR-change only) logging
// ═══════════════════════════════════════════════════════════════════════════
//
// Dedup strategy:
//   1. Fetch the latest METAR from NOAA.
//   2. Compare the FULL raw string against the last-seen value.
//   3. If IDENTICAL → update in-memory state/pressure only, skip DB + Telegram.
//   4. If DIFFERENT → log to DB, send notification, update last-seen.
//
// This means the DB only gets a new row when an actual new observation is
// published. Typical METAR cadence is every 30-60 minutes per airport, so
// with 11 airports you get ~350-530 rows/day instead of ~15,000.

func (e *Engine) processAirport(apt storage.Airport) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.FetchTimeout)
	defer cancel()

	monitorApt := storageToMonitorAirport(apt)

	// ── Fetch METAR directly from NOAA ──────────────────────────────────

	obs, err := FetchMETAR(ctx, apt.ICAO)
	if err != nil {
		log.Printf("[engine][%s] METAR: %v", apt.ICAO, err)
		return
	}

	// ── Dedup: skip if the raw METAR string is unchanged ────────────────
	//
	// Even state + pressure are updated so tracking stays current,
	// but NO database row is written and NO Telegram message is sent.

	if !e.hasRawChanged(apt.ICAO, obs.Raw) {
		e.ensureState(apt.ICAO, monitorApt)
		_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)
		if obs.HasPressure {
			e.pressure[apt.ICAO].Record(time.Now(), obs.PressureHpa)
		}
		return // ← nothing new, skip DB + notification
	}

	// ── New observation detected — proceed with full processing ─────────

	e.setLastRaw(apt.ICAO, obs.Raw)

	// ── Forecast (with 3-hour caching) ──────────────────────────────────

	ff, forecastSource := e.getOrFetchForecast(ctx, apt.ICAO, monitorApt)

	// ── Delta = Reality − Forecast ──────────────────────────────────────

	var forecastTemp, delta float64
	var condition string

	if ff != nil && ff.CurrentHour != nil {
		forecastTemp = ff.CurrentHour.TempCelsius
		delta = CalculateDelta(obs.TempCelsius, forecastTemp)
		condition = ClassifyDelta(delta)
	} else {
		condition = "NoForecast"
	}

	// ── Context fields for bias analysis ────────────────────────────────

	windSpeedMS := KnotsToMS(obs.WindSpeed)
	isRaining := hasRainOrShowers(obs.PresentWeather)
	isFoggy := hasFogOrMist(obs.PresentWeather)

	var directRadiation float64
	if ff != nil && ff.CurrentExtended != nil {
		directRadiation = ff.CurrentExtended.DirectRadiation
	}

	// ── Sensor QA warnings ──────────────────────────────────────────────

	var currentExt *HourlyExtended
	if ff != nil {
		currentExt = ff.CurrentExtended
	}
	sensorWarnings := CheckSensorBias(obs, currentExt)

	// ── Update in-memory state + pressure ───────────────────────────────

	e.ensureState(apt.ICAO, monitorApt)

	if obs.HasPressure {
		e.pressure[apt.ICAO].Record(time.Now(), obs.PressureHpa)
	}

	_ = e.states[apt.ICAO].Update(obs.TempCelsius, monitorApt)

	// ── Persist to DB (only reached when METAR changed) ─────────────────
	//
	// Delta stored as Reality − Forecast so downstream analytics
	// (Excel, bias reports) interpret the sign consistently:
	//   + means reality warmer than forecast
	//   - means reality cooler than forecast

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

	// ── Skip notification for muted airports ────────────────────────────

	if apt.IsMuted {
		label := "METAR"
		if obs.IsSpeci {
			label = "⚡SPECI"
		}
		log.Printf("[engine][%s] %s logged (muted, skipping notification)", apt.ICAO, label)
		return
	}

	// ── Build and send Telegram message ─────────────────────────────────

	snapshot := e.states[apt.ICAO].Snapshot()
	msg := buildFullMessage(apt, obs, ff, snapshot, e.pressure[apt.ICAO],
		sensorWarnings, forecastSource)

	for _, chatID := range e.cfg.ChatIDs {
		e.sendToChat(chatID, msg)
	}

	// ── Structured log ──────────────────────────────────────────────────

	label := "METAR"
	if obs.IsSpeci {
		label = "⚡SPECI"
	}
	precTag := ""
	if obs.IsPrecise {
		precTag = " T-Group"
	}
	log.Printf("[engine][%s] %s%s logged+sent (%.1f°C, Δ%+.1f°C [%s], %s, src:%s, warn:%d)",
		apt.ICAO, label, precTag,
		obs.TempCelsius, delta, DeltaVerdict(delta),
		condition, forecastSource, len(sensorWarnings))
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
// Full Telegram message builder
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
	b.Grow(2400)

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

	precLabel := ""
	if !obs.IsPrecise {
		precLabel = " [±1°C]"
	}
	fmt.Fprintf(&b, "Temperature : %s%s\n",
		FormatTempWithPrecision(obs.TempCelsius, obs.IsPrecise), precLabel)
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
	//
	// Delta = Reality − Forecast
	//   + means reality warmer than predicted
	//   - means reality colder than predicted

	b.WriteString("🎯 *FORECAST vs REALITY*\n")
	if ff != nil && ff.CurrentHour != nil {
		delta := CalculateDelta(obs.TempCelsius, ff.CurrentHour.TempCelsius)

		cacheTag := ""
		if ff.FromCache {
			cacheTag = fmt.Sprintf(" (%s)", forecastSource)
		}

		b.WriteString("```\n")
		fmt.Fprintf(&b, "Forecast    : %s%s\n",
			FormatTemp(ff.CurrentHour.TempCelsius), cacheTag)
		fmt.Fprintf(&b, "Reality     : %s\n",
			FormatTempWithPrecision(obs.TempCelsius, obs.IsPrecise))
		fmt.Fprintf(&b, "Delta       : %s\n", FormatDelta(delta))
		fmt.Fprintf(&b, "Verdict     : %s\n", FormatVerdict(delta))
		fmt.Fprintf(&b, "Accuracy    : %s\n", ClassifyDelta(delta))
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

	// ── TOMORROW ────────────────────────────────────────────────────────

	if ff != nil && ff.HasTomorrowExtremes {
		tomorrow := localNow.AddDate(0, 0, 1)
		b.WriteString("\n🌅 *TOMORROW* (")
		b.WriteString(tomorrow.Format("Mon, 02 Jan"))
		b.WriteString(")\n```\n")
		fmt.Fprintf(&b, "Max : %s\n", FormatTemp(ff.TomorrowMax))
		fmt.Fprintf(&b, "Min : %s\n", FormatTemp(ff.TomorrowMin))

		if ff.HasDailyExtremes {
			diffMax := ff.TomorrowMax - ff.DailyMax
			diffMin := ff.TomorrowMin - ff.DailyMin

			maxArrow := "→"
			if diffMax > 0.5 {
				maxArrow = "↑ warmer"
			} else if diffMax < -0.5 {
				maxArrow = "↓ cooler"
			}

			minArrow := "→"
			if diffMin > 0.5 {
				minArrow = "↑ warmer"
			} else if diffMin < -0.5 {
				minArrow = "↓ cooler"
			}

			fmt.Fprintf(&b, "\nvs Today:\n")
			fmt.Fprintf(&b, "  High %+.1f°C (%s)\n", diffMax, maxArrow)
			fmt.Fprintf(&b, "  Low  %+.1f°C (%s)\n", diffMin, minArrow)
		}

		b.WriteString("```\n")
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
// Wind formatting (Telegram-specific, includes m/s conversion)
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

// ═══════════════════════════════════════════════════════════════════════════
// Internal state helpers
// ═══════════════════════════════════════════════════════════════════════════

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

// hasRawChanged returns true if the raw METAR string differs from the
// last-seen value for this airport, or if no previous value exists
// (first poll after startup).
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
// Purge loop — 30-day retention
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
		log.Printf("[engine] purged %d stale cache entries", fc)
	}
}

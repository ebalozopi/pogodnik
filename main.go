package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pogodnik/monitor"
	"pogodnik/services"
	"pogodnik/storage"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"
	tele "gopkg.in/telebot.v3"
)

// ═══════════════════════════════════════════════════════════════════════════
// Constants
// ═══════════════════════════════════════════════════════════════════════════

const (
	defaultPollSeconds = 300 // 5 minutes — respects Open-Meteo 10k/day limit
	defaultDBPath      = "pogodnik.db"
)

// ═══════════════════════════════════════════════════════════════════════════
// Configuration
// ═══════════════════════════════════════════════════════════════════════════

type Config struct {
	TelegramToken string
	ChatIDs       []int64
	DBPath        string
	PollInterval  time.Duration
	AVWXToken     string
}

func LoadConfig() (*Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	chatIDsRaw := os.Getenv("TELEGRAM_CHAT_IDS")
	if chatIDsRaw == "" {
		return nil, fmt.Errorf("TELEGRAM_CHAT_IDS is required")
	}

	var chatIDs []int64
	for _, s := range strings.Split(chatIDsRaw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid chat ID %q: %w", s, err)
		}
		chatIDs = append(chatIDs, id)
	}
	if len(chatIDs) == 0 {
		return nil, fmt.Errorf("at least one chat ID required")
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = defaultDBPath
	}

	pollSec, _ := strconv.Atoi(os.Getenv("POLL_INTERVAL_SECONDS"))
	if pollSec <= 0 {
		pollSec = defaultPollSeconds
	}

	return &Config{
		TelegramToken: token,
		ChatIDs:       chatIDs,
		DBPath:        dbPath,
		PollInterval:  time.Duration(pollSec) * time.Second,
		AVWXToken:     os.Getenv("AVWX_TOKEN"),
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Application
// ═══════════════════════════════════════════════════════════════════════════

type App struct {
	cfg    *Config
	db     *gorm.DB
	bot    *tele.Bot
	engine *monitor.Engine
}

func NewApp(cfg *Config) (*App, error) {
	// ── Database ────────────────────────────────────────────────────────

	db, err := storage.InitDB(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("init DB: %w", err)
	}
	log.Printf("✓ Database opened: %s", cfg.DBPath)

	seedList := services.HardcodedList()
	dbSeed := make([]storage.Airport, len(seedList))
	copy(dbSeed, seedList)
	if err := storage.SeedAirports(db, dbSeed); err != nil {
		return nil, fmt.Errorf("seed airports: %w", err)
	}
	log.Printf("✓ Seeded %d default airports", len(dbSeed))

	// ── Telegram Bot ────────────────────────────────────────────────────

	bot, err := tele.NewBot(tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("init bot: %w", err)
	}
	log.Println("✓ Telegram bot created")

	app := &App{
		cfg: cfg,
		db:  db,
		bot: bot,
	}

	app.registerHandlers()
	return app, nil
}

func (app *App) Run() error {
	engineCfg := monitor.DefaultEngineConfig()
	engineCfg.PollInterval = app.cfg.PollInterval
	engineCfg.ChatIDs = app.cfg.ChatIDs

	app.engine = monitor.NewEngine(app.db, app.bot, engineCfg)
	app.engine.Start()
	log.Println("✓ Monitoring engine started")

	go func() {
		log.Println("✓ Telegram bot polling started")
		app.bot.Start()
	}()

	for _, chatID := range app.cfg.ChatIDs {
		app.send(chatID, "🟢 *Pogodnik v2\\.0 Online*\n\nType /help for commands\\.")
	}

	log.Printf("✓ All systems operational (poll every %s)", app.cfg.PollInterval)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	received := <-sig
	log.Printf("Received %v — shutting down…", received)

	return app.Shutdown()
}

func (app *App) Shutdown() error {
	for _, chatID := range app.cfg.ChatIDs {
		app.send(chatID, "🔴 *Pogodnik Shutting Down*")
	}

	app.engine.Stop()
	app.bot.Stop()

	sqlDB, err := app.db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}

	log.Println("✓ Shutdown complete")
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Command handlers
// ═══════════════════════════════════════════════════════════════════════════

func (app *App) registerHandlers() {

	// ── /start & /help ──────────────────────────────────────────────────

	welcomeHandler := func(c tele.Context) error {
		text := "🌤 *Pogodnik v2\\.0 — Aviation Weather Monitor*\n" +
			"━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +

			"✈️  *Airport Management*\n" +
			"├ /add `ICAO` — Add a new airport\n" +
			"│   _Example:_ `/add UUEE`\n" +
			"├ /del `ICAO` — Remove airport completely\n" +
			"│   _Stops fetching \\& deletes from DB_\n" +
			"├ /list — Show all monitored airports\n" +
			"│   _Displays mute status for each_\n" +
			"└ /check `ICAO` — Force fetch weather now\n" +
			"    _Example:_ `/check KORD`\n\n" +

			"🔔  *Notifications*\n" +
			"├ /mute `ICAO` — Silence notifications\n" +
			"│   _Data still collected, no messages_\n" +
			"└ /unmute `ICAO` — Resume notifications\n\n" +

			"📊  *Reports*\n" +
			"└ /export — Download Excel statistics\n" +
			"    _All recorded observations as \\.xlsx_\n\n" +

			"ℹ️  *System*\n" +
			"├ /status — Bot health \\& stats\n" +
			"└ /help — This message\n\n" +

			"━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
			fmt.Sprintf("💡 _Poll interval: %s_\n", escapeMarkdownV2(app.cfg.PollInterval.String())) +
			"_/mute keeps data, /del stops everything\\._"

		return c.Send(text, &tele.SendOptions{ParseMode: tele.ModeMarkdownV2})
	}

	app.bot.Handle("/start", welcomeHandler)
	app.bot.Handle("/help", welcomeHandler)

	// ── /add [ICAO] ─────────────────────────────────────────────────────

	app.bot.Handle("/add", func(c tele.Context) error {
		icao := extractICAO(c)
		if icao == "" {
			return c.Send("Usage: /add ICAO\nExample: /add EGLL")
		}

		existing, _ := storage.GetAirport(app.db, icao)
		if existing != nil {
			return c.Send(fmt.Sprintf("ℹ️ %s (%s) is already monitored.", existing.ICAO, existing.City))
		}

		_ = c.Send(fmt.Sprintf("🔍 Looking up %s…", icao))

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		apt, err := services.LookupAirport(ctx, icao, app.cfg.AVWXToken)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Could not find airport %s:\n%v", icao, err))
		}

		if err := app.db.Create(apt).Error; err != nil {
			return c.Send(fmt.Sprintf("❌ Database error: %v", err))
		}

		return c.Send(fmt.Sprintf("✅ Added *%s* (%s)\n📍 %.4f, %.4f\n🕐 %s",
			apt.City, apt.ICAO, apt.Lat, apt.Lon, apt.Timezone),
			&tele.SendOptions{ParseMode: tele.ModeMarkdown})
	})

	// ── /del [ICAO] ─────────────────────────────────────────────────────

	app.bot.Handle("/del", func(c tele.Context) error {
		icao := extractICAO(c)
		if icao == "" {
			return c.Send("Usage: /del ICAO\nExample: /del UUEE")
		}

		apt, err := storage.GetAirport(app.db, icao)
		if err != nil || apt == nil {
			return c.Send(fmt.Sprintf("🤷‍♂️ Airport %s not found in monitoring list.", icao))
		}

		city := apt.City

		if err := app.db.Where("airport_icao = ?", icao).Delete(&storage.WeatherLog{}).Error; err != nil {
			log.Printf("[del] failed to delete logs for %s: %v", icao, err)
		}

		if err := app.db.Where("icao = ?", icao).Delete(&storage.Airport{}).Error; err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to delete %s: %v", icao, err))
		}

		log.Printf("[del] removed airport %s (%s) and its logs", icao, city)

		return c.Send(
			fmt.Sprintf("🗑 Airport *%s* (%s) removed\\.\n_All associated weather logs deleted\\._",
				escapeMarkdownV2(city), icao),
			&tele.SendOptions{ParseMode: tele.ModeMarkdownV2},
		)
	})

	// ── /list ───────────────────────────────────────────────────────────

	app.bot.Handle("/list", func(c tele.Context) error {
		airports, err := storage.ListAirports(app.db)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ %v", err))
		}

		if len(airports) == 0 {
			return c.Send("📭 No airports monitored.\nUse /add ICAO to add one.")
		}

		var b strings.Builder
		b.WriteString("📋 *Monitored Airports*\n\n")

		for i, apt := range airports {
			status := "🟢"
			tag := ""
			if apt.IsMuted {
				status = "🔇"
				tag = " \\[MUTED\\]"
			}
			b.WriteString(fmt.Sprintf(
				"%d\\. %s `%s` — %s%s\n",
				i+1,
				status,
				apt.ICAO,
				escapeMarkdownV2(apt.City),
				tag,
			))
		}

		b.WriteString(fmt.Sprintf("\nTotal: %d airports", len(airports)))

		return c.Send(b.String(), &tele.SendOptions{ParseMode: tele.ModeMarkdownV2})
	})

	// ── /mute [ICAO] ───────────────────────────────────────────────────

	app.bot.Handle("/mute", func(c tele.Context) error {
		icao := extractICAO(c)
		if icao == "" {
			return c.Send("Usage: /mute ICAO")
		}

		if err := storage.SetMuted(app.db, icao, true); err != nil {
			return c.Send(fmt.Sprintf("❌ %v", err))
		}

		return c.Send(
			fmt.Sprintf("🔇 %s is now *muted*\\.\n_Data still collected, notifications paused\\._", icao),
			&tele.SendOptions{ParseMode: tele.ModeMarkdownV2},
		)
	})

	// ── /unmute [ICAO] ─────────────────────────────────────────────────

	app.bot.Handle("/unmute", func(c tele.Context) error {
		icao := extractICAO(c)
		if icao == "" {
			return c.Send("Usage: /unmute ICAO")
		}

		if err := storage.SetMuted(app.db, icao, false); err != nil {
			return c.Send(fmt.Sprintf("❌ %v", err))
		}

		return c.Send(
			fmt.Sprintf("🔔 %s is now *unmuted*\\. Notifications resumed\\.", icao),
			&tele.SendOptions{ParseMode: tele.ModeMarkdownV2},
		)
	})

	// ── /check [ICAO] ──────────────────────────────────────────────────

	app.bot.Handle("/check", func(c tele.Context) error {
		icao := extractICAO(c)
		if icao == "" {
			return c.Send("Usage: /check ICAO\nExample: /check KORD")
		}

		apt, err := storage.GetAirport(app.db, icao)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Airport %s not found. Add it first with /add %s", icao, icao))
		}

		_ = c.Send(fmt.Sprintf("📡 Fetching weather for %s…", apt.ICAO))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		monitorApt := dbToMonitorAirport(*apt)

		// ── Fetch METAR from NOAA ───────────────────────────────────────

		obs, err := monitor.FetchMETAR(ctx, apt.ICAO)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ METAR fetch failed: %v", err))
		}

		// ── Fetch full forecast (includes daily highs) ──────────────────

		ff, _ := monitor.FetchFullForecastFromAPI(ctx, monitorApt)

		// ── Delta = Reality − Forecast ──────────────────────────────────

		var forecastTemp, delta float64
		var condition string

		if ff != nil && ff.CurrentHour != nil {
			forecastTemp = ff.CurrentHour.TempCelsius
			delta = monitor.CalculateDelta(obs.TempCelsius, forecastTemp)
			condition = monitor.ClassifyDelta(delta)
		} else {
			condition = "NoForecast"
		}

		// ── Daily forecast context for ATH analysis ─────────────────────

		var forecastDailyHigh, forecastNextHigh float64

		if ff != nil {
			if ff.HasDailyExtremes {
				forecastDailyHigh = ff.DailyMax
			}
			if ff.HasTomorrowExtremes {
				forecastNextHigh = ff.TomorrowMax
			}
		}

		// ── Save to DB ──────────────────────────────────────────────────

		wlog := &storage.WeatherLog{
			AirportICAO:       apt.ICAO,
			Timestamp:         time.Now().UTC(),
			ForecastTemp:      forecastTemp,
			RealTemp:          obs.TempCelsius,
			Delta:             delta,
			Condition:         condition,
			IsSpeci:           obs.IsSpeci,
			WindSpeed:         monitor.KnotsToMS(obs.WindSpeed),
			ForecastDailyHigh: forecastDailyHigh,
			ForecastNextHigh:  forecastNextHigh,
			RawMETAR:          obs.Raw,
		}
		_ = storage.InsertWeatherLog(app.db, wlog)

		// ── Build and send report ───────────────────────────────────────

		msg := formatCheckReport(apt, obs, ff, delta, condition)

		return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
	})

	// ── /export ─────────────────────────────────────────────────────────

	app.bot.Handle("/export", func(c tele.Context) error {
		_ = c.Send("📊 Generating report…")

		// Fetch ALL logs for the last 30 days — no row limit.
		logs, err := storage.Last30DaysLogs(app.db)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Database error: %v", err))
		}

		if len(logs) == 0 {
			return c.Send("📭 No weather data recorded yet.")
		}

		// Fetch airport list for timezone-aware grouping.
		airports, err := storage.ListAirports(app.db)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to load airports: %v", err))
		}

		xlFile, err := services.GenerateReport(logs, airports)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Report generation failed: %v", err))
		}
		defer func(xlFile *excelize.File) {
			_ = xlFile.Close()
		}(xlFile)

		buf, err := xlFile.WriteToBuffer()
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to write Excel: %v", err))
		}

		fileName := fmt.Sprintf("pogodnik_%s.xlsx", time.Now().Format("2006-01-02_15-04"))

		doc := &tele.Document{
			File:     tele.FromReader(buf),
			FileName: fileName,
			Caption:  fmt.Sprintf("📊 Weather report — %d records (30 days, 4 sheets)", len(logs)),
		}

		return c.Send(doc)
	})

	// ── /status ─────────────────────────────────────────────────────────

	app.bot.Handle("/status", func(c tele.Context) error {
		airports, _ := storage.ListAirports(app.db)

		var totalLogs int64
		app.db.Model(&storage.WeatherLog{}).Count(&totalLogs)

		var recentCount int64
		oneHourAgo := time.Now().Add(-1 * time.Hour)
		app.db.Model(&storage.WeatherLog{}).Where("timestamp > ?", oneHourAgo).Count(&recentCount)

		muted := 0
		for _, a := range airports {
			if a.IsMuted {
				muted++
			}
		}

		// Estimate daily API calls.
		pollsPerDay := 86400 / int(app.cfg.PollInterval.Seconds())
		metarCallsPerDay := pollsPerDay * len(airports)
		// Forecast cached for 3h → ~8 calls/day per airport max.
		forecastCallsPerDay := 8 * len(airports)

		status := fmt.Sprintf(`📊 *Pogodnik Status*

• Airports: %d (%d muted)
• Total logs: %d
• Logs (last hour): %d
• Poll interval: %s
• Est. METAR checks/day: ~%d
• Est. forecast API/day: ~%d
• Database: %s
• Logging: event-driven (on METAR change only)
• Report sheets: Raw Data, Bias, Daily Summary, ATH Analysis`,
			len(airports), muted,
			totalLogs,
			recentCount,
			app.cfg.PollInterval,
			metarCallsPerDay,
			forecastCallsPerDay,
			app.cfg.DBPath,
		)

		return c.Send(status, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// /check report builder
// ═══════════════════════════════════════════════════════════════════════════

func formatCheckReport(
	apt *storage.Airport,
	obs *monitor.Observation,
	ff *monitor.FullForecast,
	delta float64,
	condition string,
) string {
	var b strings.Builder
	b.Grow(1200)

	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := time.Now().In(loc)

	// ── Header ──────────────────────────────────────────────────────────

	if obs.IsSpeci {
		b.WriteString("⚡️ *SPECI*\n")
	}
	b.WriteString(fmt.Sprintf("🛫 *%s (%s)*\n", apt.City, apt.ICAO))
	b.WriteString(fmt.Sprintf("🕐 %s\n\n", localNow.Format("Mon, 02 Jan 2006 15:04 MST")))

	// ── Current Conditions ──────────────────────────────────────────────

	precLabel := ""
	if !obs.IsPrecise {
		precLabel = " [±1°C]"
	}

	b.WriteString("📡 *Current Conditions*\n```\n")
	b.WriteString(fmt.Sprintf("Temperature : %s%s\n",
		monitor.FormatTempWithPrecision(obs.TempCelsius, obs.IsPrecise), precLabel))

	if obs.WindSpeed == 0 && obs.WindGust == 0 {
		b.WriteString("Wind        : Calm\n")
	} else {
		dir := "VRB"
		if obs.WindDir >= 0 {
			dir = fmt.Sprintf("%03d°", obs.WindDir)
		}
		wind := fmt.Sprintf("%s @ %dkt (%.1f m/s)", dir, obs.WindSpeed,
			monitor.KnotsToMS(obs.WindSpeed))
		if obs.WindGust > 0 {
			wind += fmt.Sprintf(", gusting %dkt", obs.WindGust)
		}
		b.WriteString(fmt.Sprintf("Wind        : %s\n", wind))
	}

	b.WriteString(fmt.Sprintf("Visibility  : %s\n", obs.Visibility))

	if len(obs.PresentWeather) > 0 {
		b.WriteString(fmt.Sprintf("Weather     : %s\n",
			strings.Join(obs.PresentWeather, " ")))
	}

	if obs.HasPressure {
		b.WriteString(fmt.Sprintf("Pressure    : %.1f hPa\n", obs.PressureHpa))
	}

	if obs.IsSpeci {
		b.WriteString("⚡ Type       : SPECI (Special Obs)\n")
	}

	b.WriteString("```\n\n")

	// ── Forecast vs Reality ─────────────────────────────────────────────
	//
	// Delta = Reality − Forecast
	//   + means reality warmer
	//   - means reality colder

	if ff != nil && ff.CurrentHour != nil {
		b.WriteString("🎯 *Forecast vs Reality*\n```\n")
		b.WriteString(fmt.Sprintf("Forecast    : %s\n",
			monitor.FormatTemp(ff.CurrentHour.TempCelsius)))
		b.WriteString(fmt.Sprintf("Reality     : %s\n",
			monitor.FormatTempWithPrecision(obs.TempCelsius, obs.IsPrecise)))
		b.WriteString(fmt.Sprintf("Delta       : %s\n", monitor.FormatDelta(delta)))
		b.WriteString(fmt.Sprintf("Verdict     : %s\n", monitor.FormatVerdict(delta)))
		b.WriteString(fmt.Sprintf("Accuracy    : %s\n", condition))
		b.WriteString("```\n\n")
	} else {
		b.WriteString("_Forecast not available_\n\n")
	}

	// ── Daily Outlook ───────────────────────────────────────────────────

	if ff != nil && len(ff.Upcoming) > 0 {
		b.WriteString("🌤 *Hourly Outlook*\n```\n")
		for _, hp := range ff.Upcoming {
			localHour := hp.Time.In(loc)
			b.WriteString(fmt.Sprintf("%s : %s\n",
				localHour.Format("15:04"), monitor.FormatTemp(hp.TempCelsius)))
		}
		b.WriteString("```\n\n")
	}

	// ── Daily Highs / Lows ──────────────────────────────────────────────

	if ff != nil && ff.HasDailyExtremes {
		b.WriteString("📊 *Daily Extremes (Forecast)*\n```\n")
		b.WriteString(fmt.Sprintf("Today High  : %s\n", monitor.FormatTemp(ff.DailyMax)))
		b.WriteString(fmt.Sprintf("Today Low   : %s\n", monitor.FormatTemp(ff.DailyMin)))

		if ff.HasTomorrowExtremes {
			b.WriteString(fmt.Sprintf("Tmrw High   : %s\n", monitor.FormatTemp(ff.TomorrowMax)))
			b.WriteString(fmt.Sprintf("Tmrw Low    : %s\n", monitor.FormatTemp(ff.TomorrowMin)))

			diffMax := ff.TomorrowMax - ff.DailyMax
			maxArrow := "→"
			if diffMax > 0.5 {
				maxArrow = "↑ warmer"
			} else if diffMax < -0.5 {
				maxArrow = "↓ cooler"
			}

			b.WriteString(fmt.Sprintf("\nTomorrow vs Today:\n"))
			b.WriteString(fmt.Sprintf("  High %+.1f°C (%s)\n", diffMax, maxArrow))
		}

		b.WriteString("```\n\n")
	}

	// ── Raw METAR ───────────────────────────────────────────────────────

	b.WriteString(fmt.Sprintf("📋 `%s`", obs.Raw))

	return b.String()
}

// ═══════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════

func extractICAO(c tele.Context) string {
	payload := strings.TrimSpace(c.Message().Payload)
	if payload == "" {
		return ""
	}
	parts := strings.Fields(payload)
	icao := strings.ToUpper(parts[0])
	if len(icao) != 4 {
		return ""
	}
	return icao
}

func dbToMonitorAirport(a storage.Airport) monitor.Airport {
	return monitor.Airport{
		ICAO:      a.ICAO,
		City:      a.City,
		Latitude:  a.Lat,
		Longitude: a.Lon,
		Timezone:  a.Timezone,
	}
}

func escapeMarkdownV2(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
}

func (app *App) send(chatID int64, msg string) {
	_, err := app.bot.Send(
		&tele.Chat{ID: chatID},
		msg,
		&tele.SendOptions{
			ParseMode:             tele.ModeMarkdownV2,
			DisableWebPagePreview: true,
		},
	)
	if err != nil {
		log.Printf("[bot] send to %d: %v", chatID, err)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Entry point
// ═══════════════════════════════════════════════════════════════════════════

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("Pogodnik v2.0 starting…")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	app, err := NewApp(cfg)
	if err != nil {
		log.Fatalf("Init error: %v", err)
	}

	if err := app.Run(); err != nil {
		log.Fatalf("Runtime error: %v", err)
	}
}

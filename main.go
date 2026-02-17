package main

import (
	"context"
	"fmt"
	"log"
	"math"
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
	tele "gopkg.in/telebot.v3"
	"gorm.io/gorm"
	
	 _ "github.com/joho/godotenv/autoload"
)

// ═══════════════════════════════════════════════════════════════════════════
// Configuration
// ═══════════════════════════════════════════════════════════════════════════

type Config struct {
	TelegramToken string
	ChatIDs       []int64
	PollInterval  time.Duration
	DBPath        string
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

	pollSec, _ := strconv.Atoi(os.Getenv("POLL_INTERVAL_SECONDS"))
	if pollSec <= 0 {
		pollSec = 60
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "pogodnik.db"
	}

	return &Config{
		TelegramToken: token,
		ChatIDs:       chatIDs,
		PollInterval:  time.Duration(pollSec) * time.Second,
		DBPath:        dbPath,
		AVWXToken:     os.Getenv("AVWX_TOKEN"),
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Bot application
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
		return nil, fmt.Errorf("init db: %w", err)
	}
	log.Printf("✓ Database opened: %s", cfg.DBPath)

	// Seed the hardcoded airports so the bot works out of the box.
	if err := storage.SeedAirports(db, services.HardcodedList()); err != nil {
		return nil, fmt.Errorf("seed airports: %w", err)
	}
	log.Println("✓ Default airports seeded")

	// ── Telegram bot ────────────────────────────────────────────────────

	bot, err := tele.NewBot(tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
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

// Run starts the engine and bot, blocks until a signal is received.
func (app *App) Run() error {
	// ── Start monitoring engine ─────────────────────────────────────────

	app.engine = monitor.StartMonitoring(app.db, app.bot, app.cfg.ChatIDs)
	log.Println("✓ Monitoring engine started")

	// ── Start bot (non-blocking) ────────────────────────────────────────

	go func() {
		log.Println("✓ Telegram bot polling...")
		app.bot.Start()
	}()

	// ── Notify chats ────────────────────────────────────────────────────

	for _, chatID := range app.cfg.ChatIDs {
		app.sendToChat(chatID, "🟢 *Pogodnik v2\\.0 Online*\n\nType /help for commands\\.")
	}

	log.Println("✓ All systems operational")

	// ── Wait for shutdown ───────────────────────────────────────────────

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received %v, shutting down...", sig)

	return app.Shutdown()
}

// Shutdown gracefully stops all components.
func (app *App) Shutdown() error {
	for _, chatID := range app.cfg.ChatIDs {
		app.sendToChat(chatID, "🔴 *Pogodnik Shutting Down*")
	}

	app.bot.Stop()
	log.Println("✓ Bot stopped")

	app.engine.Stop()
	log.Println("✓ Engine stopped")

	sqlDB, err := app.db.DB()
	if err == nil {
		_ = sqlDB.Close()
		log.Println("✓ Database closed")
	}

	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Command handlers
// ═══════════════════════════════════════════════════════════════════════════

func (app *App) registerHandlers() {
	app.bot.Handle("/start", app.handleStart)
	app.bot.Handle("/help", app.handleStart)
	app.bot.Handle("/add", app.handleAdd)
	app.bot.Handle("/list", app.handleList)
	app.bot.Handle("/mute", app.handleMute)
	app.bot.Handle("/unmute", app.handleUnmute)
	app.bot.Handle("/check", app.handleCheck)
	app.bot.Handle("/export", app.handleExport)
	app.bot.Handle("/status", app.handleStatus)
}

// ── /start & /help ──────────────────────────────────────────────────────

func (app *App) handleStart(c tele.Context) error {
	text := `🌤 *Pogodnik v2\.0 — Aviation Weather Monitor*

Track real\-time weather at airports worldwide\.

*Commands:*
• /add ICAO — Add airport \(e\.g\. /add EGLL\)
• /list — Show monitored airports
• /check ICAO — Force weather check
• /mute ICAO — Mute notifications
• /unmute ICAO — Unmute notifications
• /export — Download Excel report
• /status — Bot status
• /help — This message`

	return c.Send(text, &tele.SendOptions{ParseMode: tele.ModeMarkdownV2})
}

// ── /add [ICAO] ─────────────────────────────────────────────────────────

func (app *App) handleAdd(c tele.Context) error {
	args := strings.Fields(c.Message().Payload)
	if len(args) == 0 {
		return c.Send("Usage: /add ICAO\nExample: /add EGLL")
	}

	icao := strings.ToUpper(strings.TrimSpace(args[0]))
	if len(icao) != 4 {
		return c.Send("❌ ICAO code must be exactly 4 characters.")
	}

	// Check if already exists.
	existing, err := storage.GetAirport(app.db, icao)
	if err == nil && existing != nil {
		return c.Send(fmt.Sprintf("ℹ️ Airport %s (%s) is already monitored.", existing.ICAO, existing.City))
	}

	_ = c.Send(fmt.Sprintf("🔍 Looking up %s...", icao))

	// Lookup via API or fallback.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	apt, err := services.LookupAirport(ctx, icao, app.cfg.AVWXToken)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Could not find airport %s:\n%v", icao, err))
	}

	// Save to database.
	if err := app.db.Create(apt).Error; err != nil {
		return c.Send(fmt.Sprintf("❌ Failed to save airport %s:\n%v", icao, err))
	}

	msg := fmt.Sprintf("✅ Added *%s* \\(%s\\)\n📍 %.4f, %.4f\n🕐 %s",
		escapeV2(apt.City),
		apt.ICAO,
		apt.Lat,
		apt.Lon,
		escapeV2(apt.Timezone),
	)
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeMarkdownV2})
}

// ── /list ───────────────────────────────────────────────────────────────

func (app *App) handleList(c tele.Context) error {
	airports, err := storage.ListAirports(app.db)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Database error: %v", err))
	}

	if len(airports) == 0 {
		return c.Send("No airports monitored yet. Use /add ICAO to add one.")
	}

	var b strings.Builder
	b.WriteString("🗺 *Monitored Airports*\n\n")

	for i, apt := range airports {
		status := "🟢"
		mutedTag := ""
		if apt.IsMuted {
			status = "🔇"
			mutedTag = " \\[MUTED\\]"
		}

		b.WriteString(fmt.Sprintf(
			"%s `%s` — %s%s\n",
			status,
			apt.ICAO,
			escapeV2(apt.City),
			mutedTag,
		))

		// Blank line every 5 airports for readability.
		if (i+1)%5 == 0 && i < len(airports)-1 {
			b.WriteByte('\n')
		}
	}

	b.WriteString(fmt.Sprintf("\n_Total: %d airports_", len(airports)))

	return c.Send(b.String(), &tele.SendOptions{ParseMode: tele.ModeMarkdownV2})
}

// ── /mute [ICAO] ───────────────────────────────────────────────────────

func (app *App) handleMute(c tele.Context) error {
	icao, err := extractICAO(c)
	if err != nil {
		return c.Send(err.Error())
	}

	if err := storage.SetMuted(app.db, icao, true); err != nil {
		return c.Send(fmt.Sprintf("❌ %v", err))
	}

	return c.Send(fmt.Sprintf("🔇 Airport %s is now *muted*. It will still be logged but no notifications will be sent.", icao),
		&tele.SendOptions{ParseMode: tele.ModeMarkdown})
}

// ── /unmute [ICAO] ──────────────────────────────────────────────────────

func (app *App) handleUnmute(c tele.Context) error {
	icao, err := extractICAO(c)
	if err != nil {
		return c.Send(err.Error())
	}

	if err := storage.SetMuted(app.db, icao, false); err != nil {
		return c.Send(fmt.Sprintf("❌ %v", err))
	}

	return c.Send(fmt.Sprintf("🔔 Airport %s is now *unmuted*. Notifications will resume.", icao),
		&tele.SendOptions{ParseMode: tele.ModeMarkdown})
}

// ── /check [ICAO] ───────────────────────────────────────────────────────

func (app *App) handleCheck(c tele.Context) error {
	icao, err := extractICAO(c)
	if err != nil {
		return c.Send(err.Error())
	}

	apt, err := storage.GetAirport(app.db, icao)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Airport %s not found in database. Use /add %s first.", icao, icao))
	}

	_ = c.Send(fmt.Sprintf("📡 Fetching weather for %s (%s)...", apt.ICAO, apt.City))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Convert to monitor.Airport for fetch functions.
	monApt := monitor.Airport{
		ICAO:      apt.ICAO,
		City:      apt.City,
		Latitude:  apt.Lat,
		Longitude: apt.Lon,
		Timezone:  apt.Timezone,
	}

	// Fetch METAR.
	obs, err := monitor.FetchMETAR(ctx, apt.ICAO)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ METAR fetch failed for %s:\n%v", icao, err))
	}

	// Fetch forecast (best-effort).
	fc, _ := monitor.FetchForecast(ctx, monApt)

	// Calculate delta and condition.
	var forecastTemp, delta float64
	var condition string
	if fc != nil {
		forecastTemp = fc.TempCelsius
		delta = obs.TempCelsius - fc.TempCelsius
		condition = classifyDelta(delta)
	} else {
		condition = "NoForecast"
	}

	// Save to DB (always).
	wlog := &storage.WeatherLog{
		AirportICAO:  apt.ICAO,
		Timestamp:    time.Now().UTC(),
		ForecastTemp: forecastTemp,
		RealTemp:     obs.TempCelsius,
		Delta:        delta,
		Condition:    condition,
	}
	_ = storage.InsertWeatherLog(app.db, wlog)

	// Build report message.
	msg := formatCheckReport(obs, fc, monApt, delta, condition)
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
}

// ── /export ─────────────────────────────────────────────────────────────

func (app *App) handleExport(c tele.Context) error {
	_ = c.Send("📊 Generating Excel report...")

	var logs []storage.WeatherLog
	err := app.db.
		Order("timestamp DESC").
		Limit(5000).
		Find(&logs).Error
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Database error: %v", err))
	}

	if len(logs) == 0 {
		return c.Send("ℹ️ No weather logs found. Wait for the first monitoring cycle.")
	}

	f, err := services.GenerateReport(logs)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Report generation failed: %v", err))
	}
	defer func() {
		_ = f.Close()
	}()

	buf, err := f.WriteToBuffer()
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Failed to write Excel buffer: %v", err))
	}

	filename := fmt.Sprintf("pogodnik_%s.xlsx", time.Now().UTC().Format("2006-01-02_1504"))

	doc := &tele.Document{
		File:     tele.FromReader(buf),
		FileName: filename,
		Caption:  fmt.Sprintf("📊 Weather report — %d records", len(logs)),
	}

	return c.Send(doc)
}

// ── /status ─────────────────────────────────────────────────────────────

func (app *App) handleStatus(c tele.Context) error {
	airports, _ := storage.ListAirports(app.db)

	var totalLogs int64
	app.db.Model(&storage.WeatherLog{}).Count(&totalLogs)

	var todayLogs int64
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	app.db.Model(&storage.WeatherLog{}).
		Where("timestamp >= ?", todayStart).
		Count(&todayLogs)

	muted := 0
	for _, a := range airports {
		if a.IsMuted {
			muted++
		}
	}

	var dbSize string
	if info, err := os.Stat(app.cfg.DBPath); err == nil {
		mb := float64(info.Size()) / 1024 / 1024
		dbSize = fmt.Sprintf("%.2f MB", mb)
	} else {
		dbSize = "unknown"
	}

	status := fmt.Sprintf(`📊 *Pogodnik Status*

• Airports: %d (%d muted)
• Total logs: %d
• Today's logs: %d
• DB size: %s
• Poll interval: %s
• Uptime: running`,
		len(airports), muted,
		totalLogs,
		todayLogs,
		dbSize,
		app.cfg.PollInterval,
	)

	return c.Send(status, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
}

// ═══════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════

// extractICAO pulls and validates the ICAO argument from a command.
func extractICAO(c tele.Context) (string, error) {
	args := strings.Fields(c.Message().Payload)
	if len(args) == 0 {
		return "", fmt.Errorf("Usage: %s ICAO\nExample: %s KORD", c.Message().Text, c.Message().Text)
	}

	icao := strings.ToUpper(strings.TrimSpace(args[0]))
	if len(icao) != 4 {
		return "", fmt.Errorf("❌ ICAO code must be exactly 4 characters.")
	}

	return icao, nil
}

// classifyDelta maps a Celsius delta to an accuracy label.
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

// formatCheckReport builds a one-off weather report for /check.
func formatCheckReport(obs *monitor.Observation, fc *monitor.HourlyForecast, apt monitor.Airport, delta float64, condition string) string {
	var b strings.Builder
	b.Grow(800)

	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := time.Now().In(loc)

	b.WriteString(fmt.Sprintf("🛫 *%s (%s)*\n", apt.City, apt.ICAO))
	b.WriteString(fmt.Sprintf("🕐 %s\n\n", localNow.Format("Mon, 02 Jan 2006 15:04 MST")))

	// Temperature
	tempF := monitor.CelsiusToFahrenheit(obs.TempCelsius)
	b.WriteString("📡 *Current Conditions*\n")
	b.WriteString("```\n")
	b.WriteString(fmt.Sprintf("Temperature : %s%.1f°C / %s%.1f°F\n",
		tempSign(obs.TempCelsius), obs.TempCelsius,
		tempSign(tempF), tempF))

	// Wind
	wind := "Calm"
	if obs.WindSpeed > 0 || obs.WindGust > 0 {
		dir := "VRB"
		if obs.WindDir >= 0 {
			dir = fmt.Sprintf("%03d°", obs.WindDir)
		}
		wind = fmt.Sprintf("%s @ %dkt", dir, obs.WindSpeed)
		if obs.WindGust > 0 {
			wind += fmt.Sprintf(", gusting %dkt", obs.WindGust)
		}
	}
	b.WriteString(fmt.Sprintf("Wind        : %s\n", wind))
	b.WriteString(fmt.Sprintf("Visibility  : %s\n", obs.Visibility))
	b.WriteString("```\n\n")

	// Forecast comparison
	b.WriteString("🎯 *Forecast vs Reality*\n")
	if fc != nil {
		fcF := monitor.CelsiusToFahrenheit(fc.TempCelsius)
		deltaF := delta * 9.0 / 5.0
		b.WriteString("```\n")
		b.WriteString(fmt.Sprintf("Forecast    : %s%.1f°C / %s%.1f°F\n",
			tempSign(fc.TempCelsius), fc.TempCelsius,
			tempSign(fcF), fcF))
		b.WriteString(fmt.Sprintf("Reality     : %s%.1f°C / %s%.1f°F\n",
			tempSign(obs.TempCelsius), obs.TempCelsius,
			tempSign(tempF), tempF))
		b.WriteString(fmt.Sprintf("Delta       : %s%.1f°C / %s%.1f°F\n",
			tempSign(delta), delta,
			tempSign(deltaF), deltaF))
		b.WriteString(fmt.Sprintf("Verdict     : %s\n", condition))
		b.WriteString("```\n")
	} else {
		b.WriteString("_Forecast not available_\n")
	}

	b.WriteString(fmt.Sprintf("\n📋 `%s`", obs.Raw))

	return b.String()
}

// tempSign returns "+" for non-negative, "" for negative (fmt adds "-").
func tempSign(v float64) string {
	if v < 0 {
		return ""
	}
	return "+"
}

// escapeV2 escapes special characters for Telegram MarkdownV2.
func escapeV2(s string) string {
	special := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, ch := range special {
		s = strings.ReplaceAll(s, ch, "\\"+ch)
	}
	return s
}

// sendToChat delivers a message to a chat ID.
func (app *App) sendToChat(chatID int64, msg string) {
	chat := &tele.Chat{ID: chatID}
	_, err := app.bot.Send(chat, msg, &tele.SendOptions{
		ParseMode:             tele.ModeMarkdownV2,
		DisableWebPagePreview: true,
	})
	if err != nil {
		log.Printf("send to %d: %v", chatID, err)
	}
}

// Ensure excelize import is used (compile guard).
var _ *excelize.File

// ═══════════════════════════════════════════════════════════════════════════
// Entry point
// ═══════════════════════════════════════════════════════════════════════════

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("Pogodnik v2.0 starting...")

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
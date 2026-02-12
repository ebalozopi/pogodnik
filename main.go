package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pogodnik/monitor"
	_"github.com/joho/godotenv/autoload"

	tele "gopkg.in/telebot.v3"
)

type Config struct {
	TelegramToken   string
	ChatIDs         []int64
	PollInterval    time.Duration
	ChangeThreshold float64
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

	threshold, _ := strconv.ParseFloat(os.Getenv("CHANGE_THRESHOLD_C"), 64)
	if threshold <= 0 {
		threshold = 0.5
	}

	return &Config{
		TelegramToken:   token,
		ChatIDs:         chatIDs,
		PollInterval:    time.Duration(pollSec) * time.Second,
		ChangeThreshold: threshold,
	}, nil
}

type PreviousObs struct {
	TempC      float64
	WindSpeed  int
	WindDir    int
	Visibility string
}

type ObsCache struct {
	mu    sync.RWMutex
	cache map[string]PreviousObs
}

func NewObsCache() *ObsCache {
	return &ObsCache{cache: make(map[string]PreviousObs)}
}

func (oc *ObsCache) Get(icao string) (PreviousObs, bool) {
	oc.mu.RLock()
	defer oc.mu.RUnlock()
	obs, ok := oc.cache[icao]
	return obs, ok
}

func (oc *ObsCache) Set(icao string, obs PreviousObs) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	oc.cache[icao] = obs
}

func (oc *ObsCache) HasSignificantChange(icao string, newObs *monitor.Observation, threshold float64) bool {
	prev, exists := oc.Get(icao)
	if !exists {
		return true
	}

	if math.Abs(newObs.TempCelsius-prev.TempC) >= threshold {
		return true
	}

	if abs(newObs.WindSpeed-prev.WindSpeed) >= 5 {
		return true
	}

	if prev.WindDir >= 0 && newObs.WindDir >= 0 {
		dirDelta := abs(newObs.WindDir - prev.WindDir)
		if dirDelta > 180 {
			dirDelta = 360 - dirDelta
		}
		if dirDelta >= 30 {
			return true
		}
	}

	if newObs.Visibility != prev.Visibility {
		return true
	}

	return false
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

type App struct {
	config   *Config
	bot      *tele.Bot
	airports map[string]monitor.Airport
	states   map[string]*monitor.WeatherState
	pressure map[string]*monitor.PressureTracker
	obsCache *ObsCache

	reportChan chan ReportRequest
	notifyChan chan *monitor.Report
	stopChan   chan struct{}
	wg         sync.WaitGroup
}

type ReportRequest struct {
	ChatID   int64
	SingleAP string
}

func NewApp(cfg *Config) (*App, error) {
	pref := tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	}

	bot, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	airports, states := monitor.InitConfig()
	airportList := make([]monitor.Airport, 0, len(airports))
	for _, a := range airports {
		airportList = append(airportList, a)
	}

	app := &App{
		config:     cfg,
		bot:        bot,
		airports:   airports,
		states:     states,
		pressure:   monitor.NewPressureTrackers(airportList, 60),
		obsCache:   NewObsCache(),
		reportChan: make(chan ReportRequest, 10),
		notifyChan: make(chan *monitor.Report, 50),
		stopChan:   make(chan struct{}),
	}

	app.registerHandlers()
	return app, nil
}

type HourlyOutlook struct {
	Hour        string
	TempDisplay string
}

type DailyOutlook struct {
	Hours        []HourlyOutlook
	ExpectedHigh string
	ExpectedLow  string
}

func FetchDailyOutlook(ctx context.Context, apt monitor.Airport) (*DailyOutlook, error) {
	times, temps, err := monitor.FetchExtendedForecast(ctx, apt)
	if err != nil {
		return nil, err
	}

	loc, err := time.LoadLocation(apt.Timezone)
	if err != nil {
		return nil, err
	}

	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	var outlook DailyOutlook
	var todayTemps []float64

	for i, ts := range times {
		if !strings.HasPrefix(ts, today) {
			continue
		}

		parsed, err := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		if err != nil {
			continue
		}

		if parsed.Before(now) {
			continue
		}

		if i < len(temps) {
			temp := temps[i]
			todayTemps = append(todayTemps, temp)

			if len(outlook.Hours) < 6 {
				outlook.Hours = append(outlook.Hours, HourlyOutlook{
					Hour:        parsed.Format("15:04"),
					TempDisplay: monitor.FormatTemp(temp),
				})
			}
		}
	}

	if len(todayTemps) > 0 {
		sort.Float64s(todayTemps)
		outlook.ExpectedLow = monitor.FormatTemp(todayTemps[0])
		outlook.ExpectedHigh = monitor.FormatTemp(todayTemps[len(todayTemps)-1])
	}

	return &outlook, nil
}

func FormatTelegramMessage(r *monitor.Report, forecast *DailyOutlook) string {
	var b strings.Builder
	b.Grow(1500)

	b.WriteString("🛫 *")
	b.WriteString(escapeMarkdown(r.Airport.City))
	b.WriteString(" (")
	b.WriteString(r.Airport.ICAO)
	b.WriteString(")*\n")
	b.WriteString("🕐 ")
	b.WriteString(r.LocalTime.Format("Mon, 02 Jan 2006 15:04 MST"))
	b.WriteString("\n\n")

	b.WriteString("📡 *REAL-TIME CONDITIONS*\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "Temperature : %s\n", r.Observation.TempDisplay)
	fmt.Fprintf(&b, "Wind        : %s\n", r.Observation.Wind)
	fmt.Fprintf(&b, "Visibility  : %s\n", r.Observation.Visibility)
	if r.Pressure.Available {
		fmt.Fprintf(&b, "Pressure    : %s\n", r.Pressure.Display)
	}
	b.WriteString("```\n\n")

	b.WriteString("📊 *DAY STATISTICS* (")
	b.WriteString(r.Extremes.TrackingDay.Format("02 Jan"))
	b.WriteString(")\n")
	b.WriteString("```\n")
	if math.IsInf(r.Extremes.HighC, 0) {
		b.WriteString("Awaiting first observation...\n")
	} else {
		fmt.Fprintf(&b, "High (ATH)  : %s\n", r.Extremes.HighDisplay)
		fmt.Fprintf(&b, "Low  (ATL)  : %s\n", r.Extremes.LowDisplay)
	}
	b.WriteString("```\n\n")

	b.WriteString("🎯 *FORECAST vs REALITY*\n")
	if r.Comparison.Available {
		b.WriteString("```\n")
		fmt.Fprintf(&b, "Forecast    : %s\n", r.Forecast.TempDisplay)
		fmt.Fprintf(&b, "Reality     : %s\n", r.Observation.TempDisplay)
		fmt.Fprintf(&b, "Delta       : %s\n", r.Comparison.DeltaDisplay)
		fmt.Fprintf(&b, "Verdict     : %s (%s)\n", r.Comparison.Accuracy, r.Comparison.Narrative)
		b.WriteString("```\n\n")
	} else {
		b.WriteString("_Forecast data not available_\n\n")
	}

	b.WriteString("🌤 *DAILY OUTLOOK*\n")
	if forecast != nil && len(forecast.Hours) > 0 {
		b.WriteString("```\n")
		for _, h := range forecast.Hours {
			fmt.Fprintf(&b, "%s : %s\n", h.Hour, h.TempDisplay)
		}
		if forecast.ExpectedHigh != "" {
			fmt.Fprintf(&b, "\nExp. High   : %s\n", forecast.ExpectedHigh)
			fmt.Fprintf(&b, "Exp. Low    : %s\n", forecast.ExpectedLow)
		}
		b.WriteString("```\n")
	} else {
		b.WriteString("_Outlook not available_\n")
	}

	b.WriteString("\n📋 _Raw METAR:_\n`")
	b.WriteString(escapeMarkdown(r.Observation.RawMETAR))
	b.WriteString("`")

	return b.String()
}

func escapeMarkdown(s string) string {
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

// handleWelcome is the shared handler for both /start and /help.
func (app *App) handleWelcome(c tele.Context) error {
	welcome := `🌤 *Aviation Weather Monitor*

Welcome! I track real-time weather conditions at major airports worldwide.

*Available Commands:*
• /report - Get current weather for all airports
• /airport ICAO - Get weather for specific airport
• /status - Check bot status
• /help - Show this help message

*Monitored Airports:*
` + app.getAirportList()

	return c.Send(welcome, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
}

func (app *App) registerHandlers() {
	// /start and /help both use the same handler
	app.bot.Handle("/start", app.handleWelcome)
	app.bot.Handle("/help", app.handleWelcome)

	app.bot.Handle("/report", func(c tele.Context) error {
		_ = c.Send("📡 Fetching weather data for all airports...")

		app.reportChan <- ReportRequest{
			ChatID:   c.Chat().ID,
			SingleAP: "",
		}
		return nil
	})

	app.bot.Handle("/airport", func(c tele.Context) error {
		args := strings.Fields(c.Message().Payload)
		if len(args) == 0 {
			return c.Send("Usage: /airport ICAO (e.g., /airport KORD)")
		}

		icao := strings.ToUpper(args[0])
		if _, exists := app.airports[icao]; !exists {
			return c.Send(fmt.Sprintf("❌ Airport %s is not monitored.\n\n%s", icao, app.getAirportList()))
		}

		_ = c.Send(fmt.Sprintf("📡 Fetching weather for %s...", icao))

		app.reportChan <- ReportRequest{
			ChatID:   c.Chat().ID,
			SingleAP: icao,
		}
		return nil
	})

	app.bot.Handle("/status", func(c tele.Context) error {
		var active int
		var lastUpdates []string

		for icao, state := range app.states {
			snap := state.Snapshot()
			if !snap.LastUpdated.IsZero() {
				active++
				age := time.Since(snap.LastUpdated).Round(time.Second)
				lastUpdates = append(lastUpdates, fmt.Sprintf("%s: %s ago", icao, age))
			}
		}

		sort.Strings(lastUpdates)

		status := fmt.Sprintf(`📊 *Bot Status*

• Monitored Airports: %d
• Active Data Feeds: %d
• Poll Interval: %s
• Change Threshold: %.1f°C

*Last Updates:*
%s`,
			len(app.airports),
			active,
			app.config.PollInterval,
			app.config.ChangeThreshold,
			strings.Join(lastUpdates, "\n"),
		)

		return c.Send(status, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
	})
}

func (app *App) getAirportList() string {
	var lines []string
	for icao, apt := range app.airports {
		lines = append(lines, fmt.Sprintf("• %s - %s", icao, apt.City))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (app *App) startAirportPoller(apt monitor.Airport) {
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()

		log.Printf("[%s] Starting poller", apt.ICAO)

		time.Sleep(time.Duration(len(apt.ICAO)*100) * time.Millisecond)
		app.pollAirport(apt, true)

		ticker := time.NewTicker(app.config.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				app.pollAirport(apt, false)
			case <-app.stopChan:
				log.Printf("[%s] Poller stopped", apt.ICAO)
				return
			}
		}
	}()
}

func (app *App) pollAirport(apt monitor.Airport, forceNotify bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obs, err := monitor.FetchMETAR(ctx, apt.ICAO)
	if err != nil {
		log.Printf("[%s] METAR error: %v", apt.ICAO, err)
		return
	}

	if hpa, ok := monitor.ParsePressure(obs.Raw); ok {
		app.pressure[apt.ICAO].Record(time.Now(), hpa)
	}

	if err := app.states[apt.ICAO].Update(obs.TempCelsius, apt); err != nil {
		log.Printf("[%s] State error: %v", apt.ICAO, err)
		return
	}

	hasChange := app.obsCache.HasSignificantChange(apt.ICAO, obs, app.config.ChangeThreshold)

	app.obsCache.Set(apt.ICAO, PreviousObs{
		TempC:      obs.TempCelsius,
		WindSpeed:  obs.WindSpeed,
		WindDir:    obs.WindDir,
		Visibility: obs.Visibility,
	})

	if !hasChange && !forceNotify {
		return
	}

	var fc *monitor.HourlyForecast
	fc, _ = monitor.FetchForecast(ctx, apt)

	snapshot := app.states[apt.ICAO].Snapshot()
	report := monitor.AnalyzeWeather(apt, obs, fc, snapshot, app.pressure[apt.ICAO])

	select {
	case app.notifyChan <- report:
		log.Printf("[%s] Notification queued", apt.ICAO)
	default:
		log.Printf("[%s] Channel full", apt.ICAO)
	}
}

func (app *App) startReportHandler() {
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()

		for {
			select {
			case req := <-app.reportChan:
				app.handleReportRequest(req)
			case <-app.stopChan:
				return
			}
		}
	}()
}

func (app *App) handleReportRequest(req ReportRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var airportsToFetch []monitor.Airport
	if req.SingleAP != "" {
		if apt, ok := app.airports[req.SingleAP]; ok {
			airportsToFetch = []monitor.Airport{apt}
		}
	} else {
		for _, apt := range app.airports {
			airportsToFetch = append(airportsToFetch, apt)
		}
	}

	sort.Slice(airportsToFetch, func(i, j int) bool {
		return airportsToFetch[i].ICAO < airportsToFetch[j].ICAO
	})

	for _, apt := range airportsToFetch {
		obs, err := monitor.FetchMETAR(ctx, apt.ICAO)
		if err != nil {
			app.sendToChat(req.ChatID, fmt.Sprintf("❌ %s: %v", apt.ICAO, err))
			continue
		}

		_ = app.states[apt.ICAO].Update(obs.TempCelsius, apt)
		if hpa, ok := monitor.ParsePressure(obs.Raw); ok {
			app.pressure[apt.ICAO].Record(time.Now(), hpa)
		}

		fc, _ := monitor.FetchForecast(ctx, apt)
		outlook, _ := FetchDailyOutlook(ctx, apt)

		snapshot := app.states[apt.ICAO].Snapshot()
		report := monitor.AnalyzeWeather(apt, obs, fc, snapshot, app.pressure[apt.ICAO])

		msg := FormatTelegramMessage(report, outlook)
		app.sendToChat(req.ChatID, msg)

		time.Sleep(100 * time.Millisecond)
	}
}

func (app *App) startNotificationDispatcher() {
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()

		for {
			select {
			case report := <-app.notifyChan:
				app.dispatchNotification(report)
			case <-app.stopChan:
				return
			}
		}
	}()
}

func (app *App) dispatchNotification(report *monitor.Report) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outlook, _ := FetchDailyOutlook(ctx, report.Airport)
	msg := FormatTelegramMessage(report, outlook)

	for _, chatID := range app.config.ChatIDs {
		app.sendToChat(chatID, msg)
	}
}

func (app *App) sendToChat(chatID int64, msg string) {
	chat := &tele.Chat{ID: chatID}
	_, err := app.bot.Send(chat, msg, &tele.SendOptions{
		ParseMode:             tele.ModeMarkdown,
		DisableWebPagePreview: true,
	})
	if err != nil {
		log.Printf("Send error to %d: %v", chatID, err)
	}
}

func (app *App) Run() error {
	log.Println("Starting Pogodnik...")

	app.startNotificationDispatcher()
	app.startReportHandler()

	for _, apt := range app.airports {
		app.startAirportPoller(apt)
	}
	log.Printf("Started %d airport pollers", len(app.airports))

	go app.bot.Start()

	for _, chatID := range app.config.ChatIDs {
		app.sendToChat(chatID, "🟢 *Pogodnik Online*\n\nType /help for commands.")
	}

	log.Println("All systems operational")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received %v, shutting down...", sig)

	return app.Shutdown()
}

func (app *App) Shutdown() error {
	for _, chatID := range app.config.ChatIDs {
		app.sendToChat(chatID, "🔴 *Pogodnik Shutting Down*")
	}

	app.bot.Stop()
	close(app.stopChan)

	done := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("Shutdown complete")
	case <-time.After(10 * time.Second):
		log.Println("Shutdown timeout")
	}

	return nil
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

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
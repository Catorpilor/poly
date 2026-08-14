package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/telegram"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Set up logging
	setupLogging(cfg.App.LogLevel)

	// Connect to database
	db, err := database.NewConnection(&cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	log.Println("Database connection established")

	// TODO: Run database migrations
	// This would involve implementing a migration runner
	// For now, migrations need to be run manually

	// Create bot instance
	bot, err := telegram.NewBot(cfg, db)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	// Start price feed + SL/TP monitor for auto-sell threshold evaluation.
	// Monitor depends on bot (executor + notifier); bot holds a reference to
	// the monitor so arm/disarm handlers can update WS subscriptions.
	priceFeed := live.NewPriceFeedManager(bot.GetTradingClient())
	priceFeed.Start()
	sltpMonitor := live.NewSLTPMonitor(
		bot.GetSLTPArmRepository(),
		priceFeed,
		bot, // live.TradeExecutor
		bot, // live.Notifier
		live.V2CutoverPause,
	)
	bot.SetSLTPMonitor(sltpMonitor)
	// Resolved-arm sweeper: hourly Gamma closed-market check that auto-disarms
	// arms on finished markets (issue #39). Must be set before Start.
	sltpMonitor.SetClosedMarketChecker(polymarket.NewMarketClient())
	// TP-only auto-arms extend sell coverage to the whole current position
	// (feat/auto-arm-full-coverage); the bot reads holdings via the Data API.
	sltpMonitor.SetHoldingReader(bot)
	if err := sltpMonitor.Start(); err != nil {
		log.Printf("Warning: Failed to start SL/TP monitor: %v", err)
	}
	defer priceFeed.Stop()
	defer sltpMonitor.Stop()

	// Comeback Snipe: watch subscribed events, armed tokens, and held
	// positions for the crashed-favorite pattern; alert with one-tap buy.
	snipeWatcher := live.NewSnipeWatcher(
		priceFeed,
		live.NewSnipeRecipientResolver(bot.GetLiveManager(), bot.GetSLTPArmRepository()),
		bot, // live.SnipeNotifier
	)
	// Seed Session Highs from CLOB trade history so tokens that join the
	// watch mid-game (late subscription, restart) can still alert.
	snipeWatcher.SetHistorySeeder(bot.GetTradingClient())
	snipeWatcher.Start()
	defer snipeWatcher.Stop()
	bot.GetLiveManager().SetSnipeWatcher(snipeWatcher)
	bot.SetSnipe(snipeWatcher, priceFeed)
	bot.SeedSnipeArmed()

	// Durable Live Watches (ADR 0008, issue #57 phase 1): wire the Postgres
	// store, then re-register and re-resolve every stored watch so a restart is
	// watch-neutral. Must run after the snipe watcher is wired — restore
	// re-arms each event's snipe watch. A resolve failure keeps the row and is
	// logged; boot never deletes a watch (expiry is a later phase).
	bot.GetLiveManager().SetLiveWatchStore(repositories.NewLiveWatchRepository(db))
	if _, _, err := bot.GetLiveManager().RestoreWatches(context.Background()); err != nil {
		log.Printf("Warning: Failed to restore live watches: %v", err)
	}

	// Event Refresh loop (ADR 0008 phase 2, issue #55): every ~2 min, re-resolve
	// each subscribed event and register markets created after subscribe time —
	// series games appear mid-series and an unrefreshed watch misses their whole
	// crash. Delta-only registration; resolve errors log-and-skip. Bound to the
	// manager lifecycle (stops when the manager stops).
	bot.GetLiveManager().StartEventRefresh()

	// Watch expiry sweep (ADR 0008 phase 4, issue #57): a couple minutes after
	// boot and hourly thereafter, remove a Live Watch only on positive evidence
	// that its event finished — every market closed=true under Gamma's
	// identity-validated closed filter (the #40 sweeper doctrine, #33 trap). A
	// resolve failure is never closed-evidence, so errors keep the watch and
	// only silence when the event truly ends. The resolver is the closed-event
	// checker; bound to the manager lifecycle (stops when the manager stops).
	bot.GetLiveManager().SetClosedEventChecker(bot.GetLiveManager().GetResolver())
	bot.GetLiveManager().StartWatchExpirySweep()

	// Start live monitoring web server
	liveWebPort := 8081 // Default port for live monitoring web interface
	if cfg.App.Port > 0 {
		liveWebPort = cfg.App.Port + 1 // Use next port after app port
	}
	webServer := live.NewWebServer(
		bot.GetLiveManager(),
		liveWebPort,
		db,
		cfg,
		bot.GetWalletManager(),
		bot.GetTradingClient(),
	)
	if err := webServer.Start(); err != nil {
		log.Printf("Warning: Failed to start live monitoring web server: %v", err)
	} else {
		log.Printf("Live monitoring web server started on port %d", liveWebPort)
	}

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutdown signal received, stopping bot...")
		cancel()

		// Give the bot time to clean up
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()

	// Start the bot
	log.Println("Starting Polymarket Trading Bot...")
	log.Printf("Environment: %s", cfg.App.Environment)
	log.Printf("Bot running on port: %d", cfg.App.Port)

	if err := bot.Start(ctx); err != nil {
		if err != context.Canceled {
			log.Fatalf("Bot error: %v", err)
		}
	}

	log.Println("Bot stopped gracefully")
}

// setupLogging configures the logging based on the log level
func setupLogging(logLevel string) {
	// Set log flags
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// In production, you might want to use a more sophisticated logging library
	// For now, we'll use the standard library logger
	switch logLevel {
	case "debug":
		log.SetPrefix("[DEBUG] ")
	case "info":
		log.SetPrefix("[INFO] ")
	case "warn":
		log.SetPrefix("[WARN] ")
	case "error":
		log.SetPrefix("[ERROR] ")
	default:
		log.SetPrefix("[INFO] ")
	}
}

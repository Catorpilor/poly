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
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand" // Added for seeding random number generator
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"o365mbx/config"
	"o365mbx/engine"
	"o365mbx/o365client"
)

var version = "dev" // Global variable to hold the version

func main() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // Create a local random number generator

	configPath := flag.String("config", "", "Path to the configuration file (JSON)")
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit") // New flag for healthcheck

	// Define flags for settings that can be in the config file or command line
	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	tokenString := flag.String("token-string", "", "Directly provides the JWT token as a string.")
	tokenFile := flag.String("token-file", "", "Specifies a file from which to read the JWT token.")
	tokenEnv := flag.Bool("token-env", false, "Instructs the application to read the JWT token from the JWT_TOKEN environment variable.")
	removeTokenFile := flag.Bool("remove-token-file", false, "Remove the token file after all messages have been processed for security purposes.")
	processedFolder := flag.String("processed-folder", "", "Folder to move processed emails to")
	errorFolder := flag.String("error-folder", "", "Folder to move emails that failed to process")
	processingMode := flag.String("processing-mode", "", "Processing mode: full, incremental, or route")
	timeoutSeconds := flag.Int("timeout", 0, "HTTP client timeout in seconds")
	maxParallelDownloads := flag.Int("parallel", 0, "Maximum number of parallel downloads")
	apiCallsPerSecond := flag.Float64("api-rate", 0, "API calls per second for client-side rate limiting (default: 5.0)")
	apiBurst := flag.Int("api-burst", 0, "API burst capacity for client-side rate limiting (default: 10)")
	maxRetries := flag.Int("max-retries", 0, "Maximum number of retries for failed API calls")
	initialBackoffSeconds := flag.Int("initial-backoff-seconds", 0, "Initial backoff in seconds for retries")
	largeAttachmentThresholdMB := flag.Int("large-attachment-threshold-mb", 0, "Threshold in MB for large attachments")
	chunkSizeMB := flag.Int("chunk-size-mb", 0, "Chunk size in MB for large attachment downloads")
	dirPerms := flag.Int("dir-perms", 0, "Permissions for created directories (octal)")
	filePerms := flag.Int("file-perms", 0, "Permissions for created files (octal)")
	flag.Parse()

	if *displayVersion {
		fmt.Printf("O365 Mailbox Downloader Version: %s\n", version)
		os.Exit(0) // Exit cleanly after displaying version
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Override config values with command-line arguments if provided
	if *tokenString != "" {
		cfg.TokenString = *tokenString
	}
	if *tokenFile != "" {
		cfg.TokenFile = *tokenFile
	}
	if *tokenEnv {
		cfg.TokenEnv = *tokenEnv
	}
	if *removeTokenFile {
		cfg.RemoveTokenFile = *removeTokenFile
	}
	if *mailboxName != "" {
		cfg.MailboxName = *mailboxName
	}
	if *workspacePath != "" {
		cfg.WorkspacePath = *workspacePath
	}
	if *processedFolder != "" {
		cfg.ProcessedFolder = *processedFolder
	}
	if *errorFolder != "" {
		cfg.ErrorFolder = *errorFolder
	}
	if *processingMode != "" {
		cfg.ProcessingMode = *processingMode
	}
	if *timeoutSeconds != 0 {
		cfg.HTTPClientTimeoutSeconds = *timeoutSeconds
	}
	if *maxParallelDownloads != 0 {
		cfg.MaxParallelDownloads = *maxParallelDownloads
	}
	if *apiCallsPerSecond != 0 {
		cfg.APICallsPerSecond = *apiCallsPerSecond
	}
	if *apiBurst != 0 {
		cfg.APIBurst = *apiBurst
	}
	if *maxRetries != 0 {
		cfg.MaxRetries = *maxRetries
	}
	if *initialBackoffSeconds != 0 {
		cfg.InitialBackoffSeconds = *initialBackoffSeconds
	}
	if *largeAttachmentThresholdMB != 0 {
		cfg.LargeAttachmentThresholdMB = *largeAttachmentThresholdMB
	}
	if *chunkSizeMB != 0 {
		cfg.ChunkSizeMB = *chunkSizeMB
	}
	if *dirPerms != 0 {
		cfg.DirPerms = *dirPerms
	}
	if *filePerms != 0 {
		cfg.FilePerms = *filePerms
	}

	// Validate loaded configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	if cfg.RemoveTokenFile && cfg.TokenFile != "" {
		defer func() {
			log.Infof("Removing token file: %s", cfg.TokenFile)
			if err := os.Remove(cfg.TokenFile); err != nil {
				log.Errorf("Failed to remove token file: %v", err)
			}
		}()
	}

	// --- Token Acquisition and Validation ---
	tokenSourceCount := 0
	if cfg.TokenString != "" {
		tokenSourceCount++
	}
	if cfg.TokenFile != "" {
		tokenSourceCount++
	}
	if cfg.TokenEnv {
		tokenSourceCount++
	}

	if tokenSourceCount == 0 {
		log.Fatalf("No token source specified. Please use one of -token-string, -token-file, or -token-env.")
	}
	if tokenSourceCount > 1 {
		log.Fatalf("Multiple token sources specified. Please use only one of -token-string, -token-file, or -token-env.")
	}

	if cfg.TokenString != "" {
		cfg.AccessToken = cfg.TokenString
	} else if cfg.TokenFile != "" {
		tokenBytes, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			log.Fatalf("Failed to read token from file %s: %v", cfg.TokenFile, err)
		}
		cfg.AccessToken = strings.TrimSpace(string(tokenBytes))
	} else if cfg.TokenEnv {
		cfg.AccessToken = os.Getenv("JWT_TOKEN")
		if cfg.AccessToken == "" {
			log.Fatalf("Token source set to env, but JWT_TOKEN environment variable is not set or empty.")
		}
	}

	// Set up context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure cancel is called to release resources

	// Listen for interrupt signals (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.WithField("signal", <-sigChan).Warn("Received interrupt signal, initiating graceful shutdown...")
		cancel() // Call cancel function
	}()

	// Argument validation (common for both modes: healthcheck and download)
	if cfg.AccessToken == "" {
		log.WithField("argument", "accessToken").Fatalf("Error: Access token is missing.")
	}
	if !engine.IsValidEmail(cfg.MailboxName) {
		log.WithField("argument", "mailboxName").Fatalf("Error: Invalid mailbox name format: %s", cfg.MailboxName)
	}

	// Health Check Mode
	if *healthCheck {
		log.SetFormatter(&log.TextFormatter{
			FullTimestamp: true,
		})
		log.SetLevel(log.InfoLevel)

		fmt.Println("O365 Mailbox Downloader - Health Check Mode")
		log.Infof("Version: %s", version)

		// Initialize o365Client for health check
		o365Client := o365client.NewO365Client(
			cfg.AccessToken,
			time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second,
			cfg.MaxRetries,
			cfg.InitialBackoffSeconds,
			cfg.APICallsPerSecond,
			cfg.APIBurst,
			rng,
		)

		engine.RunHealthCheck(ctx, o365Client, cfg.MailboxName)
		os.Exit(0) // Exit after health check
	}

	// Normal Download Mode
	engine.Run(ctx, cfg, rng)
}

// No more `runHealthCheckMode` in main.go

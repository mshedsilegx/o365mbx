package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"o365mbx/emailprocessor"
	"o365mbx/engine"
	"o365mbx/filehandler"
	"o365mbx/o365client"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- Flag Definition ---
	tokenString := flag.String("token-string", "", "JWT token as a string.")
	tokenFile := flag.String("token-file", "", "Path to a file containing the JWT token.")
	tokenEnv := flag.Bool("token-env", false, "Read JWT token from JWT_TOKEN environment variable.")
	removeTokenFile := flag.Bool("remove-token-file", false, "Remove the token file after use (only if -token-file is specified).")
	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	configPath := flag.String("config", "", "Path to the configuration file (JSON)")
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	debug := flag.Bool("debug", false, "Enable debug logging")
	processingMode := flag.String("processing-mode", "", "Processing mode: 'full', 'incremental', or 'route'.")
	inboxFolder := flag.String("inbox-folder", "", "The source folder from which to process messages.")
	stateFilePath := flag.String("state", "", "Path to the state file for incremental processing")
	processedFolder := flag.String("processed-folder", "", "Destination folder for successfully processed messages in route mode.")
	errorFolder := flag.String("error-folder", "", "Destination folder for messages that failed processing in route mode.")
	timeoutSeconds := flag.Int("timeout", 0, "HTTP client timeout in seconds.")
	maxParallelDownloads := flag.Int("parallel", 0, "Maximum number of parallel downloads.")
	apiCallsPerSecond := flag.Float64("api-rate", 0, "API calls per second for client-side rate limiting.")
	apiBurst := flag.Int("api-burst", 0, "API burst capacity for client-side rate limiting.")
	maxRetries := flag.Int("max-retries", 0, "Maximum number of retries for failed API calls.")
	initialBackoffSeconds := flag.Int("initial-backoff-seconds", 0, "Initial backoff in seconds for retries.")
	chunkSizeMB := flag.Int("chunk-size-mb", 0, "Chunk size in MB for large attachment downloads.")
	largeAttachmentThresholdMB := flag.Int("large-attachment-threshold-mb", 0, "Threshold in MB for large attachments.")
	stateSaveInterval := flag.Int("state-save-interval", 0, "Save state every N messages.")
	bandwidthLimitMBs := flag.Float64("bandwidth-limit-mbs", 0, "Bandwidth limit in MB/s for downloads (0 for disabled).")
	flag.Parse()

	// --- Configuration Loading and Merging ---
	cfg, err := engine.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token-string": cfg.TokenString = *tokenString
		case "token-file": cfg.TokenFile = *tokenFile
		case "token-env": cfg.TokenEnv = *tokenEnv
		case "remove-token-file": cfg.RemoveTokenFile = *removeTokenFile
		case "debug": cfg.DebugLogging = *debug
		case "processing-mode": cfg.ProcessingMode = *processingMode
		case "inbox-folder": cfg.InboxFolder = *inboxFolder
		case "state": cfg.StateFilePath = *stateFilePath
		case "processed-folder": cfg.ProcessedFolder = *processedFolder
		case "error-folder": cfg.ErrorFolder = *errorFolder
		case "timeout": cfg.HTTPClientTimeoutSeconds = *timeoutSeconds
		case "parallel": cfg.MaxParallelDownloads = *maxParallelDownloads
		case "api-rate": cfg.APICallsPerSecond = *apiCallsPerSecond
		case "api-burst": cfg.APIBurst = *apiBurst
		case "max-retries": cfg.MaxRetries = *maxRetries
		case "initial-backoff-seconds": cfg.InitialBackoffSeconds = *initialBackoffSeconds
		case "chunk-size-mb": cfg.ChunkSizeMB = *chunkSizeMB
		case "large-attachment-threshold-mb": cfg.LargeAttachmentThresholdMB = *largeAttachmentThresholdMB
		case "state-save-interval": cfg.StateSaveInterval = *stateSaveInterval
		case "bandwidth-limit-mbs": cfg.BandwidthLimitMBs = *bandwidthLimitMBs
		}
	})

	// --- Logging Setup ---
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if cfg.DebugLogging {
		log.SetLevel(log.DebugLevel)
		log.Debugln("Debug logging enabled.")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	// --- Early Exit flags ---
	if *displayVersion {
		fmt.Printf("O365 Mailbox Downloader Version: %s\n", version)
		os.Exit(0)
	}

	// --- Configuration Validation ---
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// --- Context and Signal Handling ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.WithField("signal", <-sigChan).Warn("Received interrupt signal, initiating graceful shutdown...")
		cancel()
	}()

	// --- Token Loading ---
	accessToken, err := loadAccessToken(cfg)
	if err != nil {
		log.Fatalf("Error loading access token: %v", err)
	}

	// --- Final Validation ---
	if *mailboxName == "" {
		log.Fatal("Error: -mailbox is a required argument.")
	}
	if !isValidEmail(*mailboxName) {
		log.Fatalf("Error: Invalid mailbox name format: %s", *mailboxName)
	}
	if cfg.ProcessingMode == "incremental" && cfg.StateFilePath == "" {
		log.Fatal("Error: State file path must be provided for incremental processing mode.")
	}
	if cfg.ProcessingMode == "route" {
		if cfg.ProcessedFolder == "" {
			log.Fatal("Error: Processed folder name must be provided for route mode.")
		}
		if cfg.ErrorFolder == "" {
			log.Fatal("Error: Error folder name must be provided for route mode.")
		}
	}

	// --- Dependency Injection ---
	o365Client := o365client.NewO365Client(accessToken, time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second, cfg.MaxRetries, cfg.InitialBackoffSeconds, cfg.APICallsPerSecond, cfg.APIBurst, rng)
	emailProcessor := emailprocessor.NewEmailProcessor()
	fileHandler := filehandler.NewFileHandler(*workspacePath, o365Client, cfg.LargeAttachmentThresholdMB, cfg.ChunkSizeMB, cfg.BandwidthLimitMBs)

	// --- Health Check or Main Engine Execution ---
	if *healthCheck {
		runHealthCheckMode(ctx, o365Client, *mailboxName)
		os.Exit(0)
	}

	// Defer the token file removal if requested
	defer func() {
		if cfg.TokenFile != "" && cfg.RemoveTokenFile {
			log.WithField("file", cfg.TokenFile).Info("Removing token file as requested.")
			if err := os.Remove(cfg.TokenFile); err != nil {
				log.WithField("file", cfg.TokenFile).Errorf("Failed to remove token file: %v", err)
			}
		}
	}()

	engine.RunEngine(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, *mailboxName, *workspacePath, version)
}

func loadAccessToken(cfg *engine.Config) (string, error) {
	sourceCount := 0
	if cfg.TokenString != "" { sourceCount++ }
	if cfg.TokenFile != "" { sourceCount++ }
	if cfg.TokenEnv { sourceCount++ }

	if sourceCount == 0 {
		return "", fmt.Errorf("no token source specified. Please use one of -token-string, -token-file, or -token-env (or their config file equivalents)")
	}
	if sourceCount > 1 {
		return "", fmt.Errorf("multiple token sources specified. Please use only one of -token-string, -token-file, or -token-env (or their config file equivalents)")
	}

	if cfg.TokenString != "" {
		log.Info("Using token from tokenString.")
		return cfg.TokenString, nil
	}
	if cfg.TokenFile != "" {
		log.Info("Using token from tokenFile.")
		content, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %s: %w", cfg.TokenFile, err)
		}
		return strings.TrimSpace(string(content)), nil
	}
	if cfg.TokenEnv {
		log.Info("Using token from JWT_TOKEN environment variable.")
		token := os.Getenv("JWT_TOKEN")
		if token == "" {
			return "", fmt.Errorf("tokenEnv specified, but JWT_TOKEN environment variable is not set")
		}
		return token, nil
	}
	return "", fmt.Errorf("internal error: no token source identified")
}

func isValidEmail(email string) bool {
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
}

func runHealthCheckMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName string) {
	log.WithField("mailbox", mailboxName).Info("Attempting to connect to mailbox and retrieve statistics...")
	messageCount, err := client.GetMailboxStatistics(ctx, mailboxName)
	if err != nil {
		log.Fatalf("O365 API error during connect check: %v", err)
	}
	log.WithFields(log.Fields{
		"mailbox":      mailboxName,
		"messageCount": messageCount,
	}).Info("Mailbox connectivity successful. Statistics:")
	fmt.Printf("\nMailbox: %s\n", mailboxName)
	fmt.Printf("Total Messages: %d\n", messageCount)
}

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
	"text/tabwriter"
	"time"

	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- Flag Definition ---
	// Note: The -rod flag is not defined here but is handled by the underlying go-rod library.
	// It is used to pass launch parameters to the headless chrome browser for PDF conversion.
	// Example: -rod="--proxy-server=127.0.0.1:8080"
	tokenString := flag.String("token-string", "", "JWT token as a string.")
	tokenFile := flag.String("token-file", "", "Path to a file containing the JWT token.")
	tokenEnv := flag.Bool("token-env", false, "Read JWT token from JWT_TOKEN environment variable.")
	removeTokenFile := flag.Bool("remove-token-file", false, "Remove the token file after use (only if -token-file is specified).")
	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	messageDetailsFolder := flag.String("message-details", "", "When used with -healthcheck, displays message details for the specified folder.")
	debug := flag.Bool("debug", false, "Enable debug logging")
	processingMode := flag.String("processing-mode", "full", "Processing mode: 'full', 'incremental', or 'route'.")
	inboxFolder := flag.String("inbox-folder", "Inbox", "The source folder from which to process messages.")
	stateFilePath := flag.String("state", "", "Path to the state file for incremental processing")
	processedFolder := flag.String("processed-folder", "", "Destination folder for successfully processed messages in route mode.")
	errorFolder := flag.String("error-folder", "", "Destination folder for messages that failed processing in route mode.")
	timeoutSeconds := flag.Int("timeout", 60, "HTTP client timeout in seconds.")
	maxParallelDownloads := flag.Int("parallel", 4, "Maximum number of parallel downloads.")
	apiCallsPerSecond := flag.Float64("api-rate", 10, "API calls per second for client-side rate limiting.")
	apiBurst := flag.Int("api-burst", 10, "API burst capacity for client-side rate limiting.")
	maxRetries := flag.Int("max-retries", 3, "Maximum number of retries for failed API calls.")
	initialBackoffSeconds := flag.Int("initial-backoff-seconds", 5, "Initial backoff in seconds for retries.")
	chunkSizeMB := flag.Int("chunk-size-mb", 20, "Chunk size in MB for large attachment downloads.")
	largeAttachmentThresholdMB := flag.Int("large-attachment-threshold-mb", 20, "Threshold in MB for large attachments.")
	stateSaveInterval := flag.Int("state-save-interval", 100, "Save state every N messages.")
	bandwidthLimitMBs := flag.Float64("bandwidth-limit-mbs", 0, "Bandwidth limit in MB/s for downloads (0 for disabled).")
	convertBody := flag.String("convert-body", "none", "Convert body to 'text' or 'pdf'. Default is 'none'.")
	chromiumPath := flag.String("chromium-path", "", "Path to headless chromium binary for PDF conversion.")
	flag.Parse()

	cfg := &engine.Config{
		TokenString:                *tokenString,
		TokenFile:                  *tokenFile,
		TokenEnv:                   *tokenEnv,
		RemoveTokenFile:            *removeTokenFile,
		MailboxName:                *mailboxName,
		WorkspacePath:              *workspacePath,
		DebugLogging:               *debug,
		ProcessingMode:             *processingMode,
		InboxFolder:                *inboxFolder,
		StateFilePath:              *stateFilePath,
		ProcessedFolder:            *processedFolder,
		ErrorFolder:                *errorFolder,
		HTTPClientTimeoutSeconds:   *timeoutSeconds,
		MaxParallelDownloads:       *maxParallelDownloads,
		APICallsPerSecond:          *apiCallsPerSecond,
		APIBurst:                   *apiBurst,
		MaxRetries:                 *maxRetries,
		InitialBackoffSeconds:      *initialBackoffSeconds,
		ChunkSizeMB:                *chunkSizeMB,
		LargeAttachmentThresholdMB: *largeAttachmentThresholdMB,
		StateSaveInterval:          *stateSaveInterval,
		BandwidthLimitMBs:          *bandwidthLimitMBs,
		ConvertBody:                *convertBody,
		ChromiumPath:               *chromiumPath,
	}

	cfg.SetDefaults()

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
	if cfg.MailboxName == "" {
		log.Fatal("Error: Mailbox name is a required argument (set via -mailbox or in config file).")
	}
	if !isValidEmail(cfg.MailboxName) {
		log.Fatalf("Error: Invalid mailbox name format: %s", cfg.MailboxName)
	}
	if cfg.WorkspacePath == "" {
		log.Fatal("Error: Workspace path is a required argument (set via -workspace or in config file).")
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
	logger := log.WithFields(log.Fields{}) // Create a base logger
	o365Client, err := o365client.NewO365Client(accessToken, time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second, cfg.MaxRetries, cfg.InitialBackoffSeconds, cfg.APICallsPerSecond, cfg.APIBurst, rng)
	if err != nil {
		log.Fatalf("Error creating O365 client: %v", err)
	}
	emailProcessor := emailprocessor.NewEmailProcessor(logger)
	fileHandler := filehandler.NewFileHandler(cfg.WorkspacePath, o365Client, emailProcessor, cfg.LargeAttachmentThresholdMB, cfg.ChunkSizeMB, cfg.BandwidthLimitMBs, logger)

	// --- Health Check or Main Engine Execution ---
	if *healthCheck {
		if *messageDetailsFolder != "" {
			runMessageDetailsMode(ctx, o365Client, cfg.MailboxName, *messageDetailsFolder)
		} else {
			runHealthCheckMode(ctx, o365Client, cfg.MailboxName)
		}
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

	engine.RunEngine(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, version)
}

func loadAccessToken(cfg *engine.Config) (string, error) {
	sourceCount := 0
	if cfg.TokenString != "" {
		sourceCount++
	}
	if cfg.TokenFile != "" {
		sourceCount++
	}
	if cfg.TokenEnv {
		sourceCount++
	}

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
	log.WithField("mailbox", mailboxName).Info("Performing health check...")
	stats, err := client.GetMailboxHealthCheck(ctx, mailboxName)
	if err != nil {
		log.Fatalf("O365 API error during health check: %v", err)
	}

	log.WithField("mailbox", mailboxName).Info("Health check successful.")
	fmt.Println("\n--- Mailbox Health Check ---")
	fmt.Printf("Mailbox: %s\n", mailboxName)
	fmt.Println("------------------------------")
	fmt.Printf("Total Messages: %d\n", stats.TotalMessages)
	fmt.Printf("Total Folders: %d\n", len(stats.Folders))
	fmt.Printf("Total Mailbox Size: %.2f MB\n", float64(stats.TotalMailboxSize)/1024/1024)
	fmt.Println("------------------------------")
	fmt.Println("\n--- Folder Statistics ---")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', tabwriter.AlignRight)
	if _, err := fmt.Fprintln(w, "Folder\tItems\tSize (KB)\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}
	if _, err := fmt.Fprintln(w, "-------\t-----\t---------\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}

	for _, folder := range stats.Folders {
		folderSizeKB := float64(folder.Size) / 1024
		if _, err := fmt.Fprintf(w, "%s\t%d\t%.2f\t\n", folder.Name, folder.TotalItems, folderSizeKB); err != nil {
			log.Warnf("Error writing to tabwriter: %v", err)
		}
	}

	if err := w.Flush(); err != nil {
		log.Warnf("Error flushing tabwriter: %v", err)
	}
	fmt.Println("-------------------------")
}

func runMessageDetailsMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName, folderName string) {
	log.WithFields(log.Fields{
		"mailbox": mailboxName,
		"folder":  folderName,
	}).Info("Fetching message details for folder...")

	// This function will be implemented in the o365client package
	messages, err := client.GetMessageDetailsForFolder(ctx, mailboxName, folderName)
	if err != nil {
		log.Fatalf("Failed to get message details: %v", err)
	}

	fmt.Printf("\n--- Message Details for Folder: %s ---\n", folderName)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "From\tTo\tDate\tSubject\tAttachments\tTotal Size (KB)\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}
	if _, err := fmt.Fprintln(w, "----\t--\t----\t-------\t-----------\t----------------\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}

	for _, msg := range messages {
		from := msg.From
		to := msg.To
		subject := msg.Subject
		if len(subject) > 75 {
			subject = subject[:72] + "..."
		}
		totalSizeKB := float64(msg.AttachmentsTotalSize) / 1024

		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%.2f\t\n",
			from,
			to,
			msg.Date.Format("2006-01-02 15:04"),
			subject,
			msg.AttachmentCount,
			totalSizeKB,
		); err != nil {
			log.Warnf("Error writing to tabwriter: %v", err)
		}
	}

	if err := w.Flush(); err != nil {
		log.Warnf("Error flushing tabwriter: %v", err)
	}
	fmt.Println("-------------------------------------------------")
}

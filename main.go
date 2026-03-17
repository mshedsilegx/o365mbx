// Package main is the entry point for the o365mbx application.
//
// OBJECTIVE:
// This application is designed to download and process email messages and attachments
// from a Microsoft 365 (O365) mailbox using the Microsoft Graph API. It supports
// full, incremental, and "route" (move-after-process) modes, along with health checks
// and various body conversion options (Text, PDF).
//
// CORE SECTIONS:
// 1. Argument Parsing: Uses flag.FlagSet to handle CLI arguments and configuration overrides.
// 2. Configuration: Loads settings from JSON or CLI, validates them, and manages authentication tokens.
// 3. Dependency Injection: Initializes core services (O365Client, EmailProcessor, FileHandler) and injects them into the Engine.
// 4. Execution: Runs either the diagnostic Health Check or the primary download Engine (RunEngine).
//
// CORE FUNCTIONALITY:
// - Parallelized Extraction: Downloads multiple messages and attachments concurrently.
// - Content Transformation: Sanitizes HTML and optionally converts bodies to PDF or Text.
// - Reliable Storage: Persists data to a structured workspace with detailed metadata and state tracking.
// - Mailbox Management: Automates message relocation (routing) based on processing success or failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"o365mbx/emailprocessor"
	"o365mbx/engine"
	"o365mbx/filehandler"
	"o365mbx/o365client"
	"o365mbx/presenter"
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
	if err := run(os.Args, os.Stdout); err != nil {
		log.Fatalf("Application failed: %v", err)
	}
}

// run is the internal entry point that handles flag parsing, configuration setup,
// and execution of either the diagnostic health check or the main engine.
func run(args []string, out io.Writer) error {
	// --- Pre-flight Checks ---
	checkLongPathSupport()

	// #nosec G404 - math/rand is used for jitter/backoff, not for security-sensitive operations.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- Flag Definition ---
	// We use a local FlagSet to make run() testable and avoid global state issues.
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(out)

	configPath := fs.String("config", "", "Path to a JSON configuration file.")
	tokenString := fs.String("token-string", "", "JWT token as a string.")
	tokenFile := fs.String("token-file", "", "Path to a file containing the JWT token.")
	tokenEnv := fs.Bool("token-env", false, "Read JWT token from JWT_TOKEN environment variable.")
	removeTokenFile := fs.Bool("remove-token-file", false, "Remove the token file after use (only if -token-file is specified).")
	mailboxName := fs.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := fs.String("workspace", "", "Unique folder to store all artifacts")
	displayVersion := fs.Bool("version", false, "Display application version")
	healthCheck := fs.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	messageDetailsFolder := fs.String("message-details", "", "When used with -healthcheck, displays message details for the specified folder.")
	debug := fs.Bool("debug", false, "Enable debug logging")
	processingMode := fs.String("processing-mode", "full", "Processing mode: 'full', 'incremental', or 'route'.")
	inboxFolder := fs.String("inbox-folder", "Inbox", "The source folder from which to process messages.")
	stateFilePath := fs.String("state", "", "Path to the state file for incremental processing")
	processedFolder := fs.String("processed-folder", "", "Destination folder for successfully processed messages in route mode.")
	errorFolder := fs.String("error-folder", "", "Destination folder for messages that failed processing in route mode.")
	timeoutSeconds := fs.Int("timeout", 120, "HTTP client timeout in seconds.")
	maxParallelDownloads := fs.Int("parallel", 10, "Maximum number of parallel downloads.")
	apiCallsPerSecond := fs.Float64("api-rate", 5.0, "API calls per second for client-side rate limiting.")
	apiBurst := fs.Int("api-burst", 10, "API burst capacity for client-side rate limiting.")
	maxRetries := fs.Int("max-retries", 2, "Maximum number of retries for failed API calls.")
	initialBackoffSeconds := fs.Int("initial-backoff-seconds", 5, "Initial backoff in seconds for retries.")
	chunkSizeMB := fs.Int("chunk-size-mb", 8, "Chunk size in MB for large attachment downloads.")
	largeAttachmentThresholdMB := fs.Int("large-attachment-threshold-mb", 20, "Threshold in MB for large attachments.")
	bandwidthLimitMBs := fs.Float64("bandwidth-limit-mbs", 0, "Bandwidth limit in MB/s for downloads (0 for disabled).")
	convertBody := fs.String("convert-body", "none", "Convert body to 'text' or 'pdf'. Default is 'none'.")
	chromiumPath := fs.String("chromium-path", "", "Path to headless chromium binary for PDF conversion.")
	msgHandler := fs.String("msg-handler", "raw", "Handler for .msg/.eml attachments: 'raw' or 'extractor'.")
	attachmentExtractionL1 := fs.String("attachment-extraction-l1", "default", "Level 1 extraction for .msg/.eml: 'default' (attachments only) or 'inlines' (attachments + inlines).")
	maxExecutionTimeMsg := fs.Int("max-execution-time-msg", 120, "Maximum time in seconds to spend on one email message.")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// --- Configuration Loading ---
	cfg, err := engine.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("error loading configuration: %w", err)
	}

	// --- Early Exit flags ---
	if *displayVersion {
		_, _ = fmt.Fprintf(out, "O365 Mailbox Downloader Version: %s\n", version)
		return nil
	}

	// --- Command-line Override ---
	overrideConfigWithFlagsLocal(cfg, fs, configPath, tokenString, tokenFile, tokenEnv, removeTokenFile, mailboxName, workspacePath, debug, processingMode, inboxFolder, stateFilePath, processedFolder, errorFolder, timeoutSeconds, maxParallelDownloads, apiCallsPerSecond, apiBurst, maxRetries, initialBackoffSeconds, chunkSizeMB, largeAttachmentThresholdMB, bandwidthLimitMBs, convertBody, chromiumPath, msgHandler, attachmentExtractionL1, healthCheck, messageDetailsFolder, maxExecutionTimeMsg)

	// --- Logging Setup ---
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if cfg.DebugLogging {
		log.SetLevel(log.DebugLevel)
		log.Debugln("Debug logging enabled.")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	// --- Configuration Validation ---
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// --- Context and Signal Handling ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigChan
		if ok {
			log.WithField("signal", sig).Warn("Received interrupt signal, initiating graceful shutdown...")
			cancel()
		}
	}()

	// --- Token Loading ---
	accessToken, err := loadAccessToken(cfg)
	if err != nil {
		return fmt.Errorf("error loading access token: %w", err)
	}

	// --- Final Validation ---
	if err := validateFinalConfig(cfg); err != nil {
		return err
	}

	// --- Dependency Injection ---
	// Initialize core services and inject them into the engine.
	logger := log.WithFields(log.Fields{}) // Create a base logger
	o365Client, err := o365client.NewO365Client(accessToken, rng)
	if err != nil {
		return fmt.Errorf("error creating O365 client: %w", err)
	}
	emailProcessor := emailprocessor.NewEmailProcessor(logger)
	if cfg.ConvertBody == "pdf" {
		if err := emailProcessor.Initialize(ctx, cfg.ChromiumPath, cfg.MaxParallelDownloads); err != nil {
			return fmt.Errorf("failed to initialize email processor: %w", err)
		}
		defer func() {
			if err := emailProcessor.Close(); err != nil {
				log.Errorf("Error closing email processor: %v", err)
			}
		}()
	}
	fileHandler := filehandler.NewFileHandler(cfg.WorkspacePath, o365Client, emailProcessor, cfg.LargeAttachmentThresholdMB, cfg.ChunkSizeMB, cfg.BandwidthLimitMBs, cfg.MsgHandler, cfg.AttachmentExtractionL1, logger)

	// --- Health Check or Main Engine Execution ---
	// Execute either the diagnostic health check or the primary download engine.
	if cfg.HealthCheck {
		if cfg.MessageDetailsFolder != "" {
			err = presenter.RunMessageDetailsMode(ctx, o365Client, cfg.MailboxName, cfg.MessageDetailsFolder, out)
		} else {
			err = presenter.RunHealthCheckMode(ctx, o365Client, cfg.MailboxName, out)
		}
		if err != nil {
			return fmt.Errorf("diagnostic run failed: %w", err)
		}
		return nil
	}

	// Defer the token file removal if requested
	defer func() {
		if cfg.TokenFile != "" && cfg.RemoveTokenFile {
			log.WithField("file", cfg.TokenFile).Info("Removing token file as requested.")
			// #nosec G703 - cfg.TokenFile is provided by the user via config or flag, and its removal is an intended feature.
			if err := os.Remove(cfg.TokenFile); err != nil {
				log.WithField("file", cfg.TokenFile).Errorf("Failed to remove token file: %v", err)
			}
		}
	}()

	if err := engine.RunEngine(ctx, cfg, o365Client, emailProcessor, fileHandler, version); err != nil {
		return fmt.Errorf("engine failed: %w", err)
	}
	return nil
}

// loadAccessToken resolves the access token from one of the three supported sources:
// literal string, file path, or environment variable. It enforces that exactly
// one source is provided.
//
// SUPPORTED SOURCES:
// 1. -token-string: Literal JWT string.
// 2. -token-file: Path to a file containing the JWT.
// 3. -token-env: Read from JWT_TOKEN environment variable.
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
		// #nosec G703 - cfg.TokenFile is provided by the user via config or flag, and reading it is an intended feature.
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

func overrideConfigWithFlagsLocal(cfg *engine.Config, fs *flag.FlagSet, configPath, tokenString, tokenFile *string, tokenEnv *bool, removeTokenFile *bool, mailboxName, workspacePath *string, debug *bool, processingMode, inboxFolder, stateFilePath, processedFolder, errorFolder *string, timeoutSeconds, maxParallelDownloads *int, apiCallsPerSecond *float64, apiBurst, maxRetries, initialBackoffSeconds, chunkSizeMB, largeAttachmentThresholdMB *int, bandwidthLimitMBs *float64, convertBody, chromiumPath, msgHandler, attachmentExtractionL1 *string, healthCheck *bool, messageDetailsFolder *string, maxExecutionTimeMsg *int) {
	// Override config file settings with any flags set on the command line.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token-string":
			cfg.TokenString = *tokenString
		case "token-file":
			cfg.TokenFile = *tokenFile
		case "token-env":
			cfg.TokenEnv = *tokenEnv
		case "remove-token-file":
			cfg.RemoveTokenFile = *removeTokenFile
		case "mailbox":
			cfg.MailboxName = *mailboxName
		case "workspace":
			cfg.WorkspacePath = *workspacePath
		case "debug":
			cfg.DebugLogging = *debug
		case "processing-mode":
			cfg.ProcessingMode = *processingMode
		case "inbox-folder":
			cfg.InboxFolder = *inboxFolder
		case "state":
			cfg.StateFilePath = *stateFilePath
		case "processed-folder":
			cfg.ProcessedFolder = *processedFolder
		case "error-folder":
			cfg.ErrorFolder = *errorFolder
		case "timeout":
			cfg.HTTPClientTimeoutSeconds = *timeoutSeconds
		case "parallel":
			cfg.MaxParallelDownloads = *maxParallelDownloads
		case "api-rate":
			cfg.APICallsPerSecond = *apiCallsPerSecond
		case "api-burst":
			cfg.APIBurst = *apiBurst
		case "max-retries":
			cfg.MaxRetries = *maxRetries
		case "initial-backoff-seconds":
			cfg.InitialBackoffSeconds = *initialBackoffSeconds
		case "chunk-size-mb":
			cfg.ChunkSizeMB = *chunkSizeMB
		case "large-attachment-threshold-mb":
			cfg.LargeAttachmentThresholdMB = *largeAttachmentThresholdMB
		case "bandwidth-limit-mbs":
			cfg.BandwidthLimitMBs = *bandwidthLimitMBs
		case "convert-body":
			cfg.ConvertBody = *convertBody
		case "chromium-path":
			cfg.ChromiumPath = *chromiumPath
		case "msg-handler":
			cfg.MsgHandler = *msgHandler
		case "attachment-extraction-l1":
			cfg.AttachmentExtractionL1 = *attachmentExtractionL1
		case "healthcheck":
			cfg.HealthCheck = *healthCheck
		case "message-details":
			cfg.MessageDetailsFolder = *messageDetailsFolder
		case "max-execution-time-msg":
			cfg.MaxExecutionTimeMsg = *maxExecutionTimeMsg
		}
	})
}

// validateFinalConfig performs cross-field validation and ensures all required
// settings are present for the chosen processing mode.
func validateFinalConfig(cfg *engine.Config) error {
	if cfg.MailboxName == "" {
		return fmt.Errorf("mailbox name is a required argument (set via -mailbox or in config file)")
	}
	if !isValidEmail(cfg.MailboxName) {
		return fmt.Errorf("invalid mailbox name format: %s", cfg.MailboxName)
	}
	// Workspace is only required if we are not in health check mode.
	if !cfg.HealthCheck && cfg.WorkspacePath == "" {
		return fmt.Errorf("workspace path is a required argument (set via -workspace or in config file)")
	}
	if cfg.ProcessingMode == "incremental" && cfg.StateFilePath == "" {
		return fmt.Errorf("state file path must be provided for incremental processing mode")
	}
	if cfg.ProcessingMode == "route" {
		if cfg.ProcessedFolder == "" {
			return fmt.Errorf("processed folder name must be provided for route mode")
		}
		if cfg.ErrorFolder == "" {
			return fmt.Errorf("error folder name must be provided for route mode")
		}
	}
	return nil
}

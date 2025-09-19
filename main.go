package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"o365mbx/emailprocessor"
	"o365mbx/filehandler"
	"o365mbx/o365client"
)

var version = "dev"

func isValidEmail(email string) bool {
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
}

func validateWorkspacePath(path string) error {
	if path == "" {
		return fmt.Errorf("workspace path cannot be empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace path must be an absolute path: %s", path)
	}

	// Security check for critical system directories
	criticalPaths := []string{"/", "/root", "/etc", "/bin", "/sbin", "/usr", "/var"}
	for _, p := range criticalPaths {
		if path == p {
			return fmt.Errorf("for safety, using critical system directory '%s' as a workspace is not allowed", path)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Path doesn't exist, which is fine. We will create it.
			return nil
		}
		return fmt.Errorf("failed to stat workspace directory %s: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("workspace path %s exists but is not a directory", path)
	}

	// Check if the directory is empty
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open workspace directory for checking emptiness: %w", err)
	}
	defer dir.Close()

	_, err = dir.Readdir(1) // Try to read one entry
	if err == nil {
		// If we successfully read an entry, the directory is not empty.
		log.Warnf("Workspace directory %s is not empty. Files may be overwritten.", path)
	} else if err != io.EOF {
		// An error other than EOF occurred
		return fmt.Errorf("failed to check if workspace directory is empty: %w", err)
	}
	// If err is io.EOF, the directory is empty, which is good.

	return nil
}

func main() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Token management flags
	tokenString := flag.String("token-string", "", "JWT token as a string.")
	tokenFile := flag.String("token-file", "", "Path to a file containing the JWT token.")
	tokenEnv := flag.Bool("token-env", false, "Read JWT token from JWT_TOKEN environment variable.")
	removeTokenFile := flag.Bool("remove-token-file", false, "Remove the token file after use (only if -token-file is specified).")

	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	timeoutSeconds := flag.Int("timeout", 120, "HTTP client timeout in seconds (default: 120)")
	maxParallelDownloads := flag.Int("parallel", 10, "Maximum number of parallel downloads (default: 10)")
	configPath := flag.String("config", "", "Path to the configuration file (JSON)")
	apiCallsPerSecond := flag.Float64("api-rate", 0, "API calls per second for client-side rate limiting (default: 5.0)")
	apiBurst := flag.Int("api-burst", 0, "API burst capacity for client-side rate limiting (default: 10)")
	maxRetries := flag.Int("max-retries", 2, "Maximum number of retries for failed API calls.")
	initialBackoffSeconds := flag.Int("initial-backoff-seconds", 5, "Initial backoff in seconds for retries.")
	chunkSizeMB := flag.Int("chunk-size-mb", 8, "Chunk size in MB for large attachment downloads.")
	largeAttachmentThresholdMB := flag.Int("large-attachment-threshold-mb", 20, "Threshold in MB for large attachments.")
	stateSaveInterval := flag.Int("state-save-interval", 100, "Save state every N messages.")
	bandwidthLimitMBs := flag.Float64("bandwidth-limit-mbs", 0, "Bandwidth limit in MB/s for downloads (0 for disabled).")
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	debug := flag.Bool("debug", false, "Enable debug logging")
	processingMode := flag.String("processing-mode", "full", "Processing mode: 'full' or 'incremental'")
	stateFilePath := flag.String("state", "", "Path to the state file for incremental processing")
	processedFolder := flag.String("processed-folder", "Processed", "Destination folder for successfully processed messages in route mode.")
	errorFolder := flag.String("error-folder", "Error", "Destination folder for messages that failed processing in route mode.")
	flag.Parse()

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if cfg.DebugLogging {
		log.SetLevel(log.DebugLevel)
		log.Debugln("Debug logging enabled.")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	if *displayVersion {
		fmt.Printf("O365 Mailbox Downloader Version: %s\n", version)
		os.Exit(0)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Override config with flags if they were explicitly set
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token-string":
			cfg.TokenString = *tokenString
		case "token-file":
			cfg.TokenFile = *tokenFile
		case "token-env":
			cfg.TokenEnv = *tokenEnv
		case "remove-token-file":
			cfg.RemoveTokenFile = *removeTokenFile
		case "debug":
			cfg.DebugLogging = *debug
		case "processing-mode":
			cfg.ProcessingMode = *processingMode
		case "state":
			cfg.StateFilePath = *stateFilePath
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
		case "state-save-interval":
			cfg.StateSaveInterval = *stateSaveInterval
		case "bandwidth-limit-mbs":
			cfg.BandwidthLimitMBs = *bandwidthLimitMBs
		case "processed-folder":
			cfg.ProcessedFolder = *processedFolder
		case "error-folder":
			cfg.ErrorFolder = *errorFolder
		}
	})

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.WithField("signal", <-sigChan).Warn("Received interrupt signal, initiating graceful shutdown...")
		cancel()
	}()

	accessToken, err := loadAccessToken(cfg)
	if err != nil {
		log.Fatalf("Error loading access token: %v", err)
	}

	if !isValidEmail(*mailboxName) {
		log.WithField("argument", "mailboxName").Fatalf("Error: Invalid mailbox name format: %s", *mailboxName)
	}
	if cfg.ProcessingMode != "full" && cfg.ProcessingMode != "incremental" && cfg.ProcessingMode != "route" {
		log.WithField("argument", "processing-mode").Fatalf("Error: Invalid processing mode. Must be 'full', 'incremental', or 'route'.")
	}
	if cfg.ProcessingMode == "incremental" && cfg.StateFilePath == "" {
		log.WithField("argument", "state").Fatalf("Error: State file path must be provided for incremental processing mode.")
	}
	if cfg.ProcessingMode == "route" {
		if cfg.ProcessedFolder == "" {
			log.WithField("argument", "processed-folder").Fatalf("Error: Processed folder name must be provided for route mode.")
		}
		if cfg.ErrorFolder == "" {
			log.WithField("argument", "error-folder").Fatalf("Error: Error folder name must be provided for route mode.")
		}
	}

	o365Client := o365client.NewO365Client(accessToken, time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second, cfg.MaxRetries, cfg.InitialBackoffSeconds, cfg.APICallsPerSecond, cfg.APIBurst, rng)

	if *healthCheck {
		fmt.Println("O365 Mailbox Downloader - Health Check Mode")
		log.Infof("Version: %s", version)
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

	runDownloadMode(ctx, cfg, accessToken, *mailboxName, *workspacePath, rng)
}

func runRouteMode(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor *emailprocessor.EmailProcessor, fileHandler *filehandler.FileHandler, accessToken, mailboxName string, stats *RunStats) {
	log.Info("Running in route mode.")

	// Get or create folder IDs
	processedFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.ProcessedFolder)
	if err != nil {
		log.Fatalf("Failed to get or create processed folder: %v", err)
	}
	errorFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.ErrorFolder)
	if err != nil {
		log.Fatalf("Failed to get or create error folder: %v", err)
	}

	// In route mode, we process all messages, so we start with an empty state.
	state := &o365client.RunState{}
	messagesChan := make(chan o365client.Message, cfg.MaxParallelDownloads*2)
	var producerWg, workerWg sync.WaitGroup
	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	// Producer to fetch all messages
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		if err := o365Client.GetMessages(ctx, mailboxName, state, messagesChan); err != nil {
			log.Fatalf("O365 API error fetching messages for routing: %v", err)
		}
	}()

	// Workers to process and then move messages
	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for msg := range messagesChan {
				semaphore <- struct{}{}
				var processingError bool

				log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Info("Routing message.")
				atomic.AddUint32(&stats.messagesProcessed, 1)

				// Process body
				cleanedBody, err := emailProcessor.CleanHTML(msg.Body.Content)
				if err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Warn("Failed to clean HTML for message.")
					cleanedBody = msg.Body.Content
					processingError = true
				}
				if err := fileHandler.SaveEmailBody(msg.Subject, msg.ID, cleanedBody); err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("Error saving email body.")
					processingError = true
				}

				// Process attachments
				if msg.HasAttachments {
					for _, att := range msg.Attachments {
						var err error
						if att.DownloadURL != "" {
							err = fileHandler.SaveAttachment(ctx, att.Name, msg.ID, att.DownloadURL, accessToken, att.Size)
						} else if att.ContentBytes != "" {
							err = fileHandler.SaveAttachmentFromBytes(att.Name, msg.ID, att.ContentBytes)
						} else {
							atomic.AddUint32(&stats.nonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID}).Warn("Skipping attachment: No download URL or content bytes.")
							continue
						}

						if err != nil {
							atomic.AddUint32(&stats.nonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID, "error": err}).Error("Failed to save attachment.")
							processingError = true
						} else {
							atomic.AddUint32(&stats.attachmentsProcessed, 1)
						}
					}
				}

				// Move message based on outcome
				destinationID := processedFolderID
				if processingError {
					destinationID = errorFolderID
				}
				if err := o365Client.MoveMessage(ctx, mailboxName, msg.ID, destinationID); err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("Failed to move message.")
				} else {
					log.WithField("messageID", msg.ID).Infof("Successfully processed and moved message.")
				}
				<-semaphore
			}
		}()
	}

	producerWg.Wait()
	close(messagesChan)
	workerWg.Wait()
}

func loadAccessToken(cfg *Config) (string, error) {
	// Validate that exactly one token source is specified
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

	// Load token from the specified source
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

	return "", fmt.Errorf("internal error: no token source identified") // Should be unreachable
}

type AttachmentJob struct {
	Attachment  o365client.Attachment
	MessageID   string
	AccessToken string
}

type RunStats struct {
	messagesProcessed    uint32
	attachmentsProcessed uint32
	nonFatalErrors       uint32
}

func runDownloadMode(ctx context.Context, cfg *Config, accessToken, mailboxName, workspacePath string, rng *rand.Rand) {
	stats := &RunStats{}
	startTime := time.Now()

	defer func() {
		log.Info("Application finished.")
		fmt.Println("\n--- Run Summary ---")
		fmt.Printf("Total execution time: %s\n", time.Since(startTime).Round(time.Second))
		fmt.Printf("Messages processed: %d\n", atomic.LoadUint32(&stats.messagesProcessed))
		fmt.Printf("Attachments downloaded: %d\n", atomic.LoadUint32(&stats.attachmentsProcessed))
		fmt.Printf("Non-fatal errors: %d\n", atomic.LoadUint32(&stats.nonFatalErrors))
		fmt.Println("-----------------")
	}()

	if err := validateWorkspacePath(workspacePath); err != nil {
		log.Fatalf("Error validating workspace path: %v", err)
	}

	fmt.Println("O365 Mailbox Downloader")
	log.Infof("Version: %s", version)
	log.Info("Application started.")

	o365Client := o365client.NewO365Client(accessToken, time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second, cfg.MaxRetries, cfg.InitialBackoffSeconds, cfg.APICallsPerSecond, cfg.APIBurst, rng)
	emailProcessor := emailprocessor.NewEmailProcessor()
	fileHandler := filehandler.NewFileHandler(workspacePath, o365Client, cfg.LargeAttachmentThresholdMB, cfg.ChunkSizeMB, cfg.BandwidthLimitMBs)

	if err := fileHandler.CreateWorkspace(); err != nil {
		log.Fatalf("Error creating workspace: %v", err)
	}
	log.WithField("path", workspacePath).Infof("Workspace created.")

	if cfg.ProcessingMode == "route" {
		runRouteMode(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, mailboxName, stats)
		return
	}

	// Logic for 'full' and 'incremental' modes
	var state *o365client.RunState
	var err error
	if cfg.ProcessingMode == "incremental" {
		state, err = fileHandler.LoadState(cfg.StateFilePath)
		if err != nil {
			log.Fatalf("Error loading state file: %v", err)
		}
		if !state.LastRunTimestamp.IsZero() {
			log.WithFields(log.Fields{
				"lastRunTimestamp": state.LastRunTimestamp.Format(time.RFC3339Nano),
				"lastMessageID":    state.LastMessageID,
			}).Infof("Found previous state. Fetching emails since last run.")
		} else {
			log.Info("No previous state found. Fetching all available emails.")
		}
	} else {
		log.Info("Running in full processing mode. Fetching all available emails.")
		state = &o365client.RunState{}
	}

	var newLatestMessage o365client.Message
	var mu sync.Mutex

	messagesChan := make(chan o365client.Message, cfg.MaxParallelDownloads*2)
	attachmentsChan := make(chan AttachmentJob, cfg.MaxParallelDownloads*4)

	var producerWg, processorWg, downloaderWg sync.WaitGroup

	// Shared semaphore for all API-bound workers
	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	// --- Producer ---
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		err := o365Client.GetMessages(ctx, mailboxName, state, messagesChan)
		if err != nil {
			log.Fatalf("O365 API error fetching messages: %v", err)
		}
	}()

	// --- Message Processors ---
	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		processorWg.Add(1)
		go func() {
			defer processorWg.Done()
			for msg := range messagesChan {
				semaphore <- struct{}{}
				atomic.AddUint32(&stats.messagesProcessed, 1)
				log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Infof("Processing message.")

				cleanedBody, err := emailProcessor.CleanHTML(msg.Body.Content)
				if err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Warn("Failed to clean HTML for message.")
					cleanedBody = msg.Body.Content
				}
				if err := fileHandler.SaveEmailBody(msg.Subject, msg.ID, cleanedBody); err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("Error saving email body.")
				}

				if msg.HasAttachments {
					log.WithFields(log.Fields{"count": len(msg.Attachments), "messageID": msg.ID}).Infof("Found attachments.")
					for _, att := range msg.Attachments {
						attachmentsChan <- AttachmentJob{
							Attachment:  att,
							MessageID:   msg.ID,
							AccessToken: accessToken,
						}
					}
				}

				mu.Lock()
				if msg.ReceivedDateTime.After(newLatestMessage.ReceivedDateTime) || newLatestMessage.ID == "" {
					newLatestMessage = msg
				}
				// Periodically save state
				processedCount := atomic.LoadUint32(&stats.messagesProcessed)
				if processedCount%uint32(cfg.StateSaveInterval) == 0 {
					log.WithField("messageCount", processedCount).Info("Periodically saving state.")
					if err := fileHandler.SaveState(&o365client.RunState{LastRunTimestamp: newLatestMessage.ReceivedDateTime, LastMessageID: newLatestMessage.ID}, cfg.StateFilePath); err != nil {
						log.WithField("error", err).Error("Failed to periodically save state.")
					}
				}
				mu.Unlock()
				<-semaphore
			}
		}()
	}

	// --- Attachment Downloaders ---
	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		downloaderWg.Add(1)
		go func() {
			defer downloaderWg.Done()
			for job := range attachmentsChan {
				semaphore <- struct{}{}
				log.WithFields(log.Fields{
					"attachmentName": job.Attachment.Name,
					"messageID":      job.MessageID,
				}).Debug("Processing attachment.")

				var err error
				if job.Attachment.DownloadURL != "" {
					err = fileHandler.SaveAttachment(ctx, job.Attachment.Name, job.MessageID, job.Attachment.DownloadURL, job.AccessToken, job.Attachment.Size)
				} else if job.Attachment.ContentBytes != "" {
					err = fileHandler.SaveAttachmentFromBytes(job.Attachment.Name, job.MessageID, job.Attachment.ContentBytes)
				} else {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": job.Attachment.Name, "messageID": job.MessageID}).Warn("Skipping attachment: No download URL or content bytes.")
					<-semaphore
					continue
				}

				if err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": job.Attachment.Name, "messageID": job.MessageID, "error": err}).Error("Failed to save attachment.")
				} else {
					atomic.AddUint32(&stats.attachmentsProcessed, 1)
				}
				<-semaphore
			}
		}()
	}

	// Wait for producer to finish, then close message channel
	producerWg.Wait()
	close(messagesChan)

	// Wait for processors to finish, then close attachment channel
	processorWg.Wait()
	close(attachmentsChan)

	// Wait for downloaders to finish
	downloaderWg.Wait()

	if cfg.ProcessingMode == "incremental" && newLatestMessage.ID != "" {
		newState := &o365client.RunState{
			LastRunTimestamp: newLatestMessage.ReceivedDateTime,
			LastMessageID:    newLatestMessage.ID,
		}
		log.WithFields(log.Fields{
			"timestamp": newState.LastRunTimestamp.Format(time.RFC3339Nano),
			"messageID": newState.LastMessageID,
		}).Debugln("Saving new state.")
		if err := fileHandler.SaveState(newState, cfg.StateFilePath); err != nil {
			log.Errorf("Error saving state file: %v", err)
		}
	}
}

func runHealthCheckMode(ctx context.Context, client *o365client.O365Client, mailboxName string) {
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

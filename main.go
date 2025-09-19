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
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	debug := flag.Bool("debug", false, "Enable debug logging")
	processingMode := flag.String("processing-mode", "full", "Processing mode: 'full' or 'incremental'")
	stateFilePath := flag.String("state", "", "Path to the state file for incremental processing")
	flag.Parse()

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if *debug {
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

	if *timeoutSeconds != 120 {
		cfg.HTTPClientTimeoutSeconds = *timeoutSeconds
	}
	if *maxParallelDownloads != 10 {
		cfg.MaxParallelDownloads = *maxParallelDownloads
	}
	if *apiCallsPerSecond != 0 {
		cfg.APICallsPerSecond = *apiCallsPerSecond
	}
	if *apiBurst != 0 {
		cfg.APIBurst = *apiBurst
	}
	if *maxRetries != 2 {
		cfg.MaxRetries = *maxRetries
	}
	if *initialBackoffSeconds != 5 {
		cfg.InitialBackoffSeconds = *initialBackoffSeconds
	}
	if *chunkSizeMB != 8 {
		cfg.ChunkSizeMB = *chunkSizeMB
	}
	if *largeAttachmentThresholdMB != 20 {
		cfg.LargeAttachmentThresholdMB = *largeAttachmentThresholdMB
	}

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

	accessToken, err := loadAccessToken(*tokenString, *tokenFile, *tokenEnv)
	if err != nil {
		log.Fatalf("Error loading access token: %v", err)
	}

	if !isValidEmail(*mailboxName) {
		log.WithField("argument", "mailboxName").Fatalf("Error: Invalid mailbox name format: %s", *mailboxName)
	}
	if *processingMode != "full" && *processingMode != "incremental" {
		log.WithField("argument", "processing-mode").Fatalf("Error: Invalid processing mode. Must be 'full' or 'incremental'.")
	}
	if *processingMode == "incremental" && *stateFilePath == "" {
		log.WithField("argument", "state").Fatalf("Error: State file path must be provided for incremental processing mode.")
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
		if *tokenFile != "" && *removeTokenFile {
			log.WithField("file", *tokenFile).Info("Removing token file as requested.")
			if err := os.Remove(*tokenFile); err != nil {
				log.WithField("file", *tokenFile).Errorf("Failed to remove token file: %v", err)
			}
		}
	}()

	runDownloadMode(ctx, cfg, accessToken, *mailboxName, *workspacePath, *processingMode, *stateFilePath, rng)
}

func loadAccessToken(tokenString, tokenFile string, tokenEnv bool) (string, error) {
	// Validate that exactly one token source is specified
	sourceCount := 0
	if tokenString != "" {
		sourceCount++
	}
	if tokenFile != "" {
		sourceCount++
	}
	if tokenEnv {
		sourceCount++
	}

	if sourceCount == 0 {
		return "", fmt.Errorf("no token source specified. Please use one of -token-string, -token-file, or -token-env")
	}
	if sourceCount > 1 {
		return "", fmt.Errorf("multiple token sources specified. Please use only one of -token-string, -token-file, or -token-env")
	}

	// Load token from the specified source
	if tokenString != "" {
		log.Info("Using token from -token-string flag.")
		return tokenString, nil
	}

	if tokenFile != "" {
		log.Info("Using token from file specified by -token-file.")
		content, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %s: %w", tokenFile, err)
		}
		return strings.TrimSpace(string(content)), nil
	}

	if tokenEnv {
		log.Info("Using token from JWT_TOKEN environment variable.")
		token := os.Getenv("JWT_TOKEN")
		if token == "" {
			return "", fmt.Errorf("-token-env specified, but JWT_TOKEN environment variable is not set")
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

func runDownloadMode(ctx context.Context, cfg *Config, accessToken, mailboxName, workspacePath, processingMode, stateFilePath string, rng *rand.Rand) {
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

	var state *o365client.RunState
	var err error
	if processingMode == "incremental" {
		state, err = fileHandler.LoadState(stateFilePath)
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
					if err := fileHandler.SaveState(&o365client.RunState{LastRunTimestamp: newLatestMessage.ReceivedDateTime, LastMessageID: newLatestMessage.ID}, stateFilePath); err != nil {
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

	if processingMode == "incremental" && newLatestMessage.ID != "" {
		newState := &o365client.RunState{
			LastRunTimestamp: newLatestMessage.ReceivedDateTime,
			LastMessageID:    newLatestMessage.ID,
		}
		log.WithFields(log.Fields{
			"timestamp": newState.LastRunTimestamp.Format(time.RFC3339Nano),
			"messageID": newState.LastMessageID,
		}).Debugln("Saving new state.")
		if err := fileHandler.SaveState(newState, stateFilePath); err != nil {
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

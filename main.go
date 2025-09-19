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

	accessToken := flag.String("token", "", "Access token for O356 API")
	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	timeoutSeconds := flag.Int("timeout", 120, "HTTP client timeout in seconds (default: 120)")
	maxParallelDownloads := flag.Int("parallel", 10, "Maximum number of parallel downloads (default: 10)")
	configPath := flag.String("config", "", "Path to the configuration file (JSON)")
	apiCallsPerSecond := flag.Float64("api-rate", 0, "API calls per second for client-side rate limiting (default: 5.0)")
	apiBurst := flag.Int("api-burst", 0, "API burst capacity for client-side rate limiting (default: 10)")
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

	if *accessToken == "" {
		log.WithField("argument", "accessToken").Fatalf("Error: Access token is missing.")
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

	o365Client := o365client.NewO365Client(*accessToken, time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second, cfg.MaxRetries, cfg.InitialBackoffSeconds, cfg.APICallsPerSecond, cfg.APIBurst, rng)

	if *healthCheck {
		fmt.Println("O365 Mailbox Downloader - Health Check Mode")
		log.Infof("Version: %s", version)
		runHealthCheckMode(ctx, o365Client, *mailboxName)
		os.Exit(0)
	}

	runDownloadMode(ctx, cfg, *accessToken, *mailboxName, *workspacePath, *processingMode, *stateFilePath, rng)
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
	fileHandler := filehandler.NewFileHandler(workspacePath, o365Client, cfg.LargeAttachmentThresholdMB, cfg.ChunkSizeMB)

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

	messages, err := o365Client.GetMessages(ctx, mailboxName, state)
	if err != nil {
		log.Fatalf("O365 API error fetching messages: %v", err)
	}
	log.WithField("count", len(messages)).Infof("Fetched messages.")

	var messagesToProcess []o365client.Message
	if processingMode == "incremental" && state.LastMessageID != "" && len(messages) > 0 && messages[0].ID == state.LastMessageID {
		messagesToProcess = messages[1:]
		log.WithField("messageID", state.LastMessageID).Debug("Skipping first message as it was the last one processed in the previous run.")
	} else {
		messagesToProcess = messages
	}

	if len(messagesToProcess) == 0 {
		log.Info("No new messages to process.")
		return
	}

	var newLatestMessage o365client.Message
	var mu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	for _, msg := range messagesToProcess {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(msg o365client.Message) {
			defer wg.Done()
			defer func() { <-semaphore }()

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
				attachments, err := o365Client.GetAttachments(ctx, mailboxName, msg.ID)
				if err != nil {
					atomic.AddUint32(&stats.nonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("O365 API error fetching attachments.")
					return
				}
				log.WithFields(log.Fields{"count": len(attachments), "messageID": msg.ID}).Infof("Found attachments.")

				var attWg sync.WaitGroup
				for _, att := range attachments {
					attWg.Add(1)
					go func(att o365client.Attachment) {
						defer attWg.Done()
						detailedAtt, err := o365Client.GetAttachmentDetails(ctx, mailboxName, msg.ID, att.ID)
						if err != nil {
							atomic.AddUint32(&stats.nonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID, "error": err}).Error("Failed to get attachment details.")
							return
						}

						log.WithFields(log.Fields{
							"attachmentName":  detailedAtt.Name,
							"messageID":       msg.ID,
							"attachmentType":  detailedAtt.ODataType,
							"hasDownloadURL":  detailedAtt.DownloadURL != "",
							"hasContentBytes": detailedAtt.ContentBytes != "",
						}).Debug("Processing attachment.")

						if detailedAtt.DownloadURL != "" {
							err = fileHandler.SaveAttachment(ctx, detailedAtt.Name, msg.ID, detailedAtt.DownloadURL, accessToken, detailedAtt.Size)
						} else if detailedAtt.ContentBytes != "" {
							err = fileHandler.SaveAttachmentFromBytes(detailedAtt.Name, msg.ID, detailedAtt.ContentBytes)
						} else {
							atomic.AddUint32(&stats.nonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": detailedAtt.Name, "messageID": msg.ID}).Warn("Skipping attachment: No download URL or content bytes.")
							return
						}
						if err != nil {
							atomic.AddUint32(&stats.nonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID, "error": err}).Error("Failed to save attachment.")
						} else {
							atomic.AddUint32(&stats.attachmentsProcessed, 1)
						}
					}(att)
				}
				attWg.Wait()
			}

			mu.Lock()
			if msg.ReceivedDateTime.After(newLatestMessage.ReceivedDateTime) || newLatestMessage.ID == "" {
				newLatestMessage = msg
			}
			mu.Unlock()
		}(msg)
	}
	wg.Wait()

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

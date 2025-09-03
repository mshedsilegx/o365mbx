package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand" // Added for seeding random number generator
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"o365mbx/apperrors"
	"o365mbx/emailprocessor"
	"o365mbx/filehandler"
	"o365mbx/o365client"
)

var version = "dev" // Global variable to hold the version

// isValidEmail checks if a string is a valid email address.
func isValidEmail(email string) bool {
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
}

// validateWorkspacePath checks and creates the workspace directory.
func validateWorkspacePath(path string) error {
	if path == "" {
		return fmt.Errorf("workspace path cannot be empty")
	}
	// Check if the path is absolute
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace path must be an absolute path: %s", path)
	}
	// Attempt to create the directory if it doesn't exist
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("failed to create workspace directory %s: %w", path, err)
	}
	// Basic check to ensure it's a directory and we can access it
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat workspace directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace path %s is not a directory", path)
	}
	return nil
}

func main() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // Create a local random number generator

	configPath := flag.String("config", "", "Path to the configuration file (JSON)")
	displayVersion := flag.Bool("version", false, "Display application version")
	healthCheck := flag.Bool("healthcheck", false, "Perform a health check on the mailbox and exit") // New flag for healthcheck

	// Define flags for settings that can be in the config file or command line
	accessToken := flag.String("token", "", "Access token for O356 API")
	mailboxName := flag.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := flag.String("workspace", "", "Unique folder to store all artifacts")
	processedFolder := flag.String("processed-folder", "", "Folder to move processed emails to")
	timeoutSeconds := flag.Int("timeout", 0, "HTTP client timeout in seconds")
	maxParallelDownloads := flag.Int("parallel", 0, "Maximum number of parallel downloads")
	apiCallsPerSecond := flag.Float64("api-rate", 0, "API calls per second for client-side rate limiting (default: 5.0)")
	apiBurst := flag.Int("api-burst", 0, "API burst capacity for client-side rate limiting (default: 10)")
	flag.Parse()

	if *displayVersion {
		fmt.Printf("O365 Mailbox Downloader Version: %s\n", version)
		os.Exit(0) // Exit cleanly after displaying version
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Override config values with command-line arguments if provided
	if *accessToken != "" {
		cfg.AccessToken = *accessToken
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

	// Validate loaded configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
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
	if !isValidEmail(cfg.MailboxName) {
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

		runHealthCheckMode(ctx, o365Client, cfg.MailboxName)
		os.Exit(0) // Exit after health check
	}

	// Normal Download Mode
	runDownloadMode(ctx, cfg, rng)
}

// runDownloadMode handles the main logic for downloading emails and attachments.
func runDownloadMode(ctx context.Context, cfg *Config, rng *rand.Rand) {
	// Argument validation specific to download mode
	if err := validateWorkspacePath(cfg.WorkspacePath); err != nil {
		if fsErr, ok := err.(*apperrors.FileSystemError); ok {
			log.WithFields(log.Fields{
				"argument": "workspacePath",
				"path":     fsErr.Path,
				"error":    fsErr.Unwrap(),
			}).Fatalf("Critical file system error: %s", fsErr.Msg)
		} else {
			log.WithField("argument", "workspacePath").Fatalf("Error validating workspace path: %v", err)
		}
	}

	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	log.SetLevel(log.InfoLevel)

	fmt.Println("O365 Mailbox Downloader")
	log.Infof("Version: %s", version) // Display the version
	log.Info("Application started.")

	// Initialize components
	o365Client := o365client.NewO365Client(
		cfg.AccessToken,
		time.Duration(cfg.HTTPClientTimeoutSeconds)*time.Second,
		cfg.MaxRetries,
		cfg.InitialBackoffSeconds,
		cfg.APICallsPerSecond,
		cfg.APIBurst,
		rng, // Pass the local random number generator
	)
	emailProcessor := emailprocessor.NewEmailProcessor()
	fileHandler := filehandler.NewFileHandler(
		cfg.WorkspacePath,
		o365Client,
		cfg.LargeAttachmentThresholdMB,
		cfg.ChunkSizeMB,
	)

	// 1. Create workspace
	err := fileHandler.CreateWorkspace()
	if err != nil {
		if fsErr, ok := err.(*apperrors.FileSystemError); ok {
			log.Fatalf("Critical file system error: %s (Path: %s, Original: %v)", fsErr.Msg, fsErr.Path, fsErr.Unwrap())
		} else {
			log.Errorf("Error creating workspace: %v", err)
		}
	}
	log.WithField("path", cfg.WorkspacePath).Infof("Workspace created.")

	// Load last run timestamp
	var lastRunTimestamp string
	lastRunTimestamp, err = fileHandler.LoadLastRunTimestamp()
	if err != nil {
		if fsErr, ok := err.(*apperrors.FileSystemError); ok {
			log.Fatalf("Critical file system error: %s (Path: %s, Original: %v)", fsErr.Msg, fsErr.Path, fsErr.Unwrap())
		} else {
			log.Errorf("Error loading last run timestamp: %v", err)
		}
	}
	if lastRunTimestamp != "" {
		log.WithField("lastRunTimestamp", lastRunTimestamp).Infof("Fetching emails since last run.")
	} else {
		log.Info("No last run timestamp found. Fetching all available emails.")
	}

	// 2. Fetch emails
	var messages []o365client.Message
	messages, err = o365Client.GetMessages(ctx, cfg.MailboxName, lastRunTimestamp)
	if err != nil {
		switch e := err.(type) {
		case *apperrors.APIError:
			log.WithFields(log.Fields{
				"errorType":  "APIError",
				"statusCode": e.StatusCode,
			}).Fatalf("O365 API error fetching messages: %s", e.Msg)
		case *apperrors.AuthError:
			log.WithFields(log.Fields{
				"errorType": "AuthError",
			}).Fatalf("Authentication failed: %s. Please check your access token.", e.Msg)
		default:
			// Check for context cancellation errors
			if errors.Is(err, context.Canceled) {
				log.WithField("errorType", "ContextCanceled").Fatalf("Message fetching cancelled by user.")
			} else if errors.Is(err, context.DeadlineExceeded) {
				log.WithField("errorType", "ContextDeadlineExceeded").Fatalf("Message fetching timed out.")
			} else {
				log.WithField("errorType", "UnknownError").Fatalf("Unknown error fetching messages: %v", err)
			}
		}
	}
	log.WithField("count", len(messages)).Infof("Fetched messages.")

	// Semaphore to limit concurrent goroutines
	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)
	var wg sync.WaitGroup

	// 3. Process and save emails and attachments in parallel
	var latestTimestamp time.Time
	var mu sync.Mutex // Mutex to protect latestTimestamp

	processedFolderID, err := o365Client.GetFolderID(ctx, cfg.MailboxName, cfg.ProcessedFolder)
	if err != nil {
		log.Fatalf("Failed to get processed folder ID: %v", err)
	}

	for _, msg := range messages {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(msg o365client.Message) {
			defer wg.Done()
			defer func() { <-semaphore }()

			log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Infof("Processing message.")

			var cleanedBody string
			cleanedBody, err = emailProcessor.CleanHTML(msg.Body.Content)
			if err != nil {
				log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Warn("Failed to clean HTML for message.")
				cleanedBody = msg.Body.Content // Use original if cleaning fails
			}

			emailData := filehandler.EmailData{
				To:           make([]string, len(msg.ToRecipients)),
				From:         msg.From.EmailAddress.Address,
				Subject:      msg.Subject,
				ReceivedDate: msg.ReceivedDateTime.Format(time.RFC3339),
				Body:         cleanedBody,
				Attachments:  []filehandler.AttachmentData{},
			}
			for i, recipient := range msg.ToRecipients {
				emailData.To[i] = recipient.EmailAddress.Address
			}

			// Download and save attachments
			if msg.HasAttachments {
				var attachments []o365client.Attachment
				attachments, err = o365Client.GetAttachments(ctx, cfg.MailboxName, msg.ID)
				if err != nil {
					switch e := err.(type) {
					case *apperrors.APIError:
						log.WithFields(log.Fields{
							"messageID":  msg.ID,
							"errorType":  "APIError",
							"statusCode": e.StatusCode,
						}).Errorf("O365 API error fetching attachments: %s", e.Msg)
					case *apperrors.AuthError:
						log.WithFields(log.Fields{
							"messageID": msg.ID,
							"errorType": "AuthError",
						}).Errorf("Authentication failed for attachments: %s", e.Msg)
					default:
						// Check for context cancellation errors
						if errors.Is(err, context.Canceled) {
							log.WithFields(log.Fields{
								"messageID": msg.ID,
								"errorType": "ContextCanceled",
							}).Errorf("Attachment fetching cancelled by user.")
						} else if errors.Is(err, context.DeadlineExceeded) {
							log.WithFields(log.Fields{
								"messageID": msg.ID,
								"errorType": "ContextDeadlineExceeded",
							}).Errorf("Attachment fetching timed out.")
						} else {
							log.WithFields(log.Fields{
								"messageID": msg.ID,
								"errorType": "UnknownError",
								"error":     err,
							}).Errorf("Unknown error fetching attachments.")
						}
					}
					return // Skip attachments for this message if fetching fails
				}
				log.WithFields(log.Fields{"count": len(attachments), "messageID": msg.ID}).Infof("Found attachments.")

				var attWg sync.WaitGroup // WaitGroup for attachments within this message
				for _, att := range attachments {
					attWg.Add(1)
					go func(att o365client.Attachment) {
						defer attWg.Done()
						log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID}).Infof("Downloading attachment.")
						newFileName, err := fileHandler.SaveAttachment(ctx, att.Name, msg.ID, att.DownloadURL, cfg.AccessToken, att.Size)
						if err != nil {
							switch e := err.(type) {
							case *apperrors.FileSystemError:
								log.WithFields(log.Fields{
									"attachmentName": att.Name,
									"messageID":      msg.ID,
									"errorType":      "FileSystemError",
									"path":           e.Path,
									"error":          e.Unwrap(),
								}).Errorf("File system error saving attachment: %s", e.Msg)
							case *apperrors.APIError:
								log.WithFields(log.Fields{
									"attachmentName": att.Name,
									"messageID":      msg.ID,
									"errorType":      "APIError",
									"statusCode":     e.StatusCode,
								}).Errorf("API error downloading attachment: %s", e.Msg)
							default:
								if errors.Is(err, context.Canceled) {
									log.WithFields(log.Fields{
										"attachmentName": att.Name,
										"messageID":      msg.ID,
										"errorType":      "ContextCanceled",
									}).Errorf("Attachment download cancelled by user.")
								} else if errors.Is(err, context.DeadlineExceeded) {
									log.WithFields(log.Fields{
										"attachmentName": att.Name,
										"messageID":      msg.ID,
										"errorType":      "ContextDeadlineExceeded",
									}).Errorf("Attachment download timed out.")
								} else {
									log.WithFields(log.Fields{
										"attachmentName": att.Name,
										"messageID":      msg.ID,
										"errorType":      "UnknownError",
										"error":          err,
									}).Errorf("Unknown error saving attachment.")
								}
							}
						} else {
							mu.Lock()
							emailData.Attachments = append(emailData.Attachments, filehandler.AttachmentData{
								Name:        att.Name,
								Size:        att.Size,
								DownloadURL: newFileName,
							})
							mu.Unlock()
						}
					}(att)
				}
				attWg.Wait() // Wait for all attachments of this message to complete
			}

			err = fileHandler.SaveEmailAsJSON(msg.ID, emailData)
			if err != nil {
				if fsErr, ok := err.(*apperrors.FileSystemError); ok {
					log.WithFields(log.Fields{
						"messageID": msg.ID,
						"path":      fsErr.Path,
						"error":     fsErr.Unwrap(),
					}).Errorf("File system error saving email JSON: %s", fsErr.Msg)
				} else {
					log.WithFields(log.Fields{
						"messageID": msg.ID,
						"error":     err,
					}).Errorf("Error saving email JSON.")
				}
				return // Do not move message if saving fails
			}

			// Move the message to the processed folder
			err = o365Client.MoveMessage(ctx, cfg.MailboxName, msg.ID, processedFolderID)
			if err != nil {
				log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("Failed to move message.")
				return // Do not update timestamp if move fails
			}

			// Update latestTimestamp safely
			mu.Lock()
			if msg.ReceivedDateTime.After(latestTimestamp) {
				latestTimestamp = msg.ReceivedDateTime
			}
			mu.Unlock()

		}(msg)
	}

	wg.Wait() // Wait for all message processing goroutines to complete

	// If no messages were fetched, use current time as latest timestamp
	if latestTimestamp.IsZero() {
		latestTimestamp = time.Now()
	}

	// Save latest timestamp for next run
	err = fileHandler.SaveLastRunTimestamp(latestTimestamp.Format(time.RFC3339))
	if err != nil {
		if fsErr, ok := err.(*apperrors.FileSystemError); ok {
			log.WithFields(log.Fields{
				"errorType": "FileSystemError",
				"path":      fsErr.Path,
				"error":     fsErr.Unwrap(),
			}).Errorf("File system error saving last run timestamp: %s", fsErr.Msg)
		} else {
			log.WithField("error", err).Errorf("Error saving last run timestamp.")
		}
	}

	log.Info("Application finished.")
}

// runHealthCheckMode handles the logic for the health check mode.
func runHealthCheckMode(ctx context.Context, client *o365client.O365Client, mailboxName string) {
	log.WithField("mailbox", mailboxName).Info("Attempting to connect to mailbox and retrieve statistics...")

	messageCount, err := client.GetMailboxStatistics(ctx, mailboxName)
	if err != nil {
		switch e := err.(type) {
		case *apperrors.APIError:
			log.WithFields(log.Fields{
				"errorType":  "APIError",
				"statusCode": e.StatusCode,
			}).Errorf("O365 API error during connect check: %s", e.Msg)
		case *apperrors.AuthError:
			log.WithFields(log.Fields{
				"errorType": "AuthError",
			}).Errorf("Authentication failed during connect check: %s. Please check your access token.", e.Msg)
		default:
			if errors.Is(err, context.Canceled) {
				log.WithField("errorType", "ContextCanceled").Errorf("Connect check cancelled by user.")
			} else if errors.Is(err, context.DeadlineExceeded) {
				log.WithField("errorType", "ContextDeadlineExceeded").Errorf("Connect check timed out.")
			} else {
				log.WithField("errorType", "UnknownError").Errorf("Unknown error during connect check: %v", err)
			}
		}
		return
	}

	log.WithFields(log.Fields{
		"mailbox":      mailboxName,
		"messageCount": messageCount,
	}).Info("Mailbox connectivity successful. Statistics:")
	fmt.Printf("\nMailbox: %s\n", mailboxName)
	fmt.Printf("Total Messages: %d\n", messageCount)
	// Add more statistics here as needed
}

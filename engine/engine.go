package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"o365mbx/apperrors"
	"o365mbx/config"
	"o365mbx/emailprocessor"
	"o365mbx/filehandler"
	"o365mbx/o365client"

	log "github.com/sirupsen/logrus"
)

// Run handles the main logic for downloading emails and attachments.
func Run(ctx context.Context, cfg *config.Config, rng *rand.Rand) {
	// Argument validation specific to download mode
	if err := validateWorkspacePath(cfg.WorkspacePath, os.FileMode(cfg.DirPerms)); err != nil {
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
	log.Infof("Version: %s", "dev") // TODO: Find a way to pass version from main
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
		cfg.DirPerms,
		cfg.FilePerms,
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
	if cfg.ProcessingMode == "incremental" {
		var err error
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
	} else {
		log.Infof("Running in '%s' mode. Fetching all available emails from Inbox.", cfg.ProcessingMode)
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

	var processedFolderID, errorFolderID string
	if cfg.ProcessingMode == "route" {
		var err error
		processedFolderID, err = o365Client.GetFolderID(ctx, cfg.MailboxName, cfg.ProcessedFolder)
		if err != nil {
			log.Fatalf("Failed to get processed folder ID: %v", err)
		}
		errorFolderID, err = o365Client.GetFolderID(ctx, cfg.MailboxName, cfg.ErrorFolder)
		if err != nil {
			log.Fatalf("Failed to get error folder ID: %v", err)
		}
	}

	for _, msg := range messages {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(msg o365client.Message) {
			defer wg.Done()
			defer func() { <-semaphore }()

			log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Infof("Processing message.")

			var errs []string
			var attachmentData []filehandler.AttachmentData

			// 1. Clean Body
			cleanedBody, err := emailProcessor.CleanHTML(msg.Body.Content)
			if err != nil {
				errs = append(errs, fmt.Sprintf("HTML cleaning failed: %v", err))
				cleanedBody = msg.Body.Content // Use original on failure
			}

			// 2. Process Attachments
			if msg.HasAttachments {
				attachments, err := o365Client.GetAttachments(ctx, cfg.MailboxName, msg.ID)
				if err != nil {
					errs = append(errs, fmt.Sprintf("Failed to get attachment list: %v", err))
				} else {
					var attWg sync.WaitGroup
					for _, att := range attachments {
						attWg.Add(1)
						go func(att o365client.Attachment) {
							defer attWg.Done()
							newFileName, err := fileHandler.SaveAttachment(ctx, att.Name, msg.ID, att.DownloadURL, cfg.AccessToken, att.Size)
							if err != nil {
								mu.Lock()
								errs = append(errs, fmt.Sprintf("Failed to save attachment '%s': %v", att.Name, err))
								mu.Unlock()
							} else {
								mu.Lock()
								attachmentData = append(attachmentData, filehandler.AttachmentData{
									Name:        att.Name,
									Size:        att.Size,
									DownloadURL: newFileName,
								})
								mu.Unlock()
							}
						}(att)
					}
					attWg.Wait()
				}
			}

			// 3. Determine final status
			status := filehandler.StatusData{}
			isError := len(errs) > 0
			if isError {
				status.State = "error"
				status.Details = strings.Join(errs, "; ")
			} else {
				status.State = "success"
				status.Details = "Message processed successfully."
			}

			// 4. Create and save the JSON report
			emailData := filehandler.EmailData{
				To:           make([]string, len(msg.ToRecipients)),
				From:         msg.From.EmailAddress.Address,
				Subject:      msg.Subject,
				ReceivedDate: msg.ReceivedDateTime.Format(time.RFC3339),
				Body:         cleanedBody,
				Attachments:  attachmentData,
				Status:       status,
			}
			for i, recipient := range msg.ToRecipients {
				emailData.To[i] = recipient.EmailAddress.Address
			}

			jsonSaveErr := fileHandler.SaveEmailAsJSON(msg.ID, emailData)
			if jsonSaveErr != nil {
				isError = true
				log.Errorf("CRITICAL: Failed to save JSON report for msg %s: %v", msg.ID, jsonSaveErr)
			}

			// 5. Move the email
			if cfg.ProcessingMode == "route" {
				var moveErr error
				if isError {
					moveErr = o365Client.MoveMessage(ctx, cfg.MailboxName, msg.ID, errorFolderID)
				} else {
					moveErr = o365Client.MoveMessage(ctx, cfg.MailboxName, msg.ID, processedFolderID)
				}

				if moveErr != nil {
					log.Errorf("CRITICAL: Failed to move message %s to target folder: %v", msg.ID, moveErr)
					return // Don't update timestamp if move fails
				}
			}

			// 6. Update timestamp only on complete success
			if !isError {
				mu.Lock()
				if msg.ReceivedDateTime.After(latestTimestamp) {
					latestTimestamp = msg.ReceivedDateTime
				}
				mu.Unlock()
			}
		}(msg)
	}

	wg.Wait() // Wait for all message processing goroutines to complete

	// If no messages were fetched, use current time as latest timestamp
	if latestTimestamp.IsZero() {
		latestTimestamp = time.Now()
	}

	if cfg.ProcessingMode == "incremental" {
		// Save latest timestamp for next run
		err := fileHandler.SaveLastRunTimestamp(latestTimestamp.Format(time.RFC3339))
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
	}

	log.Info("Application finished.")
}

// IsValidEmail checks if a string is a valid email address.
func IsValidEmail(email string) bool {
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
}

// RunHealthCheck handles the logic for the health check mode.
func RunHealthCheck(ctx context.Context, client *o365client.O365Client, mailboxName string) {
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

// validateWorkspacePath checks and creates the workspace directory.
func validateWorkspacePath(path string, dirPerms os.FileMode) error {
	if path == "" {
		return fmt.Errorf("workspace path cannot be empty")
	}
	// Check if the path is absolute
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace path must be an absolute path: %s", path)
	}
	// Attempt to create the directory if it doesn't exist
	if err := os.MkdirAll(path, dirPerms); err != nil {
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

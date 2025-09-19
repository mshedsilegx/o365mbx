package engine

import (
	"context"
	"fmt"
	"io"
	"o365mbx/emailprocessor"
	"o365mbx/filehandler"
	"o365mbx/o365client"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

type AttachmentJob struct {
	Attachment  o365client.Attachment
	MessageID   string
	AccessToken string
}

type RunStats struct {
	MessagesProcessed    uint32
	AttachmentsProcessed uint32
	NonFatalErrors       uint32
}

func RunEngine(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor *emailprocessor.EmailProcessor, fileHandler *filehandler.FileHandler, accessToken, mailboxName, workspacePath, version string) {
	stats := &RunStats{}
	startTime := time.Now()

	defer func() {
		log.Info("Application finished.")
		fmt.Println("\n--- Run Summary ---")
		fmt.Printf("Total execution time: %s\n", time.Since(startTime).Round(time.Second))
		fmt.Printf("Messages processed: %d\n", atomic.LoadUint32(&stats.MessagesProcessed))
		fmt.Printf("Attachments downloaded: %d\n", atomic.LoadUint32(&stats.AttachmentsProcessed))
		fmt.Printf("Non-fatal errors: %d\n", atomic.LoadUint32(&stats.NonFatalErrors))
		fmt.Println("-----------------")
	}()

	if err := validateWorkspacePath(workspacePath); err != nil {
		log.Fatalf("Error validating workspacePath: %v", err)
	}

	fmt.Println("O365 Mailbox Downloader")
	log.Infof("Version: %s", version)
	log.Info("Application started.")

	if err := fileHandler.CreateWorkspace(); err != nil {
		log.Fatalf("Error creating workspace: %v", err)
	}
	log.WithField("path", workspacePath).Infof("Workspace created.")

	if cfg.ProcessingMode == "route" {
		runRouteMode(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, mailboxName, stats)
		return
	}

	runDownloadMode(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, mailboxName, stats)
}

func runDownloadMode(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor *emailprocessor.EmailProcessor, fileHandler *filehandler.FileHandler, accessToken, mailboxName string, stats *RunStats) {
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

	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	sourceFolderID := cfg.InboxFolder
	if strings.ToLower(sourceFolderID) != "inbox" {
		var err error
		sourceFolderID, err = o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.InboxFolder)
		if err != nil {
			log.Fatalf("Failed to get or create source folder '%s': %v", cfg.InboxFolder, err)
		}
	}

	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		err := o365Client.GetMessages(ctx, mailboxName, sourceFolderID, state, messagesChan)
		if err != nil {
			log.Fatalf("O365 API error fetching messages: %v", err)
		}
	}()

	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		processorWg.Add(1)
		go func() {
			defer processorWg.Done()
			for msg := range messagesChan {
				semaphore <- struct{}{}
				atomic.AddUint32(&stats.MessagesProcessed, 1)
				log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Infof("Processing message.")

				cleanedBody, err := emailProcessor.CleanHTML(msg.Body.Content)
				if err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Warn("Failed to clean HTML for message.")
					cleanedBody = msg.Body.Content
				}
				if err := fileHandler.SaveEmailBody(msg.Subject, msg.ID, cleanedBody); err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
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
				processedCount := atomic.LoadUint32(&stats.MessagesProcessed)
				if processedCount > 0 && processedCount%uint32(cfg.StateSaveInterval) == 0 {
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
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": job.Attachment.Name, "messageID": job.MessageID}).Warn("Skipping attachment: No download URL or content bytes.")
					<-semaphore
					continue
				}

				if err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": job.Attachment.Name, "messageID": job.MessageID, "error": err}).Error("Failed to save attachment.")
				} else {
					atomic.AddUint32(&stats.AttachmentsProcessed, 1)
				}
				<-semaphore
			}
		}()
	}

	producerWg.Wait()
	close(messagesChan)

	processorWg.Wait()
	close(attachmentsChan)

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

func runRouteMode(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor *emailprocessor.EmailProcessor, fileHandler *filehandler.FileHandler, accessToken, mailboxName string, stats *RunStats) {
	log.Info("Running in route mode.")

	processedFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.ProcessedFolder)
	if err != nil {
		log.Fatalf("Failed to get or create processed folder: %v", err)
	}
	errorFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.ErrorFolder)
	if err != nil {
		log.Fatalf("Failed to get or create error folder: %v", err)
	}

	state := &o365client.RunState{}
	messagesChan := make(chan o365client.Message, cfg.MaxParallelDownloads*2)
	var producerWg, workerWg sync.WaitGroup
	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	sourceFolderID := cfg.InboxFolder
	if strings.ToLower(sourceFolderID) != "inbox" {
		var err error
		sourceFolderID, err = o365Client.GetOrCreateFolderIDByName(ctx, mailboxName, cfg.InboxFolder)
		if err != nil {
			log.Fatalf("Failed to get or create source folder '%s': %v", cfg.InboxFolder, err)
		}
	}

	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		if err := o365Client.GetMessages(ctx, mailboxName, sourceFolderID, state, messagesChan); err != nil {
			log.Fatalf("O365 API error fetching messages for routing: %v", err)
		}
	}()

	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for msg := range messagesChan {
				semaphore <- struct{}{}
				var processingError bool

				log.WithFields(log.Fields{"messageID": msg.ID, "subject": msg.Subject}).Info("Routing message.")
				atomic.AddUint32(&stats.MessagesProcessed, 1)

				cleanedBody, err := emailProcessor.CleanHTML(msg.Body.Content)
				if err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Warn("Failed to clean HTML for message.")
					cleanedBody = msg.Body.Content
					processingError = true
				}
				if err := fileHandler.SaveEmailBody(msg.Subject, msg.ID, cleanedBody); err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": msg.ID, "error": err}).Errorf("Error saving email body.")
					processingError = true
				}

				if msg.HasAttachments {
					for _, att := range msg.Attachments {
						var err error
						if att.DownloadURL != "" {
							err = fileHandler.SaveAttachment(ctx, att.Name, msg.ID, att.DownloadURL, accessToken, att.Size)
						} else if att.ContentBytes != "" {
							err = fileHandler.SaveAttachmentFromBytes(att.Name, msg.ID, att.ContentBytes)
						} else {
							atomic.AddUint32(&stats.NonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID}).Warn("Skipping attachment: No download URL or content bytes.")
							continue
						}

						if err != nil {
							atomic.AddUint32(&stats.NonFatalErrors, 1)
							log.WithFields(log.Fields{"attachmentName": att.Name, "messageID": msg.ID, "error": err}).Error("Failed to save attachment.")
							processingError = true
						} else {
							atomic.AddUint32(&stats.AttachmentsProcessed, 1)
						}
					}
				}

				destinationID := processedFolderID
				if processingError {
					destinationID = errorFolderID
				}
				if err := o365Client.MoveMessage(ctx, mailboxName, msg.ID, destinationID); err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
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

func validateWorkspacePath(path string) error {
	if path == "" {
		return fmt.Errorf("workspace path cannot be empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace path must be an absolute path: %s", path)
	}

	criticalPaths := []string{"/", "/root", "/etc", "/bin", "/sbin", "/usr", "/var"}
	for _, p := range criticalPaths {
		if path == p {
			return fmt.Errorf("for safety, using critical system directory '%s' as a workspace is not allowed", path)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat workspace directory %s: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("workspace path %s exists but is not a directory", path)
	}

	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open workspace directory for checking emptiness: %w", err)
	}
	defer func() {
		if err := dir.Close(); err != nil {
			log.Warnf("Failed to close workspace directory handle: %v", err)
		}
	}()

	_, err = dir.Readdir(1)
	if err == nil {
		log.Warnf("Workspace directory %s is not empty. Files may be overwritten.", path)
	} else if err != io.EOF {
		return fmt.Errorf("failed to check if workspace directory is empty: %w", err)
	}

	return nil
}

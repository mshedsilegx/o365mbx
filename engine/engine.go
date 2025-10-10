package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"io"
	"o365mbx/apperrors"
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
	Attachment models.Attachmentable
	MessageID  string
	MsgPath    string
	Sequence   int
}

type RunStats struct {
	MessagesProcessed    uint32
	AttachmentsProcessed uint32
	NonFatalErrors       uint32
}

type ProcessingResult struct {
	MessageID        string
	Err              error
	IsInitialization bool // True if this result initializes the state for a message
	TotalTasks       int  // Set only on initialization
}

type MessageState struct {
	ExpectedTasks  int
	CompletedTasks int
	HasFailed      bool
}

type DownloadState struct {
	ExpectedAttachments  int
	CompletedAttachments int
	Attachments          []filehandler.AttachmentMetadata
	Mu                   sync.Mutex
}

func RunEngine(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, fileHandler filehandler.FileHandlerInterface, accessToken, version string) {
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

	if err := validateWorkspacePath(cfg.WorkspacePath); err != nil {
		log.Fatalf("Error validating workspacePath: %v", err)
	}

	fmt.Println("O365 Mailbox Downloader")
	log.Infof("Version: %s", version)
	log.Info("Application started.")

	if err := fileHandler.CreateWorkspace(); err != nil {
		log.Fatalf("Error creating workspace: %v", err)
	}
	log.WithField("path", cfg.WorkspacePath).Infof("Workspace created.")

	runDownloadMode(ctx, cfg, o365Client, emailProcessor, fileHandler, accessToken, stats)
}

func runDownloadMode(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, fileHandler filehandler.FileHandlerInterface, accessToken string, stats *RunStats) {
	var messageStates sync.Map

	defer func() {
		// Cleanup check for any unprocessed message states
		messageStates.Range(func(key, value interface{}) bool {
			messageID := key.(string)
			state := value.(*DownloadState)
			log.WithFields(log.Fields{
				"messageID":            messageID,
				"expectedAttachments":  state.ExpectedAttachments,
				"completedAttachments": state.CompletedAttachments,
			}).Warn("Shutdown with unprocessed message state, attachments may be incomplete.")
			return true // continue iterating
		})
	}()

	var state *o365client.RunState
	var err error
	if cfg.ProcessingMode == "incremental" {
		state, err = fileHandler.LoadState(cfg.StateFilePath)
		if err != nil {
			log.Fatalf("Error loading state file: %v", err)
		}
		if state.DeltaLink != "" {
			log.WithField("deltaLink", state.DeltaLink).Infof("Found previous state. Fetching changes since last run.")
		} else {
			log.Info("No previous state found. Starting full synchronization.")
		}
	} else {
		log.Info("Running in full or route mode. Fetching all available emails.")
		state = &o365client.RunState{}
	}

	messagesChan := make(chan models.Messageable, cfg.MaxParallelDownloads*2)
	attachmentsChan := make(chan AttachmentJob, cfg.MaxParallelDownloads*4)
	resultsChan := make(chan ProcessingResult, cfg.MaxParallelDownloads*4)

	var producerWg, processorWg, downloaderWg, aggregatorWg sync.WaitGroup

	if cfg.ProcessingMode == "route" {
		aggregatorWg.Add(1)
		go runAggregator(ctx, cfg, o365Client, resultsChan, &aggregatorWg)
	}

	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	sourceFolderID := cfg.InboxFolder
	if strings.ToLower(sourceFolderID) != "inbox" {
		var err error
		sourceFolderID, err = o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.InboxFolder)
		if err != nil {
			log.Fatalf("Failed to get or create source folder '%s': %v", cfg.InboxFolder, err)
		}
	}

	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		err := o365Client.GetMessages(ctx, cfg.MailboxName, sourceFolderID, state, messagesChan)
		if err != nil {
			if errors.Is(err, apperrors.ErrMissingDeltaLink) {
				log.Fatalf("Critical error during incremental sync: %v. This indicates a problem with the API or the local state. Please run a full sync to resolve.", err)
			}
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
				log.WithFields(log.Fields{"messageID": *msg.GetId(), "subject": *msg.GetSubject()}).Infof("Processing message.")

				processedBody, processingErr := emailProcessor.ProcessBody(*msg.GetBody().GetContent(), cfg.ConvertBody, cfg.ChromiumPath)
				effectiveConvertBody := cfg.ConvertBody
				if processingErr != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"messageID": *msg.GetId(), "error": processingErr}).Warn("Failed to process message body.")
					processedBody = *msg.GetBody().GetContent() // Fallback to original content
					if cfg.ConvertBody == "pdf" {
						effectiveConvertBody = "none" // Save with correct extension for the fallback content
					}
				}

				msgPath, saveErr := fileHandler.SaveMessage(msg, processedBody, effectiveConvertBody)
				if saveErr != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					finalErr := saveErr
					if processingErr != nil {
						finalErr = fmt.Errorf("body processing error: %w; and save error: %w", processingErr, saveErr)
					}
					log.WithFields(log.Fields{"messageID": *msg.GetId(), "error": finalErr}).Errorf("Error saving email message.")
					if cfg.ProcessingMode == "route" {
						resultsChan <- ProcessingResult{MessageID: *msg.GetId(), Err: finalErr, IsInitialization: true, TotalTasks: 1}
					}
					<-semaphore
					continue
				}

				// --- Attachment Handling (Two-Phase) ---
				if *msg.GetHasAttachments() {
					attachments, err := o365Client.GetMessageAttachments(ctx, cfg.MailboxName, *msg.GetId())
					if err != nil {
						atomic.AddUint32(&stats.NonFatalErrors, 1)
						log.WithFields(log.Fields{"messageID": *msg.GetId(), "error": err}).Error("Failed to fetch attachments for message.")
						if cfg.ProcessingMode == "route" {
							resultsChan <- ProcessingResult{MessageID: *msg.GetId(), Err: err, IsInitialization: true, TotalTasks: 1}
						}
						<-semaphore
						continue
					}

					if len(attachments) > 0 {
						log.WithFields(log.Fields{"count": len(attachments), "messageID": *msg.GetId()}).Infof("Found attachments.")
						messageStates.Store(*msg.GetId(), &DownloadState{
							ExpectedAttachments: len(attachments),
							Attachments:         make([]filehandler.AttachmentMetadata, 0, len(attachments)),
						})

						if cfg.ProcessingMode == "route" {
							totalTasks := 1 + len(attachments)
							resultsChan <- ProcessingResult{MessageID: *msg.GetId(), Err: processingErr, IsInitialization: true, TotalTasks: totalTasks}
						}

						for i, att := range attachments {
							attachmentsChan <- AttachmentJob{
								Attachment: att,
								MessageID:  *msg.GetId(),
								MsgPath:    msgPath,
								Sequence:   i + 1,
							}
						}
					} else { // hasAttachments was true, but API returned none.
						if cfg.ProcessingMode == "route" {
							resultsChan <- ProcessingResult{MessageID: *msg.GetId(), Err: processingErr, IsInitialization: true, TotalTasks: 1}
						}
					}
				} else { // No attachments.
					if cfg.ProcessingMode == "route" {
						resultsChan <- ProcessingResult{MessageID: *msg.GetId(), Err: processingErr, IsInitialization: true, TotalTasks: 1}
					}
				}

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
					"attachmentName": *job.Attachment.GetName(),
					"messageID":      job.MessageID,
				}).Debug("Processing attachment.")

				var attMetadata *filehandler.AttachmentMetadata
				var err error

				// Determine the attachment type and handle accordingly.
				switch att := job.Attachment.(type) {
				case *models.FileAttachment:
					// Skip inline attachments (e.g., signature images)
					if att.GetIsInline() != nil && *att.GetIsInline() {
						log.WithFields(log.Fields{"attachmentName": *att.GetName(), "messageID": job.MessageID}).Info("Skipping inline file attachment.")
					} else if att.GetContentBytes() != nil {
						// If contentBytes are present, it's a small attachment. Save it directly.
						log.WithFields(log.Fields{"attachmentName": *att.GetName(), "messageID": job.MessageID}).Debug("Saving file attachment from bytes.")
						attMetadata, err = fileHandler.SaveAttachmentFromBytes(job.MsgPath, att, job.Sequence)
					} else {
						// Otherwise, it's a large attachment that needs to be streamed.
						log.WithFields(log.Fields{"attachmentName": *att.GetName(), "messageID": job.MessageID}).Debug("Streaming large file attachment.")
						var contentStream io.ReadCloser
						contentStream, err = o365Client.GetAttachmentContent(ctx, cfg.MailboxName, job.MessageID, *att.GetId())
						if err == nil {
							defer contentStream.Close()
							attMetadata, err = fileHandler.SaveAttachmentFromStream(job.MsgPath, att, contentStream, job.Sequence)
						}
					}
				case *models.ItemAttachment:
					// Item attachments (like embedded emails) are streamed as MIME content.
					log.WithFields(log.Fields{"attachmentName": *att.GetName(), "messageID": job.MessageID}).Debug("Streaming item attachment (MIME).")
					var contentStream io.ReadCloser
					contentStream, err = o365Client.GetAttachmentContent(ctx, cfg.MailboxName, job.MessageID, *att.GetId())
					if err == nil {
						defer contentStream.Close()
						attMetadata, err = fileHandler.SaveAttachmentFromStream(job.MsgPath, att, contentStream, job.Sequence)
					}
				case *models.ReferenceAttachment:
					// Reference attachments are links to files (e.g., in OneDrive).
					// We will create a .url file containing the link.
					log.WithFields(log.Fields{"attachmentName": *att.GetName(), "messageID": job.MessageID}).Debug("Handling reference attachment.")
					attMetadata, err = fileHandler.SaveURLAttachment(job.MsgPath, att, job.Sequence)
					if err != nil {
						log.WithFields(log.Fields{"attachmentName": *att.GetName(), "error": err}).Error("Failed to save .url for reference attachment.")
					} else {
						log.WithFields(log.Fields{"attachmentName": *att.GetName()}).Info("Saved .url for reference attachment.")
					}
				default:
					err = fmt.Errorf("unhandled attachment type: %T", att)
				}

				if err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": *job.Attachment.GetName(), "messageID": job.MessageID, "error": err}).Error("Failed to save attachment.")
				} else if attMetadata != nil { // Only increment if metadata was successfully created
					atomic.AddUint32(&stats.AttachmentsProcessed, 1)
				}

				// Always update state to prevent memory leaks, even on failure.
				if rawState, ok := messageStates.Load(job.MessageID); ok {
					state := rawState.(*DownloadState)
					state.Mu.Lock()
					state.CompletedAttachments++
					if err == nil { // Only append metadata on success
						state.Attachments = append(state.Attachments, *attMetadata)
					}
					isLastAttachment := state.CompletedAttachments == state.ExpectedAttachments
					state.Mu.Unlock()

					if isLastAttachment {
						log.WithField("messageID", job.MessageID).Info("All attachments for message downloaded, writing final metadata.")
						metaErr := fileHandler.WriteAttachmentsToMetadata(job.MsgPath, state.Attachments)
						if metaErr != nil {
							atomic.AddUint32(&stats.NonFatalErrors, 1)
							log.WithFields(log.Fields{"messageID": job.MessageID, "error": metaErr}).Error("Failed to write final metadata.")
							err = metaErr // The metadata error takes precedence for the final report.
						}
						// Clean up state for the message
						messageStates.Delete(job.MessageID)
					}
				}

				if cfg.ProcessingMode == "route" {
					resultsChan <- ProcessingResult{MessageID: job.MessageID, Err: err}
				}
				<-semaphore
			}
		}()
	}

	producerWg.Wait()

	processorWg.Wait()
	close(attachmentsChan)

	downloaderWg.Wait()

	if cfg.ProcessingMode == "route" {
		close(resultsChan)
		aggregatorWg.Wait()
	}

	if cfg.ProcessingMode == "incremental" && state.DeltaLink != "" {
		log.WithField("deltaLink", state.DeltaLink).Info("Saving new state with delta link.")
		if err := fileHandler.SaveState(state, cfg.StateFilePath); err != nil {
			log.Errorf("Error saving state file: %v", err)
		}
	}
}

func runAggregator(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, resultsChan <-chan ProcessingResult, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Info("Aggregator started.")

	messageStates := make(map[string]*MessageState)

	processedFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.ProcessedFolder)
	if err != nil {
		log.Fatalf("Aggregator failed to get or create processed folder: %v", err)
	}
	errorFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.ErrorFolder)
	if err != nil {
		log.Fatalf("Aggregator failed to get or create error folder: %v", err)
	}

	for result := range resultsChan {
		state, exists := messageStates[result.MessageID]
		if !exists {
			if !result.IsInitialization {
				log.Errorf("Aggregator received non-initialization result for unknown message ID: %s. This should not happen.", result.MessageID)
				continue
			}
			state = &MessageState{ExpectedTasks: result.TotalTasks}
			messageStates[result.MessageID] = state
		}

		state.CompletedTasks++
		if result.Err != nil {
			state.HasFailed = true
		}

		if state.CompletedTasks >= state.ExpectedTasks {
			destinationID := processedFolderID
			if state.HasFailed {
				destinationID = errorFolderID
			}

			log.WithFields(log.Fields{"messageID": result.MessageID, "hasFailed": state.HasFailed}).Info("Message processing complete. Moving message.")
			if err := o365Client.MoveMessage(ctx, cfg.MailboxName, result.MessageID, destinationID); err != nil {
				log.WithFields(log.Fields{"messageID": result.MessageID, "error": err}).Errorf("Failed to move message.")
			} else {
				log.WithField("messageID", result.MessageID).Infof("Successfully moved message.")
			}
			delete(messageStates, result.MessageID)
		}
	}
	log.Info("Aggregator finished.")
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

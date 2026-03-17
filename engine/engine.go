// Package engine implements the core business logic and orchestrates the parallelized
// email download and processing pipeline.
//
// OBJECTIVE:
// The engine is the "brain" of the application. It manages the lifecycle of a download run,
// coordinating between the O365 API, the email processor, and the file system.
// It uses a multi-stage producer-consumer pipeline with worker pools to achieve high throughput
// while respecting API rate limits and memory constraints.
//
// CORE FUNCTIONALITY:
//  1. Pipeline Orchestration: Coordinates Message Producers, Body Processors, and Attachment Downloaders.
//  2. State Management: Tracks the progress of individual messages and their attachments.
//  3. Parallelism: Implements worker pools with semaphores to control concurrency and bandwidth.
//  4. Reliability: Handles retries, timeouts, and graceful shutdowns.
//  5. Aggregation: In "route" mode, it tracks the completion of all sub-tasks for a message
//     before moving it to a destination folder (Success or Error) in the mailbox.
//
// DATA FLOW:
//
//	MessagesChan -> Message Workers -> AttachmentsChan -> Downloader Workers -> ResultsChan -> Aggregator
package engine

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/microsoftgraph/msgraph-sdk-go/models"

	"o365mbx/utils"

	log "github.com/sirupsen/logrus"
)

// AttachmentJob represents a task for the downloader pool to process a single attachment.
type AttachmentJob struct {
	Ctx        context.Context
	Attachment models.Attachmentable
	MessageID  string
	MsgPath    string
	Sequence   int
}

type RunStats struct {
	MessagesProcessed    uint32
	AttachmentsProcessed uint32
	NonFatalErrors       uint32
	JobProcessedCount    uint32
	JobErrorCount        uint32
}

// ProcessingResult encapsulates the outcome of a processing task (body or attachment)
// for aggregation and state tracking.
type ProcessingResult struct {
	MessageID        string
	Err              error
	IsInitialization bool // True if this result initializes the state for a message
	TotalTasks       int  // Set only on initialization
	MsgPath          string
	Cancel           context.CancelFunc // Used by aggregator to release resources early
}

// MessageState tracks the processing progress of a single email message
// and provides a way to cancel its associated tasks.
type MessageState struct {
	ExpectedTasks  int
	CompletedTasks int
	HasFailed      bool
	Cancel         context.CancelFunc
}

// DownloadState manages the collection of attachment metadata for a single message
// during the two-phase download process.
type DownloadState struct {
	ExpectedAttachments  int
	CompletedAttachments int
	Attachments          []filehandler.AttachmentMetadata
	Mu                   sync.Mutex
}

// --- Entry Point ---

// RunEngine is the primary entry point for the core processing logic.
func RunEngine(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, fileHandler filehandler.FileHandlerInterface, version string) error {
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
		return fmt.Errorf("error validating workspacePath: %w", err)
	}

	fmt.Println("O365 Mailbox Downloader")
	log.Infof("Version: %s", version)
	log.Info("Application started.")

	if err := fileHandler.CreateWorkspace(); err != nil {
		return fmt.Errorf("error creating workspace: %w", err)
	}
	log.WithField("path", cfg.WorkspacePath).Infof("Workspace created.")

	// Capture initial mailbox counts
	sourceCounts, err := o365Client.GetMailboxStats(ctx, cfg.MailboxName)
	if err != nil {
		log.Warnf("Failed to capture initial mailbox counts: %v", err)
		// We continue anyway, but sourceCounts will be empty or partial
	}

	err = runDownloadMode(ctx, cfg, o365Client, emailProcessor, fileHandler, stats)

	// Save final status report
	if reportErr := fileHandler.SaveStatusReport(cfg.MailboxName, sourceCounts, int(atomic.LoadUint32(&stats.JobProcessedCount)), int(atomic.LoadUint32(&stats.JobErrorCount))); reportErr != nil {
		log.Errorf("Failed to save status report: %v", reportErr)
	}

	return err
}

// --- Pipeline Orchestration ---

// runDownloadMode manages the multi-stage producer-consumer pipeline for messages and attachments.
//
// PIPELINE STAGES:
// 1. Message Producer: Fetches message metadata from O365 (supports delta queries).
// 2. Message Processors: Sanitizes HTML and triggers attachment discovery.
// 3. Attachment Downloaders: Downloads binary content in parallel with bandwidth limiting.
// 4. Aggregator: Coordinates multi-step message completion and relocation.
func runDownloadMode(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, fileHandler filehandler.FileHandlerInterface, stats *RunStats) error {
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
			return fmt.Errorf("error loading state file: %w", err)
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

	// 1. Initialize Channels for the Producer-Consumer Pipeline
	messagesChan := make(chan models.Messageable, cfg.MaxParallelDownloads*2)
	attachmentsChan := make(chan AttachmentJob, cfg.MaxParallelDownloads*4)
	resultsChan := make(chan ProcessingResult, cfg.MaxParallelDownloads*4)

	var producerWg, processorWg, downloaderWg, aggregatorWg sync.WaitGroup
	var globalErr error
	var errOnce sync.Once

	setGlobalErr := func(err error) {
		errOnce.Do(func() {
			globalErr = err
		})
	}

	// 2. Start Aggregator (Route Mode only) to track message completion
	if cfg.ProcessingMode == "route" {
		aggregatorWg.Add(1)
		go func() {
			if err := runAggregator(ctx, cfg, o365Client, fileHandler, resultsChan, stats, &aggregatorWg); err != nil {
				setGlobalErr(err)
			}
		}()
	}

	semaphore := make(chan struct{}, cfg.MaxParallelDownloads)

	sourceFolderID := cfg.InboxFolder
	if strings.ToLower(sourceFolderID) != "inbox" {
		var err error
		sourceFolderID, err = o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.InboxFolder)
		if err != nil {
			return fmt.Errorf("failed to get or create source folder '%s': %w", cfg.InboxFolder, err)
		}
	}

	// 3. Start Message Producer Goroutine
	producerWg.Add(1)
	go func() {
		defer producerWg.Done()
		err := o365Client.GetMessages(ctx, cfg.MailboxName, sourceFolderID, state, messagesChan)
		if err != nil {
			if errors.Is(err, apperrors.ErrMissingDeltaLink) {
				setGlobalErr(fmt.Errorf("critical error during incremental sync: %w", err))
			} else {
				setGlobalErr(fmt.Errorf("O365 API error fetching messages: %w", err))
			}
		}
	}()

	// 4. Start Message Processor Workers
	for i := 0; i < cfg.MaxParallelDownloads; i++ {
		processorWg.Add(1)
		go func() {
			defer processorWg.Done()
			for msg := range messagesChan {
				// Create a per-message context with timeout
				msgCtx, msgCancel := context.WithTimeout(ctx, time.Duration(cfg.MaxExecutionTimeMsg)*time.Second)

				semaphore <- struct{}{}

				// Helper function to ensure semaphore and context are handled correctly
				func() {
					defer func() { <-semaphore }()
					// If we are not in route mode, we cancel here.
					// If in route mode, aggregator will cancel.
					if cfg.ProcessingMode != "route" {
						defer msgCancel()
					}

					messageID := utils.StringValue(msg.GetId(), "unknown")
					subject := utils.StringValue(msg.GetSubject(), "(no subject)")
					atomic.AddUint32(&stats.MessagesProcessed, 1)
					log.WithFields(log.Fields{"messageID": messageID}).Infof("Processing message.")
					log.WithFields(log.Fields{"messageID": messageID, "subject": subject}).Debugf("Processing message details.")

					var bodyContent string
					if msg.GetBody() != nil {
						bodyContent = utils.StringValue(msg.GetBody().GetContent(), "")
					}

					processedBody, processingErr := emailProcessor.ProcessBody(msgCtx, bodyContent, cfg.ConvertBody)
					effectiveConvertBody := cfg.ConvertBody
					if processingErr != nil {
						atomic.AddUint32(&stats.NonFatalErrors, 1)
						log.WithFields(log.Fields{"messageID": messageID, "error": processingErr}).Warn("Failed to process message body.")
						processedBody = bodyContent // Fallback to original content
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
						log.WithFields(log.Fields{"messageID": messageID, "error": finalErr}).Errorf("Error saving email message.")
						if cfg.ProcessingMode == "route" {
							resultsChan <- ProcessingResult{MessageID: messageID, Err: finalErr, IsInitialization: true, TotalTasks: 1, MsgPath: msgPath, Cancel: msgCancel}
						}
						return
					}

					// --- Attachment Handling (Two-Phase) ---
					hasAttachments := utils.BoolValue(msg.GetHasAttachments(), false)
					var attachments []models.Attachmentable
					var attachmentFetchErr error

					if hasAttachments {
						attachments, attachmentFetchErr = o365Client.GetMessageAttachments(msgCtx, cfg.MailboxName, messageID)
						if attachmentFetchErr != nil {
							atomic.AddUint32(&stats.NonFatalErrors, 1)
							log.WithFields(log.Fields{"messageID": messageID, "error": attachmentFetchErr}).Error("Failed to fetch attachments for message.")
							if cfg.ProcessingMode == "route" {
								resultsChan <- ProcessingResult{MessageID: messageID, Err: attachmentFetchErr, IsInitialization: true, TotalTasks: 1, MsgPath: msgPath, Cancel: msgCancel}
							}
							return
						}
					}

					if hasAttachments && len(attachments) > 0 {
						log.WithFields(log.Fields{"count": len(attachments), "messageID": messageID}).Infof("Found attachments.")
						messageStates.Store(messageID, &DownloadState{
							ExpectedAttachments: len(attachments),
							Attachments:         make([]filehandler.AttachmentMetadata, 0, len(attachments)),
						})

						if cfg.ProcessingMode == "route" {
							totalTasks := 1 + len(attachments)
							resultsChan <- ProcessingResult{MessageID: messageID, Err: processingErr, IsInitialization: true, TotalTasks: totalTasks, MsgPath: msgPath, Cancel: msgCancel}
						}

						for i, att := range attachments {
							select {
							case <-msgCtx.Done():
								log.WithField("messageID", messageID).Warn("Message timeout while queuing attachments.")
								if cfg.ProcessingMode == "route" {
									// Report remaining attachments as errors so aggregator doesn't hang
									for j := i; j < len(attachments); j++ {
										resultsChan <- ProcessingResult{MessageID: messageID, Err: msgCtx.Err(), MsgPath: msgPath}
									}
								}
								return
							case attachmentsChan <- AttachmentJob{
								Ctx:        msgCtx,
								Attachment: att,
								MessageID:  messageID,
								MsgPath:    msgPath,
								Sequence:   i + 1,
							}:
							}
						}
					} else if cfg.ProcessingMode == "route" {
						// No attachments, but we still need to report result if in route mode
						resultsChan <- ProcessingResult{MessageID: messageID, Err: processingErr, IsInitialization: true, TotalTasks: 1, MsgPath: msgPath, Cancel: msgCancel}
					}
				}()
			}
		}()
	}

	// 5. Start Attachment Downloader Workers
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

				var attMetadatas []filehandler.AttachmentMetadata
				var err error

				attMetadatas, err = fileHandler.SaveAttachmentFromBytes(job.Ctx, cfg.MailboxName, job.MessageID, job.MsgPath, job.Attachment, job.Sequence)

				if err != nil {
					atomic.AddUint32(&stats.NonFatalErrors, 1)
					log.WithFields(log.Fields{"attachmentName": *job.Attachment.GetName(), "messageID": job.MessageID, "error": err}).Error("Failed to save attachment.")
				} else {
					atomic.AddUint32(&stats.AttachmentsProcessed, 1)
				}

				// Always update state to prevent memory leaks, even on failure.
				if rawState, ok := messageStates.Load(job.MessageID); ok {
					state := rawState.(*DownloadState)
					state.Mu.Lock()
					state.CompletedAttachments++
					if err == nil { // Only append metadata on success
						state.Attachments = append(state.Attachments, attMetadatas...)
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
					resultsChan <- ProcessingResult{MessageID: job.MessageID, Err: err, MsgPath: job.MsgPath}
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

	if cfg.ProcessingMode == "incremental" && state.DeltaLink != "" && globalErr == nil {
		log.WithField("deltaLink", state.DeltaLink).Info("Saving new state with delta link.")
		if err := fileHandler.SaveState(state, cfg.StateFilePath); err != nil {
			log.Errorf("Error saving state file: %v", err)
		}
	}

	return globalErr
}

// --- Completion and Reconciliation ---

// runAggregator tracks the completion of all tasks for a message and moves it in O365.
func runAggregator(ctx context.Context, cfg *Config, o365Client o365client.O365ClientInterface, fileHandler filehandler.FileHandlerInterface, resultsChan <-chan ProcessingResult, stats *RunStats, wg *sync.WaitGroup) error {
	defer wg.Done()
	log.Info("Aggregator started.")

	messageStates := make(map[string]*MessageState)
	messagePaths := make(map[string]string)
	messageErrors := make(map[string][]error)

	processedFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.ProcessedFolder)
	if err != nil {
		return fmt.Errorf("aggregator failed to get or create processed folder: %w", err)
	}
	errorFolderID, err := o365Client.GetOrCreateFolderIDByName(ctx, cfg.MailboxName, cfg.ErrorFolder)
	if err != nil {
		return fmt.Errorf("aggregator failed to get or create error folder: %w", err)
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
			messagePaths[result.MessageID] = result.MsgPath
		}

		state.CompletedTasks++
		if result.Err != nil {
			state.HasFailed = true
			messageErrors[result.MessageID] = append(messageErrors[result.MessageID], result.Err)
		}

		// Update state with the Cancel function if provided
		if result.Cancel != nil {
			state.Cancel = result.Cancel
		}

		if state.CompletedTasks >= state.ExpectedTasks {
			// Trigger early cancellation now that all tasks are complete
			if state.Cancel != nil {
				state.Cancel()
			}

			destinationID := processedFolderID
			if state.HasFailed {
				destinationID = errorFolderID
				atomic.AddUint32(&stats.JobErrorCount, 1)

				// Create error.json in message folder
				if msgPath, ok := messagePaths[result.MessageID]; ok {
					if err := fileHandler.SaveError(msgPath, messageErrors[result.MessageID]); err != nil {
						log.WithFields(log.Fields{"messageID": result.MessageID, "error": err}).Errorf("Failed to save error.json")
					}
				}
			} else {
				atomic.AddUint32(&stats.JobProcessedCount, 1)
			}

			log.WithFields(log.Fields{"messageID": result.MessageID, "hasFailed": state.HasFailed}).Info("Message processing complete. Moving message.")
			if err := o365Client.MoveMessage(ctx, cfg.MailboxName, result.MessageID, destinationID); err != nil {
				log.WithFields(log.Fields{"messageID": result.MessageID, "error": err}).Errorf("Failed to move message.")
			} else {
				log.WithField("messageID", result.MessageID).Infof("Successfully moved message.")
			}
			delete(messageStates, result.MessageID)
			delete(messagePaths, result.MessageID)
			delete(messageErrors, result.MessageID)
		}
	}
	log.Info("Aggregator finished.")
	return nil
}

// validateWorkspacePath ensures the provided path is absolute, not a critical system
// directory, and is a valid writable directory.
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

	// #nosec G304 - path is validated by validateWorkspacePath before opening.
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

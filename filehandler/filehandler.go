// Package filehandler manages all local file system operations, including
// workspace creation, message saving, attachment handling, and state persistence.
//
// OBJECTIVE:
// This package is responsible for the "L" (Load/Store) in the ETL pipeline. It ensures
// that all downloaded data (metadata, bodies, and attachments) is stored safely and
// consistently on the local filesystem, adhering to security best practices and
// OS-specific constraints like path length limits.
//
// CORE FUNCTIONALITY:
//  1. Workspace Management: Creates and validates the root directory for all job artifacts.
//  2. Message Serialization: Saves email metadata as JSON and bodies in various formats (.txt, .html, .pdf).
//  3. Attachment Handling: Manages two-phase attachment downloads, including large file
//     chunking and MIME extraction for .msg/.eml files.
//  4. State & Reporting: Persists incremental run states and generates detailed JSON status
//     reports for job reconciliation.
//  5. Concurrency Control: Uses file-level mutexes and bandwidth limiters to ensure
//     thread-safe operations and respect environment constraints.
//
// SECURITY MEASURES:
// - Absolute Path Enforcement: Rejects relative paths for workspace.
// - Symlink Protection: Mitigates TOCTOU by verifying directories aren't symbolic links.
// - Filename Sanitization: Prevents path traversal and illegal characters in OS-neutral filenames.
package filehandler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jhillyerd/enmime"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"

	"o365mbx/apperrors"
	"o365mbx/emailprocessor"
	"o365mbx/o365client"
	"o365mbx/utils"
)

const maxPathLength = 512 // A safe limit for total path length to avoid filesystem errors.

// --- Workspace and Path Management ---

// isPathTooLong checks if a given path exceeds the maximum allowed length.
func isPathTooLong(path string) bool {
	return len(path) > maxPathLength
}

// AttachmentMetadata holds information about a saved attachment for inclusion in metadata.json.
// This struct is used both for the final metadata file and for internal state tracking
// during the download process.
type AttachmentMetadata struct {
	Name        string `json:"attachment_name_in_message"`
	ContentType string `json:"content_type_of_attachment"`
	Size        int64  `json:"size_of_attachment_in_bytes"`
	SavedAs     string `json:"attachment_name_stored_after_download"`
}

// Recipient mirrors the structure of models.Recipient for JSON serialization.
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

// EmailAddress mirrors the structure of models.EmailAddress for JSON serialization.
type EmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Metadata holds all metadata for a given email message.
// Metadata represents the structured JSON format for an email's metadata.json file.
type Metadata struct {
	To              []Recipient          `json:"to"`
	Cc              []Recipient          `json:"cc"`
	From            Recipient            `json:"from"`
	Subject         string               `json:"subject"`
	ReceivedDate    time.Time            `json:"received_date"`
	BodyFile        string               `json:"body"`
	BodyContentType string               `json:"content_type_of_body"`
	AttachmentCount int                  `json:"attachment_counts"`
	Attachments     []AttachmentMetadata `json:"list_of_attachments"`
}

// FileHandlerInterface defines the interface for FileHandler methods used by other packages.
//
//go:generate mockgen -destination=../mocks/mock_filehandler.go -package=mocks o365mbx/filehandler FileHandlerInterface
type FileHandlerInterface interface {
	CreateWorkspace() error
	SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error)
	SaveAttachmentFromBytes(ctx context.Context, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error)
	WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error
	SaveState(state *o365client.RunState, stateFilePath string) error
	LoadState(stateFilePath string) (*o365client.RunState, error)
	SaveError(msgPath string, errs []error) error
	SaveStatusReport(mailboxName string, sourceCounts map[string]int32, processedCount, errorCount int) error
}

// ErrorDetail represents a single error entry in error.json
type ErrorDetail struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// JobStatus represents the structure of status_<timestamp>.json
type JobStatus struct {
	Mailbox           string           `json:"mailbox"`
	Timestamp         string           `json:"timestamp"`
	SourceCounts      map[string]int32 `json:"source_mailbox_counts"`
	JobProcessedCount int              `json:"job_processed_count"`
	JobErrorCount     int              `json:"job_error_count"`
}

// FileHandler handles the disk-based storage and retrieval logic for the application.
type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	emailProcessor           emailprocessor.EmailProcessorInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	bandwidthLimiter         *rate.Limiter
	fileMutexes              sync.Map // Map of string (filePath) to *sync.Mutex
	msgHandler               string
	attachmentExtractionL1   string
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, largeAttachmentThresholdMB, chunkSizeMB int, bandwidthLimitMBs float64, msgHandler string, attachmentExtractionL1 string, logger log.FieldLogger) *FileHandler {
	var limiter *rate.Limiter
	if bandwidthLimitMBs > 0 {
		limit := rate.Limit(bandwidthLimitMBs * 1024 * 1024)
		limiter = rate.NewLimiter(limit, int(limit)) // Burst equal to the limit
		logger.WithField("limit", limit).Info("Bandwidth limiting enabled.")
	}

	return &FileHandler{
		workspacePath:            workspacePath,
		o365Client:               o365Client,
		emailProcessor:           emailProcessor,
		largeAttachmentThreshold: largeAttachmentThresholdMB * 1024 * 1024,
		chunkSize:                chunkSizeMB * 1024 * 1024,
		bandwidthLimiter:         limiter,
		msgHandler:               msgHandler,
		attachmentExtractionL1:   attachmentExtractionL1,
	}
}

// getMutex returns a mutex for a given file path, creating it if it doesn't exist.
func (fh *FileHandler) getMutex(filePath string) *sync.Mutex {
	if mu, ok := fh.fileMutexes.Load(filePath); ok {
		return mu.(*sync.Mutex)
	}

	mu := &sync.Mutex{}
	actual, loaded := fh.fileMutexes.LoadOrStore(filePath, mu)
	if loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

// CreateWorkspace initializes the root directory for the current job.
// It uses os.MkdirAll for idempotency and performs security checks to ensure
// the path is a real directory and not a symbolic link (mitigating TOCTOU).
func (fh *FileHandler) CreateWorkspace() error {
	err := os.MkdirAll(fh.workspacePath, 0700)
	if err != nil {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "failed to create workspace directory", Err: err}
	}

	// TOCTOU mitigation: After creating, verify it's a directory and not a symlink.
	info, err := os.Lstat(fh.workspacePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "failed to stat workspace after creation", Err: err}
	}
	if !info.IsDir() {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "workspace path is not a directory", Err: nil}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "for security, workspace cannot be a symbolic link", Err: nil}
	}

	return nil
}

func toRecipients(mRecipients []models.Recipientable) []Recipient {
	recipients := make([]Recipient, len(mRecipients))
	for i, r := range mRecipients {
		recipients[i] = toRecipient(r)
	}
	return recipients
}

func toRecipient(mRecipient models.Recipientable) Recipient {
	if mRecipient == nil || mRecipient.GetEmailAddress() == nil {
		return Recipient{}
	}
	return Recipient{
		EmailAddress: EmailAddress{
			Name:    utils.StringValue(mRecipient.GetEmailAddress().GetName(), ""),
			Address: utils.StringValue(mRecipient.GetEmailAddress().GetAddress(), ""),
		},
	}
}

// --- Message and Body Storage ---

// SaveMessage creates a unique directory for an email message and persists its
// body and metadata. It returns the absolute path to the message directory.
func (fh *FileHandler) SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error) {
	messageID := utils.StringValue(message.GetId(), "unknown")
	msgPath := filepath.Join(fh.workspacePath, messageID)
	if isPathTooLong(msgPath) {
		return "", &apperrors.FileSystemError{Path: msgPath, Msg: "generated message path exceeds maximum length", Err: nil}
	}

	if err := os.MkdirAll(msgPath, 0700); err != nil {
		return "", &apperrors.FileSystemError{Path: msgPath, Msg: "failed to create message directory", Err: err}
	}

	// Use os.OpenRoot (Go 1.24+) for secure file operations within the message directory
	root, err := os.OpenRoot(msgPath)
	if err != nil {
		return "", &apperrors.FileSystemError{Path: msgPath, Msg: "failed to open message directory root", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	if err := root.Mkdir("attachments", 0700); err != nil && !os.IsExist(err) {
		return "", &apperrors.FileSystemError{Path: filepath.Join(msgPath, "attachments"), Msg: "failed to create attachments directory", Err: err}
	}

	var bodyExt, bodyContentType string
	switch convertBody {
	case "text":
		bodyExt = ".txt"
		bodyContentType = "text/plain"
	case "pdf":
		bodyExt = ".pdf"
		bodyContentType = "application/pdf"
	case "none":
		if contentStr, ok := bodyContent.(string); ok && fh.emailProcessor.IsHTML(contentStr) {
			bodyExt = ".html"
			bodyContentType = "text/html"
		} else {
			bodyExt = ".txt"
			bodyContentType = "text/plain"
		}
	default:
		bodyExt = ".txt"
		bodyContentType = "text/plain"
	}
	bodyFileName := "body" + bodyExt

	// Save the body using OpenRoot
	var bodyData []byte
	switch content := bodyContent.(type) {
	case string:
		bodyData = []byte(content)
	case []byte:
		bodyData = content
	default:
		return "", fmt.Errorf("unsupported body content type: %T", bodyContent)
	}

	if err := root.WriteFile(bodyFileName, bodyData, 0600); err != nil {
		return "", &apperrors.FileSystemError{Path: filepath.Join(msgPath, bodyFileName), Msg: "failed to save email body", Err: err}
	}

	// Create and save metadata using OpenRoot
	metadata := Metadata{
		To:              toRecipients(message.GetToRecipients()),
		Cc:              toRecipients(message.GetCcRecipients()),
		From:            toRecipient(message.GetFrom()),
		Subject:         utils.StringValue(message.GetSubject(), "(no subject)"),
		ReceivedDate:    utils.TimeValue(message.GetReceivedDateTime(), time.Time{}),
		BodyFile:        bodyFileName,
		BodyContentType: bodyContentType,
		AttachmentCount: len(message.GetAttachments()),
		Attachments:     []AttachmentMetadata{},
	}

	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to encode metadata to JSON: %w", err)
	}

	if err := root.WriteFile("metadata.json", metadataJSON, 0600); err != nil {
		return "", &apperrors.FileSystemError{Path: filepath.Join(msgPath, "metadata.json"), Msg: "failed to save metadata file", Err: err}
	}

	return msgPath, nil
}

// --- Attachment Storage ---

// SaveAttachmentFromBytes saves an attachment (File or Item) and returns a list of metadata for it and any nested attachments.
// It uses os.OpenRoot for secure workspace access.
func (fh *FileHandler) SaveAttachmentFromBytes(ctx context.Context, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error) {
	root, err := os.OpenRoot(msgPath)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: msgPath, Msg: "failed to open message directory root for saving attachment", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	if _, ok := att.(*models.FileAttachment); ok {
		meta, err := fh.saveFileAttachment(root, att, sequence)
		if err != nil {
			return nil, err
		}
		return []AttachmentMetadata{*meta}, nil
	}

	if _, ok := att.(*models.ItemAttachment); ok {
		return fh.SaveItemAttachment(ctx, root, mailboxName, messageID, msgPath, att, sequence)
	}

	return nil, fmt.Errorf("unsupported attachment type: %T", att)
}

// saveFileAttachment saves a standard file attachment using os.OpenRoot for security.
func (fh *FileHandler) saveFileAttachment(root *os.Root, att models.Attachmentable, sequence int) (*AttachmentMetadata, error) {
	fileAttachment, _ := att.(*models.FileAttachment)
	fileName := SanitizeFileName(utils.StringValue(fileAttachment.GetName(), "unnamed"))
	savedAs := fmt.Sprintf("%02d_%s", sequence, fileName)

	contentBytes := fileAttachment.GetContentBytes()
	if contentBytes == nil {
		return nil, fmt.Errorf("attachment %s has no content bytes", utils.StringValue(fileAttachment.GetName(), "unnamed"))
	}

	// attachmentsDir is expected to exist (created in SaveMessage)
	if err := root.WriteFile(filepath.Join("attachments", savedAs), contentBytes, 0600); err != nil {
		return nil, &apperrors.FileSystemError{Path: savedAs, Msg: "failed to save file attachment", Err: err}
	}

	log.WithField("attachmentName", utils.StringValue(fileAttachment.GetName(), "unnamed")).Infof("Successfully saved file attachment.")
	return &AttachmentMetadata{
		Name:        utils.StringValue(fileAttachment.GetName(), "unnamed"),
		ContentType: utils.StringValue(fileAttachment.GetContentType(), "application/octet-stream"),
		Size:        int64(utils.Int32Value(fileAttachment.GetSize(), 0)),
		SavedAs:     savedAs,
	}, nil
}

// SaveItemAttachment downloads an ItemAttachment (e.g., a forwarded .msg or .eml file)
// as a raw MIME stream, saves it to disk, and optionally performs a level-1 extraction
// of its contents (body and attachments) if 'extractor' mode is enabled.
// It uses os.OpenRoot for secure file operations.
func (fh *FileHandler) SaveItemAttachment(ctx context.Context, root *os.Root, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error) {
	attachmentID := *att.GetId()
	attachmentName := utils.StringValue(att.GetName(), "unnamed")

	rawMimeStream, err := fh.o365Client.GetAttachmentRawStream(ctx, mailboxName, messageID, attachmentID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch raw stream for item attachment %s: %w", attachmentName, err)
	}
	defer func() {
		if closeErr := rawMimeStream.Close(); closeErr != nil {
			log.Warnf("Failed to close raw MIME stream: %v", closeErr)
		}
	}()

	// 1. Save the original item attachment (the .eml itself)
	saveName := SanitizeFileName(attachmentName)
	if !strings.Contains(saveName, ".") {
		saveName += ".eml"
	}

	// Let's stick to original naming pattern
	fileName := fmt.Sprintf("%02d_%s", sequence, saveName)
	filePathInAtt := filepath.Join("attachments", fileName)

	file, err := root.Create(filePathInAtt)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: filepath.Join(msgPath, filePathInAtt), Msg: "failed to create item attachment file", Err: err}
	}

	// Use a context-aware copy to prevent hangs during streaming
	err = copyWithContext(ctx, file, rawMimeStream)
	if closeErr := file.Close(); closeErr != nil {
		log.Warnf("Failed to close attachment file %s: %v", fileName, closeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stream item attachment to disk: %w", err)
	}

	// Get size from root
	fileInfo, err := root.Stat(filePathInAtt)
	var size int64
	if err == nil {
		size = fileInfo.Size()
	}

	allMetadata := []AttachmentMetadata{
		{
			Name:        attachmentName,
			ContentType: utils.StringValue(att.GetContentType(), "message/rfc822"),
			Size:        size,
			SavedAs:     fileName,
		},
	}

	// 2. If extractor mode, parse and extract body + 1 level of attachments
	if fh.msgHandler == "extractor" {
		savedFile, err := root.Open(filePathInAtt)
		if err != nil {
			log.WithFields(log.Fields{"path": fileName, "error": err}).Warn("Failed to re-open attachment for extraction.")
			return allMetadata, nil
		}
		defer func() {
			if closeErr := savedFile.Close(); closeErr != nil {
				log.Warnf("Failed to close saved file %s: %v", fileName, closeErr)
			}
		}()

		env, err := enmime.ReadEnvelope(savedFile)
		if err != nil {
			log.WithFields(log.Fields{"attachmentName": attachmentName, "error": err}).Warn("Failed to parse MIME content, skipping extraction.")
			return allMetadata, nil
		}

		// Save Body
		bodyExt := ".html"
		bodyContent := env.HTML
		if bodyContent == "" {
			bodyContent = env.Text
			bodyExt = ".txt"
		}

		if bodyContent != "" {
			bodyName := fmt.Sprintf("%02d_%s_extracted%s", sequence, SanitizeFileName(attachmentName), bodyExt)
			if err := root.WriteFile(filepath.Join("attachments", bodyName), []byte(bodyContent), 0600); err == nil {
				allMetadata = append(allMetadata, AttachmentMetadata{
					Name:        attachmentName + " (body)",
					ContentType: "text/html",
					Size:        int64(len(bodyContent)),
					SavedAs:     bodyName,
				})
			}
		}

		// Extract 1 level of nested attachments
		nestedMetas := fh.extractFilesFromEnvelope(root, env, sequence)
		allMetadata = append(allMetadata, nestedMetas...)
	}

	return allMetadata, nil
}

// extractFilesFromEnvelope handles one level of extraction from a parsed MIME envelope.
func (fh *FileHandler) extractFilesFromEnvelope(root *os.Root, env *enmime.Envelope, parentSequence int) []AttachmentMetadata {
	if fh.msgHandler == "raw" {
		return nil
	}

	var metadatas []AttachmentMetadata

	allParts := env.Attachments
	if fh.attachmentExtractionL1 == "inlines" {
		allParts = append(allParts, env.Inlines...)
	}

	for i, part := range allParts {
		nestedCount := i + 1
		fileName := part.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("attachment_%d", nestedCount)
		}

		safeName := fmt.Sprintf("%02d_%d_%s", parentSequence, nestedCount, SanitizeFileName(fileName))
		filePathInAtt := filepath.Join("attachments", safeName)

		log.WithField("nestedName", fileName).Infof("-> Extracting: %s", safeName)

		if err := root.WriteFile(filePathInAtt, part.Content, 0600); err != nil {
			log.WithFields(log.Fields{"fileName": fileName, "error": err}).Error("Failed to save nested attachment.")
			continue
		}

		metadatas = append(metadatas, AttachmentMetadata{
			Name:        fileName,
			ContentType: part.ContentType,
			Size:        int64(len(part.Content)),
			SavedAs:     safeName,
		})
	}

	return metadatas
}

// WriteAttachmentsToMetadata writes the final list of attachments to the metadata.json file.
// It uses os.OpenRoot for secure workspace access.
func (fh *FileHandler) WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error {
	mu := fh.getMutex(filepath.Join(msgPath, "metadata.json"))
	mu.Lock()
	defer mu.Unlock()

	root, err := os.OpenRoot(msgPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: msgPath, Msg: "failed to open message directory root for metadata update", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	data, err := root.ReadFile("metadata.json")
	if err != nil {
		return &apperrors.FileSystemError{Path: filepath.Join(msgPath, "metadata.json"), Msg: "failed to read metadata file for final update", Err: err}
	}

	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to unmarshal metadata for final update: %w", err)
	}

	metadata.Attachments = attachments

	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode final metadata to JSON: %w", err)
	}

	if err := root.WriteFile("metadata.json", metadataJSON, 0600); err != nil {
		return &apperrors.FileSystemError{Path: filepath.Join(msgPath, "metadata.json"), Msg: "failed to write final metadata file", Err: err}
	}

	return nil
}

// --- State and Reporting ---

// SaveState saves the RunState to the given file path as JSON. It uses atomic
// writes (temp file + rename) via os.OpenRoot for security and stability.
func (fh *FileHandler) SaveState(state *o365client.RunState, stateFilePath string) error {
	mu := fh.getMutex(stateFilePath)
	mu.Lock()
	defer mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state to JSON: %w", err)
	}

	stateDir := filepath.Dir(stateFilePath)
	stateFile := filepath.Base(stateFilePath)
	tempFile := stateFile + ".tmp"

	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return &apperrors.FileSystemError{Path: stateDir, Msg: "failed to open root for saving state", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	// Atomic write: Write to temp file first
	if err := root.WriteFile(tempFile, data, 0600); err != nil {
		return &apperrors.FileSystemError{Path: filepath.Join(stateDir, tempFile), Msg: "failed to write temporary state file", Err: err}
	}

	// Rename temp file to final destination
	if err := root.Rename(tempFile, stateFile); err != nil {
		_ = root.Remove(tempFile) // Attempt cleanup
		return &apperrors.FileSystemError{Path: stateFilePath, Msg: "failed to rename temporary state file", Err: err}
	}

	return nil
}

// LoadState loads the RunState from the given file path. It uses os.OpenRoot
// for secure workspace access.
func (fh *FileHandler) LoadState(stateFilePath string) (*o365client.RunState, error) {
	stateDir := filepath.Dir(stateFilePath)
	stateFile := filepath.Base(stateFilePath)

	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: stateDir, Msg: "failed to open root for loading state", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	content, err := root.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &o365client.RunState{}, nil // File does not exist, return empty state
		}
		return nil, &apperrors.FileSystemError{Path: stateFilePath, Msg: "failed to read state file", Err: err}
	}

	var state o365client.RunState
	err = json.Unmarshal(content, &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal state from JSON: %w", err)
	}
	return &state, nil
}

// SaveError persists a list of errors encountered during message processing
// into an 'error.json' file within the message directory. It uses Go 1.24+
// os.OpenRoot for secure file access.
func (fh *FileHandler) SaveError(msgPath string, errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	var existingErrors []ErrorDetail

	// Use os.OpenRoot (Go 1.24+) to safely read existing errors and prevent directory traversal (G304)
	if root, err := os.OpenRoot(msgPath); err == nil {
		if data, err := root.ReadFile("error.json"); err == nil {
			_ = json.Unmarshal(data, &existingErrors)
		}
		_ = root.Close() // Explicitly ignore close error for read-only root
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	for _, err := range errs {
		if err != nil {
			existingErrors = append(existingErrors, ErrorDetail{
				Timestamp: timestamp,
				Message:   err.Error(),
			})
		}
	}

	data, err := json.MarshalIndent(existingErrors, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal error details: %w", err)
	}

	// Use os.OpenRoot (Go 1.24+) to safely write the error file
	root, err := os.OpenRoot(msgPath)
	if err != nil {
		return fmt.Errorf("failed to open root for writing error: %w", err)
	}
	defer func() {
		_ = root.Close()
	}()

	return root.WriteFile("error.json", data, 0600)
}

// SaveStatusReport generates a root-level JSON report summarizing the entire run,
// including initial mailbox counts and final processing statistics.
// It uses os.OpenRoot for secure workspace access.
func (fh *FileHandler) SaveStatusReport(mailboxName string, sourceCounts map[string]int32, processedCount, errorCount int) error {
	now := time.Now().UTC()
	timestamp := now.Format(time.RFC3339)
	fileTimestamp := now.Format("20060102_150405")

	status := JobStatus{
		Mailbox:           mailboxName,
		Timestamp:         timestamp,
		SourceCounts:      sourceCounts,
		JobProcessedCount: processedCount,
		JobErrorCount:     errorCount,
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal job status: %w", err)
	}

	root, err := os.OpenRoot(fh.workspacePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "failed to open workspace root for status report", Err: err}
	}
	defer func() {
		_ = root.Close()
	}()

	fileName := fmt.Sprintf("status_%s.json", fileTimestamp)
	return root.WriteFile(fileName, data, 0600)
}

// --- Utilities and Helpers ---

// ExportToRecipient is an exported version of toRecipient for testing purposes.
func ExportToRecipient(mRecipient models.Recipientable) Recipient {
	return toRecipient(mRecipient)
}

// ExportExtractFilesFromEnvelope is an exported version of extractFilesFromEnvelope for testing purposes.
func (fh *FileHandler) ExportExtractFilesFromEnvelope(root *os.Root, env *enmime.Envelope, parentSequence int) []AttachmentMetadata {
	return fh.extractFilesFromEnvelope(root, env, parentSequence)
}

// ExportGetMutex is an exported version of getMutex for testing purposes.
func (fh *FileHandler) ExportGetMutex(filePath string) *sync.Mutex {
	return fh.getMutex(filePath)
}

// ExportCopyWithContext is an exported version of copyWithContext for testing purposes.
func ExportCopyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	return copyWithContext(ctx, dst, src)
}

// copyWithContext performs an io.Copy but respects context cancellation.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	type result struct {
		n   int64
		err error
	}
	resChan := make(chan result, 1)

	go func() {
		n, err := io.Copy(dst, src)
		resChan <- result{n, err}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-resChan:
		return res.err
	}
}

// SanitizeFileName removes invalid characters from a string to be used as a file name.
func SanitizeFileName(name string) string {
	// Replace path traversal sequences iteratively to handle cases like "...".
	sanitized := name
	for strings.Contains(sanitized, "..") {
		sanitized = strings.ReplaceAll(sanitized, "..", "_")
	}

	// Define a regex to match invalid characters for Windows/Unix file names
	// \ is for backslash, \x00-\x1f are control characters
	invalidChars := regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
	sanitized = invalidChars.ReplaceAllString(sanitized, "_")

	// Prevent filenames starting with a dash
	if strings.HasPrefix(sanitized, "-") {
		sanitized = "_" + sanitized[1:]
	}

	// Truncate to a reasonable length to avoid filesystem errors
	const maxLen = 200
	if len(sanitized) > maxLen {
		sanitized = sanitized[:maxLen]
	}

	return sanitized
}

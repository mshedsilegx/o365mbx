// Package filehandler manages all local file system operations, including
// workspace creation, message saving, attachment handling, and state persistence.
package filehandler

import (
	"context"
	"encoding/json"
	"errors"
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

// isPathTooLong checks if a given path exceeds the maximum allowed length.
func isPathTooLong(path string) bool {
	return len(path) > maxPathLength
}

// AttachmentMetadata holds metadata for a single attachment.
// AttachmentMetadata holds information about a saved attachment for inclusion in metadata.json.
type AttachmentMetadata struct {
	Name        string `json:"attachment_name_in_message"`
	ContentType string `json:"content_type_of_attachment"`
	Size        int32  `json:"size_of_attachment_in_bytes"`
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

type FileHandlerInterface interface {
	CreateWorkspace() error
	SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error)
	SaveAttachment(ctx context.Context, msgPath string, att models.Attachmentable, accessToken string, sequence int) (*AttachmentMetadata, error)
	SaveAttachmentFromBytes(ctx context.Context, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error)
	WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error
	SaveState(state *o365client.RunState, stateFilePath string) error
	LoadState(stateFilePath string) (*o365client.RunState, error)
}

// FileHandler handles the disk-based storage and retrieval logic for the application.
type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	emailProcessor           emailprocessor.EmailProcessorInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	bandwidthLimiter         *rate.Limiter
	fileMutexes              map[string]*sync.Mutex
	mapMutex                 sync.Mutex
	msgHandler               string
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, largeAttachmentThresholdMB, chunkSizeMB int, bandwidthLimitMBs float64, msgHandler string, logger log.FieldLogger) *FileHandler {
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
		fileMutexes:              make(map[string]*sync.Mutex),
		msgHandler:               msgHandler,
	}
}

// getMutex returns a mutex for a given file path, creating it if it doesn't exist.
func (fh *FileHandler) getMutex(filePath string) *sync.Mutex {
	fh.mapMutex.Lock()
	defer fh.mapMutex.Unlock()

	if mu, ok := fh.fileMutexes[filePath]; ok {
		return mu
	}

	mu := &sync.Mutex{}
	fh.fileMutexes[filePath] = mu
	return mu
}

// CreateWorkspace creates the unique workspace directory and verifies it's not a symlink.
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

// SaveMessage creates the directory structure for a message and saves the metadata and body.
func (fh *FileHandler) SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error) {
	messageID := utils.StringValue(message.GetId(), "unknown")
	msgPath := filepath.Join(fh.workspacePath, messageID)
	if isPathTooLong(msgPath) {
		return "", &apperrors.FileSystemError{Path: msgPath, Msg: "generated message path exceeds maximum length", Err: nil}
	}
	attachmentsPath := filepath.Join(msgPath, "attachments")
	if err := os.MkdirAll(attachmentsPath, 0700); err != nil {
		return "", &apperrors.FileSystemError{Path: attachmentsPath, Msg: "failed to create attachments directory", Err: err}
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
		// Fallback for any unexpected value, though config validation should prevent this.
		bodyExt = ".txt"
		bodyContentType = "text/plain"
	}
	bodyFileName := "body" + bodyExt

	// Save the body
	bodyFilePath := filepath.Join(msgPath, bodyFileName)
	if isPathTooLong(bodyFilePath) {
		return "", &apperrors.FileSystemError{Path: bodyFilePath, Msg: "generated body file path exceeds maximum length", Err: nil}
	}
	if err := fh.saveEmailBody(bodyFilePath, bodyContent); err != nil {
		return "", err
	}

	// Create and save metadata
	metadata := Metadata{
		To:              toRecipients(message.GetToRecipients()),
		Cc:              toRecipients(message.GetCcRecipients()),
		From:            toRecipient(message.GetFrom()),
		Subject:         utils.StringValue(message.GetSubject(), "(no subject)"),
		ReceivedDate:    utils.TimeValue(message.GetReceivedDateTime(), time.Time{}),
		BodyFile:        bodyFileName,
		BodyContentType: bodyContentType,
		AttachmentCount: len(message.GetAttachments()),
		Attachments:     []AttachmentMetadata{}, // This will be populated as attachments are saved
	}

	metadataPath := filepath.Join(msgPath, "metadata.json")
	if isPathTooLong(metadataPath) {
		return "", &apperrors.FileSystemError{Path: metadataPath, Msg: "generated metadata file path exceeds maximum length", Err: nil}
	}
	// #nosec G304 - metadataPath is constructed from workspacePath and messageID, not user input.
	metadataFile, err := os.Create(metadataPath)
	if err != nil {
		return "", &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to create metadata file", Err: err}
	}
	defer func() {
		if err := metadataFile.Close(); err != nil {
			log.Warnf("Failed to close metadata file: %v", err)
		}
	}()

	encoder := json.NewEncoder(metadataFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return "", fmt.Errorf("failed to encode metadata to JSON: %w", err)
	}

	return msgPath, nil
}

// saveEmailBody saves the email body to a specific file path.
func (fh *FileHandler) saveEmailBody(filePath string, bodyContent interface{}) error {
	var data []byte
	switch content := bodyContent.(type) {
	case string:
		data = []byte(content)
	case []byte:
		data = content
	default:
		return fmt.Errorf("unsupported body content type: %T", bodyContent)
	}

	err := os.WriteFile(filePath, data, 0600)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to save email body", Err: err}
	}
	return nil
}

// SaveAttachment saves an attachment to a file and returns its metadata.
func (fh *FileHandler) SaveAttachment(ctx context.Context, msgPath string, att models.Attachmentable, accessToken string, sequence int) (metadata *AttachmentMetadata, err error) {
	// This function is now a placeholder and will not be used for downloading from URL.
	// The new approach is to get the attachment content directly.
	return nil, errors.New("SaveAttachment with download URL is deprecated, use SaveAttachmentFromBytes")
}

// SaveAttachmentFromBytes saves an attachment (File or Item) and returns a list of metadata for it and any nested attachments.
// SaveAttachmentFromBytes determines the type of attachment and delegates to the appropriate save method.
func (fh *FileHandler) SaveAttachmentFromBytes(ctx context.Context, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error) {
	if _, ok := att.(*models.FileAttachment); ok {
		meta, err := fh.saveFileAttachment(msgPath, att, sequence)
		if err != nil {
			return nil, err
		}
		return []AttachmentMetadata{*meta}, nil
	}

	if _, ok := att.(*models.ItemAttachment); ok {
		return fh.SaveItemAttachment(ctx, mailboxName, messageID, msgPath, att, sequence)
	}

	return nil, fmt.Errorf("unsupported attachment type: %T", att)
}

// saveFileAttachment saves a standard file attachment.
func (fh *FileHandler) saveFileAttachment(msgPath string, att models.Attachmentable, sequence int) (*AttachmentMetadata, error) {
	fileAttachment, _ := att.(*models.FileAttachment)
	fileName := fmt.Sprintf("%02d_%s", sequence, sanitizeFileName(utils.StringValue(fileAttachment.GetName(), "unnamed")))
	filePath := filepath.Join(msgPath, "attachments", fileName)
	if isPathTooLong(filePath) {
		return nil, &apperrors.FileSystemError{Path: filePath, Msg: "generated attachment file path exceeds maximum length", Err: nil}
	}

	contentBytes := fileAttachment.GetContentBytes()
	if contentBytes == nil {
		return nil, fmt.Errorf("attachment %s has no content bytes", utils.StringValue(fileAttachment.GetName(), "unnamed"))
	}

	// #nosec G304 - filePath is constructed from msgPath and fileName, not direct user input.
	err := os.WriteFile(filePath, contentBytes, 0600)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: filePath, Msg: "failed to save file attachment", Err: err}
	}

	log.WithField("attachmentName", utils.StringValue(fileAttachment.GetName(), "unnamed")).Infof("Successfully saved file attachment.")
	return &AttachmentMetadata{
		Name:        utils.StringValue(fileAttachment.GetName(), "unnamed"),
		ContentType: utils.StringValue(fileAttachment.GetContentType(), "application/octet-stream"),
		Size:        utils.Int32Value(fileAttachment.GetSize(), 0),
		SavedAs:     fileName,
	}, nil
}

// SaveItemAttachment downloads an ItemAttachment as MIME, saves it, and optionally extracts its nested attachments.
// SaveItemAttachment handles .msg/.eml attachments by downloading the raw MIME stream
// and optionally extracting its contents based on the msgHandler configuration.
func (fh *FileHandler) SaveItemAttachment(ctx context.Context, mailboxName, messageID string, msgPath string, att models.Attachmentable, sequence int) ([]AttachmentMetadata, error) {
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
	saveName := fmt.Sprintf("%02d_%s", sequence, sanitizeFileName(attachmentName))
	if !strings.Contains(saveName, ".") {
		saveName += ".eml"
	}
	filePath := filepath.Join(msgPath, "attachments", saveName)

	// #nosec G304 - filePath is constructed from msgPath and saveName, not direct user input.
	file, err := os.Create(filePath)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: filePath, Msg: "failed to create item attachment file", Err: err}
	}

	_, err = io.Copy(file, rawMimeStream)
	if closeErr := file.Close(); closeErr != nil {
		log.Warnf("Failed to close attachment file %s: %v", filePath, closeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stream item attachment to disk: %w", err)
	}

	fileInfo, _ := os.Stat(filePath)
	allMetadata := []AttachmentMetadata{
		{
			Name:        attachmentName,
			ContentType: utils.StringValue(att.GetContentType(), "message/rfc822"),
			// #nosec G115 - fileInfo.Size() is converted to int32 for metadata. Email attachments are practically much smaller than 2GB.
			Size:    int32(fileInfo.Size()),
			SavedAs: saveName,
		},
	}

	// 2. If extractor mode, parse and extract body + 1 level of attachments
	if fh.msgHandler == "extractor" {
		// #nosec G304 - filePath is constructed from msgPath and saveName, not direct user input.
		savedFile, err := os.Open(filePath)
		if err != nil {
			log.WithFields(log.Fields{"path": filePath, "error": err}).Warn("Failed to re-open attachment for extraction.")
			return allMetadata, nil
		}
		defer func() {
			if closeErr := savedFile.Close(); closeErr != nil {
				log.Warnf("Failed to close saved file %s: %v", filePath, closeErr)
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
			bodyName := fmt.Sprintf("%02d_%s_extracted%s", sequence, sanitizeFileName(attachmentName), bodyExt)
			bodyPath := filepath.Join(msgPath, "attachments", bodyName)
			// #nosec G304 - bodyPath is constructed from msgPath and bodyName, not direct user input.
			if err := os.WriteFile(bodyPath, []byte(bodyContent), 0600); err == nil {
				allMetadata = append(allMetadata, AttachmentMetadata{
					Name:        attachmentName + " (body)",
					ContentType: "text/html",
					// #nosec G115 - len(bodyContent) is converted to int32 for metadata. Email bodies are practically much smaller than 2GB.
					Size:    int32(len(bodyContent)),
					SavedAs: bodyName,
				})
			}
		}

		// Extract 1 level of nested attachments
		nestedMetas := fh.extractFilesFromEnvelope(env, msgPath, sequence)
		allMetadata = append(allMetadata, nestedMetas...)
	}

	return allMetadata, nil
}

// extractFilesFromEnvelope handles one level of extraction from a parsed MIME envelope.
func (fh *FileHandler) extractFilesFromEnvelope(env *enmime.Envelope, msgPath string, parentSequence int) []AttachmentMetadata {
	var metadatas []AttachmentMetadata

	for i, part := range env.Attachments {
		nestedCount := i + 1
		fileName := part.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("attachment_%d", nestedCount)
		}

		safeName := fmt.Sprintf("%02d_%d_%s", parentSequence, nestedCount, sanitizeFileName(fileName))
		filePath := filepath.Join(msgPath, "attachments", safeName)

		log.WithField("nestedName", fileName).Infof("-> Extracting: %s", safeName)

		// #nosec G304 - filePath is constructed from msgPath and safeName, not direct user input.
		if err := os.WriteFile(filePath, part.Content, 0600); err != nil {
			log.WithFields(log.Fields{"fileName": fileName, "error": err}).Error("Failed to save nested attachment.")
			continue
		}

		metadatas = append(metadatas, AttachmentMetadata{
			Name:        fileName,
			ContentType: part.ContentType,
			// #nosec G115 - len(part.Content) is converted to int32 for metadata. Email attachments are practically much smaller than 2GB.
			Size:    int32(len(part.Content)),
			SavedAs: safeName,
		})
	}

	return metadatas
}

// WriteAttachmentsToMetadata writes the final list of attachments to the metadata.json file.
func (fh *FileHandler) WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error {
	metadataPath := filepath.Join(msgPath, "metadata.json")
	mu := fh.getMutex(metadataPath)
	mu.Lock()
	defer mu.Unlock()

	// #nosec G304 - metadataPath is constructed from msgPath and "metadata.json", not direct user input.
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to read metadata file for final update", Err: err}
	}

	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to unmarshal metadata for final update: %w", err)
	}

	metadata.Attachments = attachments

	// #nosec G304 - metadataPath is constructed from msgPath and "metadata.json", not direct user input.
	metadataFile, err := os.Create(metadataPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to create metadata file for final update", Err: err}
	}
	defer func() {
		if err := metadataFile.Close(); err != nil {
			log.Warnf("Failed to close metadata file for final update: %v", err)
		}
	}()

	encoder := json.NewEncoder(metadataFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return fmt.Errorf("failed to encode final metadata to JSON: %w", err)
	}

	return nil
}

// SaveState saves the RunState to the given file path as JSON.
func (fh *FileHandler) SaveState(state *o365client.RunState, stateFilePath string) error {
	mu := fh.getMutex(stateFilePath)
	mu.Lock()
	defer mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state to JSON: %w", err)
	}
	// #nosec G304 - stateFilePath is provided via configuration, not direct user input.
	err = os.WriteFile(stateFilePath, data, 0600)
	if err != nil {
		return &apperrors.FileSystemError{Path: stateFilePath, Msg: "failed to save state file", Err: err}
	}
	return nil
}

// LoadState loads the RunState from the given file path.
func (fh *FileHandler) LoadState(stateFilePath string) (*o365client.RunState, error) {
	// #nosec G304 - stateFilePath is provided via configuration, not direct user input.
	content, err := os.ReadFile(stateFilePath)
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

// sanitizeFileName removes invalid characters from a string to be used as a file name.
func sanitizeFileName(name string) string {
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

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

	"github.com/microsoftgraph/msgraph-sdk-go/models"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"

	"o365mbx/apperrors"
	"o365mbx/emailprocessor"
	"o365mbx/o365client"
)

// AttachmentMetadata holds metadata for a single attachment.
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
	SaveAttachmentFromBytes(msgPath string, att models.Attachmentable, sequence int) (*AttachmentMetadata, error)
	WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error
	SaveState(state *o365client.RunState, stateFilePath string) error
	LoadState(stateFilePath string) (*o365client.RunState, error)
}

type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	emailProcessor           emailprocessor.EmailProcessorInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	bandwidthLimiter         *rate.Limiter
	fileMutexes              map[string]*sync.Mutex
	mapMutex                 sync.Mutex
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, emailProcessor emailprocessor.EmailProcessorInterface, largeAttachmentThresholdMB, chunkSizeMB int, bandwidthLimitMBs float64) *FileHandler {
	var limiter *rate.Limiter
	if bandwidthLimitMBs > 0 {
		limit := rate.Limit(bandwidthLimitMBs * 1024 * 1024)
		limiter = rate.NewLimiter(limit, int(limit)) // Burst equal to the limit
		log.WithField("limit", limit).Info("Bandwidth limiting enabled.")
	}

	return &FileHandler{
		workspacePath:            workspacePath,
		o365Client:               o365Client,
		emailProcessor:           emailProcessor,
		largeAttachmentThreshold: largeAttachmentThresholdMB * 1024 * 1024,
		chunkSize:                chunkSizeMB * 1024 * 1024,
		bandwidthLimiter:         limiter,
		fileMutexes:              make(map[string]*sync.Mutex),
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
	err := os.MkdirAll(fh.workspacePath, 0755)
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
	if mRecipient == nil {
		return Recipient{}
	}
	return Recipient{
		EmailAddress: EmailAddress{
			Name:    *mRecipient.GetEmailAddress().GetName(),
			Address: *mRecipient.GetEmailAddress().GetAddress(),
		},
	}
}

// SaveMessage creates the directory structure for a message and saves the metadata and body.
func (fh *FileHandler) SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error) {
	msgPath := filepath.Join(fh.workspacePath, *message.GetId())
	attachmentsPath := filepath.Join(msgPath, "attachments")
	if err := os.MkdirAll(attachmentsPath, 0755); err != nil {
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
	if err := fh.saveEmailBody(filepath.Join(msgPath, bodyFileName), bodyContent); err != nil {
		return "", err
	}

	// Create and save metadata
	metadata := Metadata{
		To:              toRecipients(message.GetToRecipients()),
		Cc:              toRecipients(message.GetCcRecipients()),
		From:            toRecipient(message.GetFrom()),
		Subject:         *message.GetSubject(),
		ReceivedDate:    *message.GetReceivedDateTime(),
		BodyFile:        bodyFileName,
		BodyContentType: bodyContentType,
		AttachmentCount: len(message.GetAttachments()),
		Attachments:     []AttachmentMetadata{}, // This will be populated as attachments are saved
	}

	metadataPath := filepath.Join(msgPath, "metadata.json")
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

	err := os.WriteFile(filePath, data, 0644)
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

// SaveAttachmentFromBytes saves an attachment from its base64 encoded content and returns its metadata.
func (fh *FileHandler) SaveAttachmentFromBytes(msgPath string, att models.Attachmentable, sequence int) (metadata *AttachmentMetadata, err error) {
	fileAttachment, ok := att.(*models.FileAttachment)
	if !ok {
		return nil, fmt.Errorf("attachment is not a file attachment")
	}

	fileName := fmt.Sprintf("%02d_%s", sequence, sanitizeFileName(*fileAttachment.GetName()))
	filePath := filepath.Join(msgPath, "attachments", fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return nil, &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment from bytes", Err: err}
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = &apperrors.FileSystemError{Path: filePath, Msg: "failed to close file after writing attachment", Err: closeErr}
		}
	}()

	contentBytes := fileAttachment.GetContentBytes()
	if contentBytes == nil {
		return nil, fmt.Errorf("attachment %s has no content bytes", *fileAttachment.GetName())
	}

	_, err = out.Write(contentBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode and save base64 content for attachment %s: %w", *fileAttachment.GetName(), err)
	}

	log.WithField("attachmentName", *fileAttachment.GetName()).Infof("Successfully saved attachment from contentBytes.")
	metadata = &AttachmentMetadata{
		Name:        *fileAttachment.GetName(),
		ContentType: *fileAttachment.GetContentType(),
		Size:        *fileAttachment.GetSize(),
		SavedAs:     fileName,
	}
	return
}

// WriteAttachmentsToMetadata writes the final list of attachments to the metadata.json file.
func (fh *FileHandler) WriteAttachmentsToMetadata(msgPath string, attachments []AttachmentMetadata) error {
	metadataPath := filepath.Join(msgPath, "metadata.json")
	mu := fh.getMutex(metadataPath)
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to read metadata file for final update", Err: err}
	}

	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to unmarshal metadata for final update: %w", err)
	}

	metadata.Attachments = attachments

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
	err = os.WriteFile(stateFilePath, data, 0644)
	if err != nil {
		return &apperrors.FileSystemError{Path: stateFilePath, Msg: "failed to save state file", Err: err}
	}
	return nil
}

// LoadState loads the RunState from the given file path.
func (fh *FileHandler) LoadState(stateFilePath string) (*o365client.RunState, error) {
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
	// Replace path traversal sequences
	sanitized := strings.ReplaceAll(name, "..", "_")

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

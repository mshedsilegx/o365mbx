package filehandler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	Size        int    `json:"size_of_attachment_in_bytes"`
	SavedAs     string `json:"attachment_name_stored_after_download"`
}

// Metadata holds all metadata for a given email message.
type Metadata struct {
	To                []o365client.Recipient `json:"to"`
	Cc                []o365client.Recipient `json:"cc"`
	From              o365client.Recipient   `json:"from"`
	Subject           string                 `json:"subject"`
	ReceivedDate      time.Time              `json:"received_date"`
	BodyFile          string                 `json:"body"`
	BodyContentType   string                 `json:"content_type_of_body"`
	AttachmentCount   int                    `json:"attachment_counts"`
	Attachments       []AttachmentMetadata   `json:"list_of_attachments"`
}

type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	emailProcessor           *emailprocessor.EmailProcessor
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	bandwidthLimiter         *rate.Limiter
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, emailProcessor *emailprocessor.EmailProcessor, largeAttachmentThresholdMB, chunkSizeMB int, bandwidthLimitMBs float64) *FileHandler {
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
	}
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

// SaveMessage creates the directory structure for a message and saves the metadata and body.
func (fh *FileHandler) SaveMessage(message *o365client.Message, bodyContent interface{}, convertBody string) (string, error) {
	msgPath := filepath.Join(fh.workspacePath, message.ID)
	attachmentsPath := filepath.Join(msgPath, "attachments")
	if err := os.MkdirAll(attachmentsPath, 0755); err != nil {
		return "", &apperrors.FileSystemError{Path: attachmentsPath, Msg: "failed to create attachments directory", Err: err}
	}

	var bodyExt, bodyContentType string
	switch convertBody {
	case "text":
		bodyExt = ".txt"
		bodyContentType = "text"
	case "pdf":
		bodyExt = ".pdf"
		bodyContentType = "pdf"
	case "none":
		if contentStr, ok := bodyContent.(string); ok && fh.emailProcessor.IsHTML(contentStr) {
			bodyExt = ".html"
			bodyContentType = "html"
		} else {
			bodyExt = ".txt"
			bodyContentType = "text"
		}
	default:
		// Fallback for any unexpected value, though config validation should prevent this.
		bodyExt = ".txt"
		bodyContentType = "text"
	}
	bodyFileName := "body" + bodyExt

	// Save the body
	if err := fh.saveEmailBody(filepath.Join(msgPath, bodyFileName), bodyContent); err != nil {
		return "", err
	}

	// Create and save metadata
	metadata := Metadata{
		To:              message.ToRecipients,
		Cc:              message.CcRecipients,
		From:            message.From,
		Subject:         message.Subject,
		ReceivedDate:    message.ReceivedDateTime,
		BodyFile:        bodyFileName,
		BodyContentType: bodyContentType,
		AttachmentCount: len(message.Attachments),
		Attachments:     []AttachmentMetadata{}, // This will be populated as attachments are saved
	}

	metadataPath := filepath.Join(msgPath, "metadata.json")
	metadataFile, err := os.Create(metadataPath)
	if err != nil {
		return "", &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to create metadata file", Err: err}
	}
	defer metadataFile.Close()

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

// limitedReader is a wrapper around an io.Reader that applies a rate limit.
type limitedReader struct {
	reader  io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

func (lr *limitedReader) Read(p []byte) (n int, err error) {
	n, err = lr.reader.Read(p)
	if n > 0 {
		if err := lr.limiter.WaitN(lr.ctx, n); err != nil {
			return n, err
		}
	}
	return
}

// SaveAttachment saves an attachment to a file.
func (fh *FileHandler) SaveAttachment(ctx context.Context, msgPath string, att o365client.Attachment, accessToken string, sequence int) error {
	fileName := fmt.Sprintf("%02d_%s", sequence, sanitizeFileName(att.Name))
	filePath := filepath.Join(msgPath, "attachments", fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment", Err: err}
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Warnf("Error closing file %s: %v", filePath, err)
		}
	}()

	if att.Size <= fh.largeAttachmentThreshold {
		// Existing direct download logic for smaller attachments
		req, err := http.NewRequestWithContext(ctx, "GET", att.DownloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create HTTP request for attachment download: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := fh.o365Client.DoRequestWithRetry(req)
		if err != nil {
			if apiErr, ok := err.(*apperrors.APIError); ok {
				return apiErr
			}
			if authErr, ok := err.(*apperrors.AuthError); ok {
				return authErr
			}
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("attachment download cancelled by user: %w", err)
			} else if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("attachment download timed out: %w", err)
			} else {
				return fmt.Errorf("failed to download attachment from %s: %w", att.DownloadURL, err)
			}
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body for small attachment: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download small attachment from %s", att.DownloadURL)}
		}

		var reader io.Reader = resp.Body
		if fh.bandwidthLimiter != nil {
			reader = &limitedReader{reader: resp.Body, limiter: fh.bandwidthLimiter, ctx: ctx}
		}

		_, err = io.Copy(out, reader)
		if err != nil {
			return &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment content to file", Err: err}
		}
		log.WithField("attachmentName", att.Name).Infof("Successfully downloaded small attachment.")
	} else {
		// Chunked download logic for large attachments
		log.WithFields(log.Fields{"attachmentName": att.Name, "attachmentSize": att.Size}).Infof("Downloading large attachment in chunks.")
		var downloadedBytes int64 = 0
		for downloadedBytes < int64(att.Size) {
			select {
			case <-ctx.Done():
				log.WithField("error", ctx.Err()).Warn("Context cancelled during large attachment download.")
				return ctx.Err()
			default:
			}

			endByte := downloadedBytes + int64(fh.chunkSize) - 1
			if endByte >= int64(att.Size) {
				endByte = int64(att.Size) - 1
			}

			req, err := http.NewRequestWithContext(ctx, "GET", att.DownloadURL, nil)
			if err != nil {
				return fmt.Errorf("failed to create HTTP request for attachment chunk: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", downloadedBytes, endByte))

			resp, err := fh.o365Client.DoRequestWithRetry(req)
			if err != nil {
				if apiErr, ok := err.(*apperrors.APIError); ok {
					return apiErr
				}
				if authErr, ok := err.(*apperrors.AuthError); ok {
					return authErr
				}
				if errors.Is(err, context.Canceled) {
					return fmt.Errorf("attachment download cancelled by user: %w", err)
				} else if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("attachment download timed out: %w", err)
				} else {
					return fmt.Errorf("failed to download attachment chunk from %s (range %d-%d): %w", att.DownloadURL, downloadedBytes, endByte, err)
				}
			}

			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				if err := resp.Body.Close(); err != nil {
					log.Warnf("Error closing response body for failed large attachment chunk: %v", err)
				}
				return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download attachment chunk from %s (range %d-%d)", att.DownloadURL, downloadedBytes, endByte)}
			}

			n, err := io.Copy(out, resp.Body)
			if err != nil {
				if err := resp.Body.Close(); err != nil {
					log.Warnf("Error closing response body after partial write for large attachment chunk: %v", err)
				}
				return &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment chunk content to file", Err: err}
			}
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body for large attachment chunk: %v", err)
			}
			downloadedBytes += n
			log.WithFields(log.Fields{"downloadedBytes": n, "attachmentName": att.Name, "totalDownloaded": downloadedBytes, "attachmentSize": att.Size}).Debug("Downloaded bytes for attachment.")
		}
		log.WithField("attachmentName", att.Name).Infof("Successfully downloaded large attachment.")
	}

	return fh.UpdateMetadataWithAttachment(msgPath, att, fileName)
}

// SaveAttachmentFromBytes saves an attachment from its base64 encoded content using a streaming approach.
func (fh *FileHandler) SaveAttachmentFromBytes(msgPath string, att o365client.Attachment, sequence int) error {
	fileName := fmt.Sprintf("%02d_%s", sequence, sanitizeFileName(att.Name))
	filePath := filepath.Join(msgPath, "attachments", fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment from bytes", Err: err}
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Warnf("Error closing file %s: %v", filePath, err)
		}
	}()

	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(att.ContentBytes))
	_, err = io.Copy(out, decoder)
	if err != nil {
		return fmt.Errorf("failed to decode and save base64 content for attachment %s: %w", att.Name, err)
	}

	log.WithField("attachmentName", att.Name).Infof("Successfully saved attachment from contentBytes.")
	return fh.UpdateMetadataWithAttachment(msgPath, att, fileName)
}

// UpdateMetadataWithAttachment appends attachment metadata to the metadata.json file.
func (fh *FileHandler) UpdateMetadataWithAttachment(msgPath string, att o365client.Attachment, savedAs string) error {
	metadataPath := filepath.Join(msgPath, "metadata.json")

	// Read existing metadata
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to read metadata file for update", Err: err}
	}

	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to unmarshal metadata for update: %w", err)
	}

	// Append new attachment info
	attMetadata := AttachmentMetadata{
		Name:        att.Name,
		ContentType: att.ContentType,
		Size:        att.Size,
		SavedAs:     savedAs,
	}
	metadata.Attachments = append(metadata.Attachments, attMetadata)

	// Write updated metadata back to the file
	metadataFile, err := os.Create(metadataPath)
	if err != nil {
		return &apperrors.FileSystemError{Path: metadataPath, Msg: "failed to create metadata file for update", Err: err}
	}
	defer metadataFile.Close()

	encoder := json.NewEncoder(metadataFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return fmt.Errorf("failed to encode updated metadata to JSON: %w", err)
	}

	return nil
}

// SaveState saves the RunState to the given file path as JSON.
func (fh *FileHandler) SaveState(state *o365client.RunState, stateFilePath string) error {
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

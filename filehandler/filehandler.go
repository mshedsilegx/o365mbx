package filehandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"

	"o365mbx/apperrors"
	"o365mbx/o365client"
)

type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	dirPerms                 os.FileMode
	filePerms                os.FileMode
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, largeAttachmentThresholdMB, chunkSizeMB int, dirPerms, filePerms int) *FileHandler {
	return &FileHandler{
		workspacePath:            workspacePath,
		o365Client:               o365Client,
		largeAttachmentThreshold: largeAttachmentThresholdMB * 1024 * 1024,
		chunkSize:                chunkSizeMB * 1024 * 1024,
		dirPerms:                 os.FileMode(dirPerms),
		filePerms:                os.FileMode(filePerms),
	}
}

// CreateWorkspace creates the unique workspace directory.
func (fh *FileHandler) CreateWorkspace() error {
	err := os.MkdirAll(fh.workspacePath, fh.dirPerms)
	if err != nil {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "failed to create workspace directory", Err: err}
	}
	return nil
}

// AttachmentData defines the structure for attachment information in the JSON output.
type AttachmentData struct {
	Name        string `json:"name"`
	Size        int    `json:"size"`
	DownloadURL string `json:"downloadUrl"`
}

// StatusData defines the structure for processing status in the JSON output.
type StatusData struct {
	State   string `json:"state"`
	Details string `json:"details"`
}

// EmailData defines the structure for the JSON output file.
type EmailData struct {
	To           []string         `json:"to"`
	From         string           `json:"from"`
	Subject      string           `json:"subject"`
	ReceivedDate string           `json:"receivedDate"`
	BodyFile     string           `json:"bodyFile"`
	Attachments  []AttachmentData `json:"attachments"`
	Status       StatusData       `json:"status"`
}

// SaveBodyAsText saves the email body to a text file and returns the file name.
func (fh *FileHandler) SaveBodyAsText(messageID, bodyContent string) (string, error) {
	fileName := fmt.Sprintf("%s_body.txt", messageID)
	filePath := filepath.Join(fh.workspacePath, fileName)
	err := os.WriteFile(filePath, []byte(bodyContent), fh.filePerms)
	if err != nil {
		return "", &apperrors.FileSystemError{Path: filePath, Msg: "failed to save email body text file", Err: err}
	}
	return fileName, nil
}

// SaveEmailAsJSON saves the email data as a JSON file.
func (fh *FileHandler) SaveEmailAsJSON(messageID string, data EmailData) error {
	fileName := fmt.Sprintf("%s.json", messageID)
	filePath := filepath.Join(fh.workspacePath, fileName)

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal email data to JSON: %w", err)
	}

	err = os.WriteFile(filePath, jsonData, fh.filePerms)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to save email JSON file", Err: err}
	}
	return nil
}

// SaveAttachment saves an attachment to a file and returns the new file name.
func (fh *FileHandler) SaveAttachment(ctx context.Context, attachmentName, messageID, downloadURL, accessToken string, attachmentSize int) (string, error) { // ctx added
	fileName := fmt.Sprintf("%s_%s_%s", sanitizeFileName(attachmentName), messageID, attachmentName)
	filePath := filepath.Join(fh.workspacePath, fileName)

	out, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fh.filePerms)
	if err != nil {
		return "", &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment", Err: err}
	}
	defer out.Close()

	if attachmentSize <= fh.largeAttachmentThreshold {
		// Existing direct download logic for smaller attachments
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil) // Pass ctx to request
		if err != nil {
			return "", fmt.Errorf("failed to create HTTP request for attachment download: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := fh.o365Client.DoRequestWithRetry(req)
		if err != nil {
			// Check if the error from DoRequestWithRetry is already a custom error
			if apiErr, ok := err.(*apperrors.APIError); ok {
				return "", apiErr // Return the specific API error
			}
			if authErr, ok := err.(*apperrors.AuthError); ok {
				return "", authErr // Return the specific Auth error
			}
			// Check for context cancellation errors
			if errors.Is(err, context.Canceled) {
				return "", fmt.Errorf("attachment download cancelled by user: %w", err)
			} else if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("attachment download timed out: %w", err)
			} else {
				// Otherwise, wrap in a generic error with more context
				return "", fmt.Errorf("failed to download attachment from %s: %w", downloadURL, err)
			}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download small attachment from %s", downloadURL)}
		}

		_, err = io.Copy(out, resp.Body)
		if err != nil {
			return "", &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment content to file", Err: err}
		}
		log.WithField("attachmentName", attachmentName).Infof("Successfully downloaded small attachment.")
	} else {
		// Chunked download logic for large attachments
		log.WithFields(log.Fields{"attachmentName": attachmentName, "attachmentSize": attachmentSize}).Infof("Downloading large attachment in chunks.")
		var downloadedBytes int64 = 0
		for downloadedBytes < int64(attachmentSize) {
			select {
			case <-ctx.Done():
				log.WithField("error", ctx.Err()).Warn("Context cancelled during large attachment download.")
				return "", ctx.Err() // Return context cancellation error
			default:
				// Continue
			}

			endByte := downloadedBytes + int64(fh.chunkSize) - 1
			if endByte >= int64(attachmentSize) {
				endByte = int64(attachmentSize) - 1
			}

			req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil) // Pass ctx to request
			if err != nil {
				return "", fmt.Errorf("failed to create HTTP request for attachment chunk: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", downloadedBytes, endByte))

			resp, err := fh.o365Client.DoRequestWithRetry(req)
			if err != nil {
				// Check if the error from DoRequestWithRetry is already a custom error
				if apiErr, ok := err.(*apperrors.APIError); ok {
					return "", apiErr // Return the specific API error
				}
				if authErr, ok := err.(*apperrors.AuthError); ok {
					return "", authErr // Return the specific Auth error
				}
				// Check for context cancellation errors
				if errors.Is(err, context.Canceled) {
					return "", fmt.Errorf("attachment download cancelled by user: %w", err)
				} else if errors.Is(err, context.DeadlineExceeded) {
					return "", fmt.Errorf("attachment download timed out: %w", err)
				} else {
					// Otherwise, wrap in a generic error with more context
					return "", fmt.Errorf("failed to download attachment chunk from %s (range %d-%d): %w", downloadURL, downloadedBytes, endByte, err)
				}
			}
			defer resp.Body.Close() // Defer inside the loop, will be closed on each iteration

			// Graph API returns 206 Partial Content for range requests, 200 OK if range is ignored or full file.
			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				return "", &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download attachment chunk from %s (range %d-%d)", downloadURL, downloadedBytes, endByte)}
			}

			n, err := io.Copy(out, resp.Body)
			if err != nil {
				return "", &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment chunk content to file", Err: err}
			}
			downloadedBytes += n
			log.WithFields(log.Fields{"downloadedBytes": n, "attachmentName": attachmentName, "totalDownloaded": downloadedBytes, "attachmentSize": attachmentSize}).Debug("Downloaded bytes for attachment.")
		}
		log.WithField("attachmentName", attachmentName).Infof("Successfully downloaded large attachment.")
	}

	return fileName, nil
}

// SaveLastRunTimestamp saves the timestamp of the last successful run.
func (fh *FileHandler) SaveLastRunTimestamp(timestamp string) error {
	filePath := filepath.Join(fh.workspacePath, "last_run_timestamp.txt")
	err := os.WriteFile(filePath, []byte(timestamp), fh.filePerms)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to save last run timestamp", Err: err}
	}
	return nil
}

// LoadLastRunTimestamp loads the timestamp of the last successful run.
func (fh *FileHandler) LoadLastRunTimestamp() (string, error) {
	filePath := filepath.Join(fh.workspacePath, "last_run_timestamp.txt")
	content, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // File does not exist, first run
		}
		return "", &apperrors.FileSystemError{Path: filePath, Msg: "failed to read last run timestamp file", Err: err}
	}
	return strings.TrimSpace(string(content)), nil
}

// sanitizeFileName removes invalid characters from a string to be used as a file name.
func sanitizeFileName(name string) string {
	// Define a regex to match invalid characters for Windows file names
	// \ is for backslash, \x00-\x1f are control characters
	// The other characters are directly included
	invalidChars := regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`) // Corrected escaping for backslash and double quote within regex
	return invalidChars.ReplaceAllString(name, "_")
}

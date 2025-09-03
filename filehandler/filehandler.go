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

	"o365mbx/apperrors"
	"o365mbx/o365client"
)

// RunState represents the state of the last successful incremental run.
type RunState struct {
	LastRunTimestamp time.Time `json:"lastRunTimestamp"`
	LastMessageID    string    `json:"lastMessageId"`
}

type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, largeAttachmentThresholdMB, chunkSizeMB int) *FileHandler {
	return &FileHandler{
		workspacePath:            workspacePath,
		o365Client:               o365Client,
		largeAttachmentThreshold: largeAttachmentThresholdMB * 1024 * 1024,
		chunkSize:                chunkSizeMB * 1024 * 1024,
	}
}

// CreateWorkspace creates the unique workspace directory.
func (fh *FileHandler) CreateWorkspace() error {
	err := os.MkdirAll(fh.workspacePath, 0755)
	if err != nil {
		return &apperrors.FileSystemError{Path: fh.workspacePath, Msg: "failed to create workspace directory", Err: err}
	}
	return nil
}

// SaveEmailBody saves the cleaned email body to a file.
func (fh *FileHandler) SaveEmailBody(subject, messageID, bodyContent string) error {
	fileName := fmt.Sprintf("%s_%s.txt", sanitizeFileName(subject), messageID)
	filePath := filepath.Join(fh.workspacePath, fileName)
	err := os.WriteFile(filePath, []byte(bodyContent), 0644)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to save email body", Err: err}
	}
	return nil
}

// SaveAttachment saves an attachment to a file.
func (fh *FileHandler) SaveAttachment(ctx context.Context, attachmentName, messageID, downloadURL, accessToken string, attachmentSize int) error { // ctx added
	fileName := fmt.Sprintf("%s_%s_%s", sanitizeFileName(attachmentName), messageID, attachmentName)
	filePath := filepath.Join(fh.workspacePath, fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment", Err: err}
	}
	defer out.Close()

	if attachmentSize <= fh.largeAttachmentThreshold {
		// Existing direct download logic for smaller attachments
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil) // Pass ctx to request
		if err != nil {
			return fmt.Errorf("failed to create HTTP request for attachment download: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := fh.o365Client.DoRequestWithRetry(req)
		if err != nil {
			// Check if the error from DoRequestWithRetry is already a custom error
			if apiErr, ok := err.(*apperrors.APIError); ok {
				return apiErr // Return the specific API error
			}
			if authErr, ok := err.(*apperrors.AuthError); ok {
				return authErr // Return the specific Auth error
			}
			// Check for context cancellation errors
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("attachment download cancelled by user: %w", err)
			} else if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("attachment download timed out: %w", err)
			} else {
				// Otherwise, wrap in a generic error with more context
				return fmt.Errorf("failed to download attachment from %s: %w", downloadURL, err)
			}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download small attachment from %s", downloadURL)}
		}

		_, err = io.Copy(out, resp.Body)
		if err != nil {
			return &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment content to file", Err: err}
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
				return ctx.Err() // Return context cancellation error
			default:
				// Continue
			}

			endByte := downloadedBytes + int64(fh.chunkSize) - 1
			if endByte >= int64(attachmentSize) {
				endByte = int64(attachmentSize) - 1
			}

			req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil) // Pass ctx to request
			if err != nil {
				return fmt.Errorf("failed to create HTTP request for attachment chunk: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", downloadedBytes, endByte))

			resp, err := fh.o365Client.DoRequestWithRetry(req)
			if err != nil {
				// Check if the error from DoRequestWithRetry is already a custom error
				if apiErr, ok := err.(*apperrors.APIError); ok {
					return apiErr // Return the specific API error
				}
				if authErr, ok := err.(*apperrors.AuthError); ok {
					return authErr // Return the specific Auth error
				}
				// Check for context cancellation errors
				if errors.Is(err, context.Canceled) {
					return fmt.Errorf("attachment download cancelled by user: %w", err)
				} else if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("attachment download timed out: %w", err)
				} else {
					// Otherwise, wrap in a generic error with more context
					return fmt.Errorf("failed to download attachment chunk from %s (range %d-%d): %w", downloadURL, downloadedBytes, endByte, err)
				}
			}
			defer resp.Body.Close() // Defer inside the loop, will be closed on each iteration

			// Graph API returns 206 Partial Content for range requests, 200 OK if range is ignored or full file.
			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download attachment chunk from %s (range %d-%d)", downloadURL, downloadedBytes, endByte)}
			}

			n, err := io.Copy(out, resp.Body)
			if err != nil {
				return &apperrors.FileSystemError{Path: filePath, Msg: "failed to write attachment chunk content to file", Err: err}
			}
			downloadedBytes += n
			log.WithFields(log.Fields{"downloadedBytes": n, "attachmentName": attachmentName, "totalDownloaded": downloadedBytes, "attachmentSize": attachmentSize}).Debug("Downloaded bytes for attachment.")
		}
		log.WithField("attachmentName", attachmentName).Infof("Successfully downloaded large attachment.")
	}

	return nil
}

// SaveAttachmentFromBytes saves an attachment from its base64 encoded content.
func (fh *FileHandler) SaveAttachmentFromBytes(attachmentName, messageID, contentBytes string) error {
	fileName := fmt.Sprintf("%s_%s_%s", sanitizeFileName(attachmentName), messageID, attachmentName)
	filePath := filepath.Join(fh.workspacePath, fileName)

	decodedBytes, err := base64.StdEncoding.DecodeString(contentBytes)
	if err != nil {
		return fmt.Errorf("failed to decode base64 content for attachment %s: %w", attachmentName, err)
	}

	err = os.WriteFile(filePath, decodedBytes, 0644)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to save attachment from bytes", Err: err}
	}

	log.WithField("attachmentName", attachmentName).Infof("Successfully saved attachment from contentBytes.")
	return nil
}

// SaveState saves the RunState to the given file path as JSON.
func (fh *FileHandler) SaveState(state *RunState, stateFilePath string) error {
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
func (fh *FileHandler) LoadState(stateFilePath string) (*RunState, error) {
	content, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &RunState{}, nil // File does not exist, return empty state
		}
		return nil, &apperrors.FileSystemError{Path: stateFilePath, Msg: "failed to read state file", Err: err}
	}

	var state RunState
	err = json.Unmarshal(content, &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal state from JSON: %w", err)
	}
	return &state, nil
}

// sanitizeFileName removes invalid characters from a string to be used as a file name.
func sanitizeFileName(name string) string {
	// Define a regex to match invalid characters for Windows file names
	// \ is for backslash, \x00-\x1f are control characters
	// The other characters are directly included
	invalidChars := regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`) // Corrected escaping for backslash and double quote within regex
	return invalidChars.ReplaceAllString(name, "_")
}

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

	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"

	"o365mbx/apperrors"
	"o365mbx/o365client"
)

type FileHandler struct {
	workspacePath            string
	o365Client               o365client.O365ClientInterface
	largeAttachmentThreshold int // in bytes
	chunkSize                int // in bytes
	bandwidthLimiter         *rate.Limiter
}

func NewFileHandler(workspacePath string, o365Client o365client.O365ClientInterface, largeAttachmentThresholdMB, chunkSizeMB int, bandwidthLimitMBs float64) *FileHandler {
	var limiter *rate.Limiter
	if bandwidthLimitMBs > 0 {
		limit := rate.Limit(bandwidthLimitMBs * 1024 * 1024)
		limiter = rate.NewLimiter(limit, int(limit)) // Burst equal to the limit
		log.WithField("limit", limit).Info("Bandwidth limiting enabled.")
	}

	return &FileHandler{
		workspacePath:            workspacePath,
		o365Client:               o365Client,
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
func (fh *FileHandler) SaveAttachment(ctx context.Context, attachmentName, messageID, downloadURL, accessToken string, attachmentSize int) error { // ctx added
	fileName := fmt.Sprintf("%s_%s_%s", sanitizeFileName(attachmentName), messageID, attachmentName)
	filePath := filepath.Join(fh.workspacePath, fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment", Err: err}
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Warnf("Error closing file %s: %v", filePath, err)
		}
	}()

	if attachmentSize <= fh.largeAttachmentThreshold {
		// Existing direct download logic for smaller attachments
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil) // Pass ctx to request
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
				return fmt.Errorf("failed to download attachment from %s: %w", downloadURL, err)
			}
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body for small attachment: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download small attachment from %s", downloadURL)}
		}

		var reader io.Reader = resp.Body
		if fh.bandwidthLimiter != nil {
			reader = &limitedReader{reader: resp.Body, limiter: fh.bandwidthLimiter, ctx: ctx}
		}

		_, err = io.Copy(out, reader)
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
				return ctx.Err()
			default:
			}

			endByte := downloadedBytes + int64(fh.chunkSize) - 1
			if endByte >= int64(attachmentSize) {
				endByte = int64(attachmentSize) - 1
			}

			req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
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

			// Graph API returns 206 Partial Content for range requests, 200 OK if range is ignored or full file.
			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				if err := resp.Body.Close(); err != nil {
					log.Warnf("Error closing response body for failed large attachment chunk: %v", err)
				}
				return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to download attachment chunk from %s (range %d-%d)", downloadURL, downloadedBytes, endByte)}
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
			log.WithFields(log.Fields{"downloadedBytes": n, "attachmentName": attachmentName, "totalDownloaded": downloadedBytes, "attachmentSize": attachmentSize}).Debug("Downloaded bytes for attachment.")
		}
		log.WithField("attachmentName", attachmentName).Infof("Successfully downloaded large attachment.")
	}

	return nil
}

// SaveAttachmentFromBytes saves an attachment from its base64 encoded content using a streaming approach.
func (fh *FileHandler) SaveAttachmentFromBytes(attachmentName, messageID, contentBytes string) error {
	fileName := fmt.Sprintf("%s_%s_%s", sanitizeFileName(attachmentName), messageID, attachmentName)
	filePath := filepath.Join(fh.workspacePath, fileName)

	out, err := os.Create(filePath)
	if err != nil {
		return &apperrors.FileSystemError{Path: filePath, Msg: "failed to create file for attachment from bytes", Err: err}
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Warnf("Error closing file %s: %v", filePath, err)
		}
	}()

	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(contentBytes))
	_, err = io.Copy(out, decoder)
	if err != nil {
		return fmt.Errorf("failed to decode and save base64 content for attachment %s: %w", attachmentName, err)
	}

	log.WithField("attachmentName", attachmentName).Infof("Successfully saved attachment from contentBytes.")
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

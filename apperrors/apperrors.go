package apperrors

import (
	"errors"
	"fmt"
)

// ErrMissingDeltaLink is returned when an incremental run completes without a delta link.
var ErrMissingDeltaLink = errors.New("API did not provide a delta link on the final page of an incremental sync")

// APIError represents an error from the O365 Graph API.
type APIError struct {
	StatusCode int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status: %d): %s", e.StatusCode, e.Msg)
}

// FileSystemError represents an error related to file system operations.
type FileSystemError struct {
	Path string
	Msg  string
	Err  error
}

func (e *FileSystemError) Error() string {
	return fmt.Sprintf("file system error on path '%s': %s: %v", e.Path, e.Msg, e.Err)
}

func (e *FileSystemError) Unwrap() error {
	return e.Err
}

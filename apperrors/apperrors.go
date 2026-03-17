// Package apperrors defines custom error types for structured error handling
// throughout the application.
//
// OBJECTIVE:
// This package provides a centralized location for application-specific error types.
// These types allow for better error identification, categorization, and reporting
// across different packages (e.g., distinguishing between API errors and FS errors).
//
// CORE FUNCTIONALITY:
// 1. APIError: Represents structured errors from the Microsoft Graph API, including status codes.
// 2. FileSystemError: Wraps local I/O errors with path context and descriptive messages.
// 3. Sentinel Errors: Defines common error conditions like ErrMissingDeltaLink.
package apperrors

import (
	"errors"
	"fmt"
)

// ErrMissingDeltaLink is returned when an incremental run completes without a delta link.
var ErrMissingDeltaLink = errors.New("API did not provide a delta link on the final page of an incremental sync")

// APIError represents a structured error returned by the Microsoft Graph API.
// It includes the HTTP status code to allow for specific error handling logic
// (e.g., retrying on 429 Too Many Requests).
type APIError struct {
	StatusCode int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status: %d): %s", e.StatusCode, e.Msg)
}

// FileSystemError represents an error encountered during local file system operations.
// It includes the path where the error occurred and wraps the underlying OS error.
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

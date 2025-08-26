package apperrors

import "fmt"

// AuthError represents an authentication-related error.
type AuthError struct {
	Msg string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("authentication error: %s", e.Msg)
}

// APIError represents an error returned from the O365 Graph API.
type APIError struct {
	StatusCode int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status %d): %s", e.StatusCode, e.Msg)
}

// FileSystemError represents an error during file system operations.
type FileSystemError struct {
	Path string
	Msg  string
	Err  error // Original error
}

func (e *FileSystemError) Error() string {
	return fmt.Sprintf("file system error on %s: %s (%v)", e.Path, e.Msg, e.Err)
}

func (e *FileSystemError) Unwrap() error {
	return e.Err
}

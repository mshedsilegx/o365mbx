// Package apperrors defines custom error types for structured error handling
// throughout the application.
//
// This file contains unit tests for the apperrors package.
package apperrors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAPIError_Error(t *testing.T) {
	err := &APIError{
		StatusCode: 404,
		Msg:        "Not Found",
	}
	assert.Equal(t, "API error (status: 404): Not Found", err.Error())
}

func TestFileSystemError_Error(t *testing.T) {
	innerErr := errors.New("disk full")
	err := &FileSystemError{
		Path: "/tmp/test",
		Msg:  "failed to write",
		Err:  innerErr,
	}
	assert.Equal(t, "file system error on path '/tmp/test': failed to write: disk full", err.Error())
	assert.Equal(t, innerErr, errors.Unwrap(err))
}

func TestErrMissingDeltaLink(t *testing.T) {
	assert.Equal(t, "API did not provide a delta link on the final page of an incremental sync", ErrMissingDeltaLink.Error())
}

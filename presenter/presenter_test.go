// Package presenter provides formatted output and display logic for health checks
// and message details.
//
// This file contains unit tests for the presenter package.
package presenter_test

import (
	"context"
	"errors"
	"io"
	"o365mbx/mocks"
	"o365mbx/o365client"
	"o365mbx/presenter"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestRunHealthCheckMode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	stats := &o365client.MailboxHealthStats{
		TotalMessages:    10,
		TotalMailboxSize: 1024,
		Folders: []o365client.FolderStats{
			{Name: "Inbox", TotalItems: 10, Size: 1024},
		},
	}

	mockClient.EXPECT().GetMailboxHealthCheck(gomock.Any(), "test@example.com").Return(stats, nil)

	err := presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com", os.Stdout)
	assert.NoError(t, err)

	// Test error case
	mockClient.EXPECT().GetMailboxHealthCheck(gomock.Any(), "test@example.com").Return(nil, errors.New("API Error"))
	err = presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com", os.Stdout)
	assert.Error(t, err)
}

func TestRunHealthCheckMode_MultipleFolders(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	stats := &o365client.MailboxHealthStats{
		TotalMessages:    25,
		TotalMailboxSize: 2048 * 1024,
		Folders: []o365client.FolderStats{
			{Name: "Inbox", TotalItems: 10, Size: 1024 * 1024},
			{Name: "Sent", TotalItems: 15, Size: 1024 * 1024},
		},
	}

	mockClient.EXPECT().GetMailboxHealthCheck(gomock.Any(), "test@example.com").Return(stats, nil)

	err := presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com", os.Stdout)
	assert.NoError(t, err)
}

func TestRunMessageDetailsMode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), "test@example.com", "Inbox", gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			detailsChan <- o365client.MessageDetail{
				From:    "sender@test.com",
				To:      "receiver@test.com",
				Subject: "Test",
			}
			close(detailsChan)
			return nil
		},
	)

	err := presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox", os.Stdout)
	assert.NoError(t, err)

	// Test error case
	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), "test@example.com", "Inbox", gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			close(detailsChan)
			return errors.New("API Error")
		},
	)
	err = presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox", os.Stdout)
	assert.Error(t, err)
}

func TestRunHealthCheckMode_TabwriterError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	stats := &o365client.MailboxHealthStats{
		TotalMessages:    10,
		TotalMailboxSize: 1024,
		Folders: []o365client.FolderStats{
			{Name: "Inbox", TotalItems: 10, Size: 1024},
		},
	}

	mockClient.EXPECT().GetMailboxHealthCheck(gomock.Any(), gomock.Any()).Return(stats, nil)

	// Use a writer that fails to trigger warning paths
	err := presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com", &errorWriter{})
	assert.NoError(t, err) // Should log warning but not return error
}

func TestRunMessageDetailsMode_TabwriterError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			detailsChan <- o365client.MessageDetail{Subject: "Test"}
			close(detailsChan)
			return nil
		},
	)

	err := presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox", &errorWriter{})
	assert.NoError(t, err) // Should log warning but not return error
}

func TestRunMessageDetailsMode_LongSubject(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	longSubject := "This is a very long subject that should be truncated by the presenter to ensure it fits within the table limits and doesn't break the formatting of the output"
	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			detailsChan <- o365client.MessageDetail{
				From:    "sender@test.com",
				To:      "receiver@test.com",
				Subject: longSubject,
			}
			close(detailsChan)
			return nil
		},
	)

	err := presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox", os.Stdout)
	assert.NoError(t, err)
}

func TestRunMessageDetailsMode_ContextCancelled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			close(detailsChan)
			return context.Canceled
		},
	)

	err := presenter.RunMessageDetailsMode(ctx, mockClient, "test@example.com", "Inbox", io.Discard)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "canceled"))
}

type errorWriter struct{}

func (e *errorWriter) Write(p []byte) (n int, err error) {
	return 0, errors.New("write error")
}

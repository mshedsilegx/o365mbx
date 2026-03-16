package presenter_test

import (
	"context"
	"errors"
	"o365mbx/mocks"
	"o365mbx/o365client"
	"o365mbx/presenter"
	"testing"

	"github.com/stretchr/testify/assert"
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

	err := presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com")
	assert.NoError(t, err)

	// Test error case
	mockClient.EXPECT().GetMailboxHealthCheck(gomock.Any(), "test@example.com").Return(nil, errors.New("API Error"))
	err = presenter.RunHealthCheckMode(context.Background(), mockClient, "test@example.com")
	assert.Error(t, err)
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

	err := presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox")
	assert.NoError(t, err)

	// Test error case
	mockClient.EXPECT().GetMessageDetailsForFolder(gomock.Any(), "test@example.com", "Inbox", gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, detailsChan chan<- o365client.MessageDetail) error {
			close(detailsChan)
			return errors.New("API Error")
		},
	)
	err = presenter.RunMessageDetailsMode(context.Background(), mockClient, "test@example.com", "Inbox")
	assert.Error(t, err)
}

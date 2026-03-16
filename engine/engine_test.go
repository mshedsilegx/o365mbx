package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"o365mbx/filehandler"
	"o365mbx/mocks"
	"o365mbx/o365client"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestRunEngine_Basic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-test-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		MaxParallelDownloads: 1,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		ConvertBody:          "none",
	}
	cfg.SetDefaults()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-1"
			msg.SetId(&id)
			subject := "Test Subject"
			msg.SetSubject(&subject)
			hasAtt := false
			msg.SetHasAttachments(&hasAtt)

			body := models.NewItemBody()
			content := "Hello World"
			body.SetContent(&content)
			msg.SetBody(body)

			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), "none", "").Return("Hello World", nil)
	mockHandler.EXPECT().SaveMessage(gomock.Any(), "Hello World", "none").Return(filepath.Join(tmpDir, "msg-1"), nil)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 0).Return(nil)

	err := RunEngine(ctx, cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_Incremental(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-incremental-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	stateFile := filepath.Join(tmpDir, "state.json")
	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "incremental",
		StateFilePath:        stateFile,
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockHandler.EXPECT().LoadState(stateFile).Return(&o365client.RunState{DeltaLink: "old-delta"}, nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			state.DeltaLink = "new-delta"
			return nil
		},
	)

	mockHandler.EXPECT().SaveState(gomock.Any(), stateFile).Return(nil)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 0).Return(nil)

	err := RunEngine(ctx, cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_RouteMode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-route-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "route",
		ProcessedFolder:      "Done",
		ErrorFolder:          "Fail",
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Done").Return("done-id", nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Fail").Return("fail-id", nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-ok"
			msg.SetId(&id)
			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), gomock.Any(), gomock.Any()).Return("body", nil)
	mockHandler.EXPECT().SaveMessage(gomock.Any(), gomock.Any(), gomock.Any()).Return("path", nil)

	mockClient.EXPECT().MoveMessage(gomock.Any(), "user@example.com", "msg-ok", "done-id").Return(nil)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 1, 0).Return(nil)

	err := RunEngine(ctx, cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_GetMessagesError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-error-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockClient.EXPECT().GetMessages(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			return os.ErrPermission
		},
	)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 0).Return(nil)

	err := RunEngine(context.Background(), cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.Error(t, err)
}

func TestRunEngine_SaveMessageError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-save-error-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-fail"
			msg.SetId(&id)
			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), gomock.Any(), gomock.Any()).Return("body", nil)
	mockHandler.EXPECT().SaveMessage(gomock.Any(), gomock.Any(), gomock.Any()).Return("", os.ErrPermission)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 0).Return(nil)

	err := RunEngine(context.Background(), cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_WithAttachments(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-att-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		MaxParallelDownloads: 1,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		ConvertBody:          "none",
	}
	cfg.SetDefaults()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-att"
			msg.SetId(&id)
			hasAtt := true
			msg.SetHasAttachments(&hasAtt)

			body := models.NewItemBody()
			content := "Hello World"
			body.SetContent(&content)
			msg.SetBody(body)

			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), "none", "").Return("Hello World", nil)
	mockHandler.EXPECT().SaveMessage(gomock.Any(), "Hello World", "none").Return(filepath.Join(tmpDir, "msg-att"), nil)

	att := models.NewFileAttachment()
	mockClient.EXPECT().GetMessageAttachments(gomock.Any(), "user@example.com", "msg-att").Return([]models.Attachmentable{att}, nil)

	name := "test.txt"
	att.SetName(&name)

	mockHandler.EXPECT().SaveAttachmentFromBytes(gomock.Any(), gomock.Any(), "msg-att", gomock.Any(), gomock.Any(), gomock.Any()).Return([]filehandler.AttachmentMetadata{}, nil)
	mockHandler.EXPECT().WriteAttachmentsToMetadata(gomock.Any(), gomock.Any()).Return(nil)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 0).Return(nil)

	err := RunEngine(context.Background(), cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_AggregatorLogic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-agg-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "route",
		ProcessedFolder:      "Done",
		ErrorFolder:          "Fail",
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Done").Return("done-id", nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Fail").Return("fail-id", nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-agg"
			msg.SetId(&id)
			hasAtt := true
			msg.SetHasAttachments(&hasAtt)
			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), gomock.Any(), gomock.Any()).Return("body", nil)
	mockHandler.EXPECT().SaveMessage(gomock.Any(), gomock.Any(), gomock.Any()).Return("msg-path", nil)

	att1 := models.NewFileAttachment()
	id1 := "att1"
	att1.SetId(&id1)
	name1 := "att1.txt"
	att1.SetName(&name1)

	att2 := models.NewFileAttachment()
	id2 := "att2"
	att2.SetId(&id2)
	name2 := "att2.txt"
	att2.SetName(&name2)

	mockClient.EXPECT().GetMessageAttachments(gomock.Any(), "user@example.com", "msg-agg").Return([]models.Attachmentable{att1, att2}, nil)

	name1 = "att1.txt"
	att1.SetName(&name1)
	name2 = "att2.txt"
	att2.SetName(&name2)

	mockHandler.EXPECT().SaveAttachmentFromBytes(gomock.Any(), gomock.Any(), "msg-agg", gomock.Any(), gomock.Any(), gomock.Any()).Return([]filehandler.AttachmentMetadata{}, nil).Times(2)
	mockHandler.EXPECT().WriteAttachmentsToMetadata(gomock.Any(), gomock.Any()).Return(nil)

	mockClient.EXPECT().MoveMessage(gomock.Any(), "user@example.com", "msg-agg", "done-id").Return(nil)
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 1, 0).Return(nil)

	err := RunEngine(ctx, cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestRunEngine_AggregatorError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	mockHandler := mocks.NewMockFileHandlerInterface(ctrl)

	tmpDir, _ := os.MkdirTemp("", "o365mbx-agg-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfg := &Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "route",
		ProcessedFolder:      "Done",
		ErrorFolder:          "Fail",
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockHandler.EXPECT().CreateWorkspace().Return(nil)
	mockClient.EXPECT().GetMailboxStats(gomock.Any(), "user@example.com").Return(map[string]int32{"Inbox": 10}, nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Done").Return("done-id", nil)
	mockClient.EXPECT().GetOrCreateFolderIDByName(gomock.Any(), "user@example.com", "Fail").Return("fail-id", nil)

	mockClient.EXPECT().GetMessages(gomock.Any(), "user@example.com", "Inbox", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, mailbox, folder string, state *o365client.RunState, msgChan chan<- models.Messageable) error {
			defer close(msgChan)
			msg := models.NewMessage()
			id := "msg-err"
			msg.SetId(&id)
			msgChan <- msg
			return nil
		},
	)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	mockProcessor.EXPECT().ProcessBody(gomock.Any(), gomock.Any(), gomock.Any()).Return("body", nil)
	// Simulate save error
	mockHandler.EXPECT().SaveMessage(gomock.Any(), gomock.Any(), gomock.Any()).Return("msg-path", os.ErrPermission)

	// Expect SaveError to be called
	mockHandler.EXPECT().SaveError("msg-path", gomock.Any()).Return(nil)

	// Expect move to Fail folder
	mockClient.EXPECT().MoveMessage(gomock.Any(), "user@example.com", "msg-err", "fail-id").Return(nil)

	// Expect status report with 1 error
	mockHandler.EXPECT().SaveStatusReport("user@example.com", gomock.Any(), 0, 1).Return(nil)

	err := RunEngine(ctx, cfg, mockClient, mockProcessor, mockHandler, "token", "1.0.0")
	assert.NoError(t, err)
}

func TestValidateWorkspacePath(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "o365mbx-validate-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"Empty path", "", true},
		{"Relative path", "relative/path", true},
		{"Critical path", "/", true},
		{"Valid path", tmpDir, false},
		{"Non-existent path", filepath.Join(tmpDir, "does-not-exist"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWorkspacePath(tt.path)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

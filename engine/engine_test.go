package engine

import (
	"context"
	"o365mbx/emailprocessor"
	"o365mbx/filehandler"
	"o365mbx/o365client"
	"testing"
	"time"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

type mockO365Client struct {
	o365client.O365ClientInterface
}

func (m *mockO365Client) GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *o365client.RunState, messagesChan chan<- models.Messageable) error {
	msg := models.NewMessage()
	id := "test-id"
	msg.SetId(&id)
	// Subject is nil
	// Body is nil
	// HasAttachments is nil
	messagesChan <- msg
	close(messagesChan)
	return nil
}

func (m *mockO365Client) GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error) {
	return "folder-id", nil
}

func (m *mockO365Client) GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*o365client.MailboxHealthStats, error) {
	return &o365client.MailboxHealthStats{}, nil
}

type mockEmailProcessor struct {
	emailprocessor.EmailProcessorInterface
}

func (m *mockEmailProcessor) ProcessBody(htmlContent, convertBody, chromiumPath string) (interface{}, error) {
	return htmlContent, nil
}

type mockFileHandler struct {
	filehandler.FileHandlerInterface
}

func (m *mockFileHandler) CreateWorkspace() error { return nil }
func (m *mockFileHandler) SaveMessage(message models.Messageable, bodyContent interface{}, convertBody string) (string, error) {
	return "msg-path", nil
}

func TestRunEngine_NilFields(t *testing.T) {
	cfg := &Config{
		MaxParallelDownloads: 1,
		ProcessingMode:       "full",
		WorkspacePath:        "C:\\temp\\test-workspace",
	}
	cfg.SetDefaults()

	client := &mockO365Client{}
	processor := &mockEmailProcessor{}
	handler := &mockFileHandler{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// This should not panic
	RunEngine(ctx, cfg, client, processor, handler, "token", "version")
}

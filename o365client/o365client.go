package o365client

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"o365mbx/apperrors"

	abstractions "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	log "github.com/sirupsen/logrus"
)

// O365ClientInterface defines the interface for O365Client methods used by other packages.
type O365ClientInterface interface {
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error
	GetMailboxStatistics(ctx context.Context, mailboxName string) (int, error)
	MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error
	GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error)
}

type O365Client struct {
	client *msgraphsdk.GraphServiceClient
	rng    *rand.Rand
}

func NewO365Client(accessToken string, timeout time.Duration, maxRetries int, initialBackoffSeconds int, apiCallsPerSecond float64, apiBurst int, rng *rand.Rand) (*O365Client, error) {
	authProvider, err := NewStaticTokenAuthenticationProvider(accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth provider: %w", err)
	}

	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph adapter: %w", err)
	}

	client := msgraphsdk.NewGraphServiceClient(adapter)

	return &O365Client{
		client: client,
		rng:    rng,
	}, nil
}

// GetMessages fetches a list of messages for a given mailbox and streams them to a channel.
func (c *O365Client) GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error {
	defer close(messagesChan)

	headers := abstractions.NewRequestHeaders()
	headers.Add("Prefer", "outlook.body-content-type=\"text\"")

	requestConfiguration := &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
		Headers: headers,
		QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
			Expand:  []string{"attachments"},
			Select:  []string{"id", "subject", "receivedDateTime", "body", "hasAttachments", "from", "toRecipients", "ccRecipients"},
			Orderby: []string{"receivedDateTime asc", "id asc"},
		},
	}

	if !state.LastRunTimestamp.IsZero() {
		timestamp := state.LastRunTimestamp.Format(time.RFC3339Nano)
		filter := fmt.Sprintf("receivedDateTime ge %s", timestamp)
		requestConfiguration.QueryParameters.Filter = &filter
	}

	messages, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(sourceFolderID).Messages().Get(ctx, requestConfiguration)
	if err != nil {
		return handleError(err)
	}

	pageIterator, err := msgraphgocore.NewPageIterator[models.Messageable](messages, c.client.GetAdapter(), models.CreateMessageCollectionResponseFromDiscriminatorValue)
	if err != nil {
		return fmt.Errorf("failed to create page iterator: %w", err)
	}

	err = pageIterator.Iterate(ctx, func(message models.Messageable) bool {
		select {
		case <-ctx.Done():
			log.WithField("error", ctx.Err()).Warn("Context cancelled during message streaming.")
			return false // Stop iteration
		case messagesChan <- message:
			return true // Continue iteration
		}
	})

	if err != nil {
		return fmt.Errorf("error iterating pages: %w", err)
	}

	return nil
}

// GetOrCreateFolderIDByName gets the ID of a folder by name, creating it if it doesn't exist.
func (c *O365Client) GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error) {
	filter := fmt.Sprintf("displayName eq '%s'", folderName)
	requestConfiguration := &users.ItemMailFoldersRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersRequestBuilderGetQueryParameters{
			Filter: &filter,
		},
	}

	folders, err := c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, requestConfiguration)
	if err != nil {
		return "", handleError(err)
	}

	if len(folders.GetValue()) > 0 {
		folderID := *folders.GetValue()[0].GetId()
		log.WithField("folderName", folderName).Info("Found existing folder.")
		return folderID, nil
	}

	log.WithField("folderName", folderName).Info("Folder not found, creating it.")
	newFolder := models.NewMailFolder()
	newFolder.SetDisplayName(&folderName)

	createdFolder, err := c.client.Users().ByUserId(mailboxName).MailFolders().Post(ctx, newFolder, nil)
	if err != nil {
		return "", handleError(err)
	}

	folderID := *createdFolder.GetId()
	log.WithFields(log.Fields{"folderName": folderName, "folderId": folderID}).Info("Successfully created folder.")
	return folderID, nil
}

// GetMailboxStatistics fetches the total message count for a given mailbox's Inbox.
func (c *O365Client) GetMailboxStatistics(ctx context.Context, mailboxName string) (int, error) {
	headers := abstractions.NewRequestHeaders()
	headers.Add("ConsistencyLevel", "eventual")

	requestConfiguration := &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
		Headers: headers,
		QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
			Count: Ptr(true),
		},
	}

	// We need to get the messages collection to get the count.
	// The SDK does not have a separate `.Count()` method on the collection itself.
	// The count is returned as part of the collection response.
	// So we make a GET request for messages with a page size of 1 and the $count parameter.
	requestConfiguration.QueryParameters.Top = Ptr(int32(1))
	result, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId("inbox").Messages().Get(ctx, requestConfiguration)
	if err != nil {
		return 0, handleError(err)
	}

	if result.GetOdataCount() == nil {
		return 0, fmt.Errorf("odata.count not returned in response")
	}

	return int(*result.GetOdataCount()), nil
}

// MoveMessage moves a message to a specified destination folder.
func (c *O365Client) MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error {
	requestBody := users.NewItemMessagesItemMovePostRequestBody()
	requestBody.SetDestinationId(&destinationFolderID)

	_, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Move().Post(ctx, requestBody, nil)
	if err != nil {
		return handleError(err)
	}
	return nil
}

// RunState represents the state of the last successful incremental run.
type RunState struct {
	LastRunTimestamp time.Time `json:"lastRunTimestamp"`
	LastMessageID    string    `json:"lastMessageId"`
}

// handleError converts odataerrors.ODataError to a more specific application error.
func handleError(err error) error {
	if odataErr, ok := err.(*odataerrors.ODataError); ok {
		return &apperrors.APIError{StatusCode: 0, Msg: odataErr.Error()}
	}
	return err
}

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

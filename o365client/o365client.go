package o365client

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"o365mbx/apperrors"

	abstractions "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	log "github.com/sirupsen/logrus"
)

type FolderStats struct {
	Name         string
	TotalItems   int32
	TotalSize    int64
	LastItemDate *time.Time
}

type MailboxHealthStats struct {
	TotalMessages int32
	TotalSize     int64
	Folders       []FolderStats
}

// O365ClientInterface defines the interface for O365Client methods used by other packages.
type O365ClientInterface interface {
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error
	GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error)
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

// GetMessages fetches a list of messages for a given mailbox using delta query and streams them to a channel.
// It updates the state with the new delta link after completion.
func (c *O365Client) GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error {
	defer close(messagesChan)

	var (
		messagesResponse users.ItemMailFoldersItemMessagesDeltaGetResponseable
		err              error
	)

	if state.DeltaLink == "" {
		log.Info("No delta link found. Starting initial synchronization.")
		// By default, the API returns the body in HTML format.
		// We omit the 'Prefer: outlook.body-content-type' header to get the full HTML.
		requestConfiguration := &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetQueryParameters{
				Expand: []string{"attachments"},
				Select: []string{"id", "subject", "receivedDateTime", "body", "hasAttachments", "from", "toRecipients", "ccRecipients"},
			},
		}
		messagesResponse, err = c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(sourceFolderID).Messages().Delta().Get(ctx, requestConfiguration)
	} else {
		log.WithField("deltaLink", state.DeltaLink).Info("Found delta link. Fetching incremental changes.")
		builder := users.NewItemMailFoldersItemMessagesDeltaRequestBuilder(state.DeltaLink, c.client.GetAdapter())
		messagesResponse, err = builder.Get(ctx, nil)
	}

	if err != nil {
		return handleError(err)
	}

	for {
		pageMessages := messagesResponse.GetValue()
		for _, message := range pageMessages {
			select {
			case <-ctx.Done():
				log.Warn("Context cancelled during message streaming.")
				return ctx.Err()
			case messagesChan <- message:
			}
		}

		nextLink := messagesResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			deltaLink := messagesResponse.GetOdataDeltaLink()
			if deltaLink != nil && *deltaLink != "" {
				log.WithField("deltaLink", *deltaLink).Info("Captured new delta link for next run.")
				state.DeltaLink = *deltaLink
			} else {
				log.Warn("Expected a delta link on the final page, but found none.")
			}
			break
		}

		log.Debug("Fetching next page of messages.")
		builder := users.NewItemMailFoldersItemMessagesDeltaRequestBuilder(*nextLink, c.client.GetAdapter())
		messagesResponse, err = builder.Get(ctx, nil)
		if err != nil {
			return handleError(err)
		}
	}

	log.Info("Finished processing all message pages.")
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

func (c *O365Client) GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error) {
	stats := &MailboxHealthStats{
		Folders: make([]FolderStats, 0),
	}

	// 1. Get all mail folders
	foldersResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, nil)
	if err != nil {
		return nil, handleError(err)
	}

	allFolders := foldersResponse.GetValue()
	for _, folder := range allFolders {
		folderStat := FolderStats{
			Name:       *folder.GetDisplayName(),
			TotalItems: *folder.GetTotalItemCount(),
			TotalSize:  *folder.GetSizeInBytes(),
		}

		// 2. If it's the Inbox, get the last message date
		if strings.ToLower(*folder.GetDisplayName()) == "inbox" {
			// Query for the most recent message
			lastMessage, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(*folder.GetId()).Messages().Get(ctx, &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
				QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
					Top:    Ptr(int32(1)),
					Select: []string{"receivedDateTime"},
					Orderby: []string{"receivedDateTime desc"},
				},
			})
			if err != nil {
				log.WithField("folder", folderStat.Name).Warnf("Could not fetch last message date: %v", err)
			} else if len(lastMessage.GetValue()) > 0 {
				folderStat.LastItemDate = lastMessage.GetValue()[0].GetReceivedDateTime()
			}
		}

		stats.Folders = append(stats.Folders, folderStat)
		stats.TotalMessages += folderStat.TotalItems
		stats.TotalSize += folderStat.TotalSize
	}

	return stats, nil
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
	DeltaLink string `json:"deltaLink"`
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

package o365client

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"sort"
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
	Size         int64
	LastItemDate *time.Time
}

type MailboxHealthStats struct {
	TotalMessages    int32
	TotalMailboxSize int64
	Folders          []FolderStats
}

type MessageDetail struct {
	From                 string
	To                   string
	Date                 time.Time
	Subject              string
	AttachmentCount      int
	AttachmentsTotalSize int64
}

// O365ClientInterface defines the interface for O365Client methods used by other packages.
type O365ClientInterface interface {
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error
	GetMessageAttachments(ctx context.Context, mailboxName, messageID string) ([]models.Attachmentable, error)
	GetAttachmentContent(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error)
	GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error)
	GetMessageDetailsForFolder(ctx context.Context, mailboxName, folderName string, detailsChan chan<- MessageDetail) error
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

	isIncrementalRun := state.DeltaLink != ""

	var (
		messagesResponse users.ItemMailFoldersItemMessagesDeltaGetResponseable
		err              error
	)

	if !isIncrementalRun {
		log.Info("No delta link found. Starting initial synchronization.")
		// We no longer expand attachments here to reduce memory usage.
		// Attachments will be fetched on a per-message basis.
		requestConfiguration := &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetQueryParameters{
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
				if isIncrementalRun {
					log.Error("Critical error: A delta link was expected on the final page of an incremental sync, but was not provided by the API.")
					return apperrors.ErrMissingDeltaLink
				}
				log.Warn("Expected a delta link on the final page, but found none. This is not critical for a full sync, but state for the next incremental run cannot be saved.")
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

// GetMessageAttachments fetches all attachments for a specific message.
func (c *O365Client) GetMessageAttachments(ctx context.Context, mailboxName, messageID string) ([]models.Attachmentable, error) {
	log.WithFields(log.Fields{"messageID": messageID}).Debug("Fetching attachments for message.")

	// When fetching attachments, we select only the essential properties to reduce the initial payload size.
	// The actual content will be downloaded on-demand.
	requestConfiguration := &users.ItemMessagesItemAttachmentsRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMessagesItemAttachmentsRequestBuilderGetQueryParameters{
			Select: []string{"id", "name", "contentType", "size", "isInline"},
		},
	}

	response, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Attachments().Get(ctx, requestConfiguration)
	if err != nil {
		return nil, handleError(err)
	}

	attachments := response.GetValue()
	log.WithFields(log.Fields{"messageID": messageID, "count": len(attachments)}).Info("Successfully fetched attachments.")
	return attachments, nil
}

// GetAttachmentContent streams the raw content of a single attachment.
// This is used for downloading large file attachments or the MIME content of item attachments.
func (c *O365Client) GetAttachmentContent(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error) {
	log.WithFields(log.Fields{"messageID": messageID, "attachmentID": attachmentID}).Debug("Requesting attachment content stream.")

	// The Go SDK does not have a fluent method for /$value on attachments. The templating engine in Kiota
	// appears to have issues with special characters in path parameters, so we will
	// build the URL string directly and parse it to avoid the templating mechanism.
	requestAdapter := c.client.GetAdapter()

	// Manually construct the URL string with properly escaped path parameters.
	// Note: url.PathEscape does not escape '@', so we use url.QueryEscape for the mailbox.
	rawURL := fmt.Sprintf(
		"%s/users/%s/messages/%s/attachments/%s/$value",
		requestAdapter.GetBaseUrl(),
		url.QueryEscape(mailboxName),
		url.PathEscape(messageID),
		url.PathEscape(attachmentID),
	)

	// Parse the raw URL to create a URL object.
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse attachment content URL: %w", err)
	}

	// Create a new request information object and set the URI directly.
	requestInfo := abstractions.NewRequestInformation()
	requestInfo.Method = abstractions.GET
	requestInfo.SetUri(*parsedURL)

	// Execute the request and return the stream.
	// The error mapping is set to nil because we will handle the error after the call.
	response, err := requestAdapter.SendPrimitive(ctx, requestInfo, "io.ReadCloser", nil)
	if err != nil {
		return nil, handleError(fmt.Errorf("failed to get attachment content stream: %w", err))
	}

	if response == nil {
		return nil, fmt.Errorf("received nil response for attachment content stream")
	}

	return response.(io.ReadCloser), nil
}

// GetOrCreateFolderIDByName gets the ID of a folder by name, creating it if it doesn't exist.
func (c *O365Client) GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error) {
	filter := fmt.Sprintf("displayName eq '%s'", folderName)
	requestConfiguration := &users.ItemMailFoldersRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersRequestBuilderGetQueryParameters{
			Filter: &filter,
			Top:    Ptr(int32(1)), // We only need one result
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

	// If folder is not found by exact match, try a case-insensitive search on client side
	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return "", fmt.Errorf("could not get all folders for case-insensitive search: %w", err)
	}

	for _, folder := range allFolders {
		if strings.EqualFold(*folder.GetDisplayName(), folderName) {
			folderID := *folder.GetId()
			log.WithField("folderName", folderName).Info("Found existing folder (case-insensitive).")
			return folderID, nil
		}
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

// GetAllFolders retrieves all mail folders for a mailbox, handling pagination.
func (c *O365Client) GetAllFolders(ctx context.Context, mailboxName string) ([]models.MailFolderable, error) {
	allFolders := make([]models.MailFolderable, 0)
	requestConfiguration := &users.ItemMailFoldersRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersRequestBuilderGetQueryParameters{
			Select: []string{"id", "displayName", "totalItemCount"},
		},
	}
	foldersResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, requestConfiguration)
	if err != nil {
		return nil, handleError(err)
	}

	for {
		pageFolders := foldersResponse.GetValue()
		allFolders = append(allFolders, pageFolders...)

		nextLink := foldersResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			break
		}

		log.Debug("Fetching next page of mail folders.")
		builder := users.NewItemMailFoldersRequestBuilder(*nextLink, c.client.GetAdapter())
		foldersResponse, err = builder.Get(ctx, nil)
		if err != nil {
			return nil, handleError(err)
		}
	}
	return allFolders, nil
}

func (c *O365Client) GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error) {
	stats := &MailboxHealthStats{
		Folders: make([]FolderStats, 0),
	}

	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return nil, fmt.Errorf("failed to get all folders: %w", err)
	}

	// Sort folders by name
	sort.Slice(allFolders, func(i, j int) bool {
		return strings.ToLower(*allFolders[i].GetDisplayName()) < strings.ToLower(*allFolders[j].GetDisplayName())
	})

	var totalMailboxSize int64
	for _, folder := range allFolders {
		var folderSize int64
		// The 'sizeInBytes' property is not a first-class property in the Go model.
		// We must retrieve it from the additional data bag.
		additionalData := folder.GetAdditionalData()
		if size, ok := additionalData["sizeInBytes"]; ok {
			if sizeInt64, ok := size.(*int64); ok && sizeInt64 != nil {
				folderSize = *sizeInt64
			}
		}

		folderStat := FolderStats{
			Name:       *folder.GetDisplayName(),
			TotalItems: *folder.GetTotalItemCount(),
			Size:       folderSize,
		}

		// 2. If it's the Inbox, get the last message date
		if strings.ToLower(*folder.GetDisplayName()) == "inbox" {
			// Query for the most recent message
			lastMessage, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(*folder.GetId()).Messages().Get(ctx, &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
				QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
					Top:     Ptr(int32(1)),
					Select:  []string{"receivedDateTime"},
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
		totalMailboxSize += folderSize
	}

	stats.TotalMailboxSize = totalMailboxSize

	return stats, nil
}

func (c *O365Client) GetMessageDetailsForFolder(ctx context.Context, mailboxName, folderName string, detailsChan chan<- MessageDetail) error {
	defer close(detailsChan)

	folderID, err := c.GetOrCreateFolderIDByName(ctx, mailboxName, folderName)
	if err != nil {
		return fmt.Errorf("could not find or create folder '%s': %w", folderName, err)
	}

	requestConfig := &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
			Select: []string{
				"from", "toRecipients", "receivedDateTime", "subject", "hasAttachments",
			},
			Expand: []string{"attachments($select=size)"},
			Top:    Ptr(int32(100)),
		},
	}

	messagesResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(folderID).Messages().Get(ctx, requestConfig)
	if err != nil {
		return handleError(err)
	}

	for {
		pageMessages := messagesResponse.GetValue()
		for _, msg := range pageMessages {
			var totalAttachmentSize int64
			attachmentCount := 0
			if msg.GetHasAttachments() != nil && *msg.GetHasAttachments() {
				attachments := msg.GetAttachments()
				attachmentCount = len(attachments)
				for _, att := range attachments {
					if size := att.GetSize(); size != nil {
						totalAttachmentSize += int64(*size)
					}
				}
			}

			var toString string
			if toRecipients := msg.GetToRecipients(); len(toRecipients) > 0 {
				toEmails := make([]string, len(toRecipients))
				for i, r := range toRecipients {
					if r.GetEmailAddress() != nil && r.GetEmailAddress().GetAddress() != nil {
						toEmails[i] = *r.GetEmailAddress().GetAddress()
					}
				}
				toString = strings.Join(toEmails, ";")
			}

			var fromString string
			if from := msg.GetFrom(); from != nil && from.GetEmailAddress() != nil && from.GetEmailAddress().GetAddress() != nil {
				fromString = *from.GetEmailAddress().GetAddress()
			}

			detail := MessageDetail{
				From:                 fromString,
				To:                   toString,
				Date:                 *msg.GetReceivedDateTime(),
				Subject:              *msg.GetSubject(),
				AttachmentCount:      attachmentCount,
				AttachmentsTotalSize: totalAttachmentSize,
			}

			select {
			case <-ctx.Done():
				log.Warn("Context cancelled during message detail streaming.")
				return ctx.Err()
			case detailsChan <- detail:
			}
		}

		nextLink := messagesResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			break
		}

		log.Debug("Fetching next page of messages for details.")
		builder := users.NewItemMailFoldersItemMessagesRequestBuilder(*nextLink, c.client.GetAdapter())
		messagesResponse, err = builder.Get(ctx, nil)
		if err != nil {
			return handleError(err)
		}
	}

	return nil
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
// extracting the HTTP status code if available
func handleError(err error) error {
	if odataErr, ok := err.(*odataerrors.ODataError); ok {
		var statusCode int
		// The ODataError is expected to implement an interface that provides the status code.
		// We perform a type assertion to safely access it
		if apierror, ok := err.(interface{ GetResponseStatusCode() int }); ok {
			statusCode = apierror.GetResponseStatusCode()
		}

		// The main error message from the API is what we want to show
		// odataErr.error() provides a good summary
		return &apperrors.APIError{StatusCode: statusCode, Msg: odataErr.Error()}
	}
	return err
}

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

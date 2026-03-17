// Package o365client handles all interactions with the Microsoft Graph API,
// including message retrieval, attachment streaming, and mailbox management.
//
// OBJECTIVE:
// This package serves as the "E" (Extract) in the ETL pipeline. It encapsulates
// all communication with the Microsoft Graph API, providing a high-level interface
// for the rest of the application to interact with O365 mailboxes.
//
// CORE FUNCTIONALITY:
//  1. Authentication: Manages static token-based authentication for Graph API requests.
//  2. Message Retrieval: Fetches messages using delta queries for efficient incremental sync.
//  3. Attachment Streaming: Provides methods to download attachments, including raw
//     MIME streams for item attachments (.msg/.eml).
//  4. Mailbox Management: Retrieves folder structures, item counts, and storage statistics.
//  5. Resilience: Implements API rate limiting and basic error mapping for Graph API responses.
package o365client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"o365mbx/apperrors"
	"o365mbx/utils"

	kiota "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	log "github.com/sirupsen/logrus"
)

// FolderStats encapsulates basic statistics for a specific mail folder.
type FolderStats struct {
	Name         string
	TotalItems   int32
	Size         int64
	LastItemDate *time.Time
}

// MailboxHealthStats provides an overview of the mailbox's health and structure.
type MailboxHealthStats struct {
	TotalMessages    int32
	TotalMailboxSize int64
	Folders          []FolderStats
}

// MessageDetail contains granular information about a specific email message.
type MessageDetail struct {
	From                 string
	To                   string
	Date                 time.Time
	Subject              string
	AttachmentCount      int
	AttachmentsTotalSize int64
}

// O365ClientInterface defines the interface for O365Client methods used by other packages.
//
//go:generate mockgen -destination=../mocks/mock_o365client.go -package=mocks o365mbx/o365client O365ClientInterface
type O365ClientInterface interface {
	GetMailboxStats(ctx context.Context, mailboxName string) (map[string]int32, error)
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error
	GetMessageAttachments(ctx context.Context, mailboxName, messageID string) ([]models.Attachmentable, error)
	GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error)
	GetMessageDetailsForFolder(ctx context.Context, mailboxName, folderName string, detailsChan chan<- MessageDetail) error
	MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error
	GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error)
	GetAttachmentRawStream(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error)
}

// O365Client implements the O365ClientInterface using the Microsoft Graph SDK.
type O365Client struct {
	client *msgraphsdk.GraphServiceClient
	rng    *rand.Rand
}

// --- Initialization ---

// NewO365Client initializes a new Graph API client with the provided access token and random source.
// It sets up the authentication provider and request adapter for the Microsoft Graph SDK.
func NewO365Client(accessToken string, rng *rand.Rand) (*O365Client, error) {
	authProvider, err := NewStaticTokenAuthenticationProvider(accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth provider: %w", err)
	}

	// For production, we use the default GraphRequestAdapter
	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph adapter: %w", err)
	}

	return NewO365ClientWithAdapter(adapter, rng), nil
}

// NewO365ClientWithAdapter allows injecting a custom adapter for testing (e.g., with httpmock)
func NewO365ClientWithAdapter(adapter kiota.RequestAdapter, rng *rand.Rand) *O365Client {
	return &O365Client{
		client: msgraphsdk.NewGraphServiceClient(adapter),
		rng:    rng,
	}
}

// --- Message Retrieval ---

// GetMailboxStats returns a map of folder names to their total item counts.
func (c *O365Client) GetMailboxStats(ctx context.Context, mailboxName string) (map[string]int32, error) {
	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return nil, fmt.Errorf("failed to get all folders for stats: %w", err)
	}

	stats := make(map[string]int32)
	for _, folder := range allFolders {
		if folder.GetDisplayName() != nil && folder.GetTotalItemCount() != nil {
			stats[*folder.GetDisplayName()] = *folder.GetTotalItemCount()
		}
	}
	return stats, nil
}

// GetMessages fetches a list of messages for a given mailbox using delta query and streams them to a channel.
// It uses Microsoft Graph's delta query capability to efficiently retrieve only changes
// since the last run. It updates the provided RunState with the new delta link.
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
		log.WithFields(log.Fields{
			"mailboxName":    mailboxName,
			"sourceFolderID": sourceFolderID,
		}).Debug("Requesting message delta from Graph API.")
		messagesResponse, err = c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(sourceFolderID).Messages().Delta().Get(ctx, requestConfiguration)
	} else {
		log.WithField("deltaLink", state.DeltaLink).Info("Found delta link. Fetching incremental changes.")
		builder := users.NewItemMailFoldersItemMessagesDeltaRequestBuilder(state.DeltaLink, c.client.GetAdapter())
		messagesResponse, err = builder.Get(ctx, nil)
	}

	if err != nil {
		return handleError(err)
	}

	if messagesResponse == nil {
		log.Warn("Received nil response from O365 API for messages.")
		return nil
	}

	for {
		pageMessages := messagesResponse.GetValue()
		log.WithField("count", len(pageMessages)).Info("Fetched page of messages.")
		for i, message := range pageMessages {
			id := "unknown"
			if message.GetId() != nil {
				id = *message.GetId()
			}
			log.WithFields(log.Fields{"index": i, "id": id}).Debug("Streaming message to channel.")
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

	response, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Attachments().Get(ctx, nil)
	if err != nil {
		return nil, handleError(err)
	}

	attachments := response.GetValue()
	log.WithFields(log.Fields{"messageID": messageID, "count": len(attachments)}).Info("Successfully fetched attachments.")
	return attachments, nil
}

// --- Folder Management ---

// GetOrCreateFolderIDByName gets the ID of a folder by name, creating it if it doesn't exist.
// It first attempts an exact match using an OData filter. If that fails, it performs
// a case-insensitive search across all folders in the mailbox.
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

	if folders != nil && folders.GetValue() != nil && len(folders.GetValue()) > 0 {
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
			Select: []string{"id", "displayName", "totalItemCount", "sizeInBytes"},
		},
	}
	foldersResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, requestConfiguration)
	if err != nil {
		return nil, handleError(err)
	}

	if foldersResponse == nil {
		return allFolders, nil
	}

	for {
		pageFolders := foldersResponse.GetValue()
		if pageFolders != nil {
			allFolders = append(allFolders, pageFolders...)
		}

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

// --- Health and Diagnostics ---

// GetMailboxHealthCheck retrieves aggregate statistics and folder details for the mailbox.
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
			folderSize = parseFolderSize(size)
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

// GetMessageDetailsForFolder streams granular metadata for all messages in a
// specified folder to a channel. It expands attachment information to calculate
// total attachment sizes for each message.
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
		for _, msg := range messagesResponse.GetValue() {
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
				Date:                 utils.TimeValue(msg.GetReceivedDateTime(), time.Time{}),
				Subject:              utils.StringValue(msg.GetSubject(), "(no subject)"),
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

// MoveMessage relocates a message to a different folder within the same mailbox.
// This is typically used in 'route' mode to move processed messages to
// successful or error folders.
func (c *O365Client) MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error {
	requestBody := users.NewItemMessagesItemMovePostRequestBody()
	requestBody.SetDestinationId(&destinationFolderID)

	_, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Move().Post(ctx, requestBody, nil)
	if err != nil {
		return handleError(err)
	}
	return nil
}

// --- Attachment and Stream Handling ---

// GetAttachmentRawStream fetches the raw stream of an attachment using the $value endpoint.
func (c *O365Client) GetAttachmentRawStream(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error) {
	log.WithFields(log.Fields{
		"messageID":    messageID,
		"attachmentID": attachmentID,
	}).Debug("Fetching raw attachment stream ($value).")

	// 2. Build the URL string
	baseUrl := c.client.GetAdapter().GetBaseUrl()
	urlPathStr := fmt.Sprintf("%s/users/%s/messages/%s/attachments/%s/$value",
		baseUrl, mailboxName, messageID, attachmentID)

	// 3. PARSE the string into a *url.URL object
	parsedUrl, err := url.Parse(urlPathStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse attachment URL: %w", err)
	}

	// 4. Send the request via the Adapter or native HTTP
	// We use native http.Client here because Kiota's SendPrimitive often fails
	// with "no factory registered" for message/rfc822 or other binary streams
	// when using the $value endpoint.
	// Use an optimized transport for connection pooling in production.
	transport := http.DefaultTransport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := t.Clone()
		cloned.MaxIdleConnsPerHost = 100 // Allow more concurrent idle connections per host
		transport = cloned
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second, // Base safety timeout
	}

	req, err := http.NewRequestWithContext(ctx, "GET", parsedUrl.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, handleError(err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("graph api returned status %d for $value endpoint", resp.StatusCode)
	}

	return resp.Body, nil
}

// RunState represents the state of the last successful incremental run.
type RunState struct {
	DeltaLink string `json:"deltaLink"`
}

// handleError converts odataerrors.ODataError to a more specific application error.
// extracting the HTTP status code if available
func handleError(err error) error {
	if err == nil {
		return nil
	}

	var odataErr *odataerrors.ODataError
	if errors.As(err, &odataErr) {
		statusCode := 0
		type responseStatusCode interface {
			GetResponseStatusCode() int
		}
		var rsc responseStatusCode
		if errors.As(err, &rsc) {
			statusCode = rsc.GetResponseStatusCode()
		}

		return &apperrors.APIError{StatusCode: statusCode, Msg: odataErr.Error()}
	}
	return err
}

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

// parseFolderSize extracts an int64 from various types that Kiota might use for sizeInBytes.
func parseFolderSize(size interface{}) int64 {
	switch v := size.(type) {
	case *int64:
		if v != nil {
			return *v
		}
	case *int32:
		if v != nil {
			return int64(*v)
		}
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case *float64:
		if v != nil {
			return int64(*v)
		}
	}
	return 0
}

// ExportParseFolderSize is an exported version of parseFolderSize for testing.
func ExportParseFolderSize(size interface{}) int64 {
	return parseFolderSize(size)
}

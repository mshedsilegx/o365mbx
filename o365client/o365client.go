package o365client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"

	"o365mbx/apperrors"
)

const graphAPIBaseURL = "https://graph.microsoft.com/v1.0"

// O365ClientInterface defines the interface for O365Client methods used by other packages.
type O365ClientInterface interface {
	DoRequestWithRetry(req *http.Request) (*http.Response, error)
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- Message) error
	GetMailboxStatistics(ctx context.Context, mailboxName string) (int, error)
	MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error
	GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error)
}

type O365Client struct {
	accessToken    string
	httpClient     *http.Client
	maxRetries     int
	initialBackoff time.Duration
	limiter        *rate.Limiter
	rng            *rand.Rand // Local random number generator
}

func NewO365Client(accessToken string, timeout time.Duration, maxRetries int, initialBackoffSeconds int, apiCallsPerSecond float64, apiBurst int, rng *rand.Rand) *O365Client {
	return &O365Client{
		accessToken:    accessToken,
		httpClient:     &http.Client{Timeout: timeout},
		maxRetries:     maxRetries,
		initialBackoff: time.Duration(initialBackoffSeconds) * time.Second,
		limiter:        rate.NewLimiter(rate.Limit(apiCallsPerSecond), apiBurst),
		rng:            rng, // Store the local random number generator
	}
}

// doRequestWithRetry executes an HTTP request with retry logic and exponential backoff.
func (c *O365Client) DoRequestWithRetry(req *http.Request) (*http.Response, error) {
	maxRetries := c.maxRetries
	initialBackoff := c.initialBackoff
	var lastErr error // Store the last error encountered

	for i := 0; i < maxRetries; i++ {
		// Apply client-side rate limiting
		if err := c.limiter.Wait(req.Context()); err != nil {
			// If context is cancelled during wait, return the context error
			return nil, fmt.Errorf("rate limiter wait failed: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err == nil {
			// Check for retryable status codes
			if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode <= 599) {
				log.WithFields(log.Fields{"statusCode": resp.StatusCode, "attempt": i + 1, "maxRetries": maxRetries}).Warn("Retrying due to status code.")
				if err := resp.Body.Close(); err != nil {
					log.Warnf("Failed to close response body: %v", err)
				}
				lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode) // Store the status code as an error
				// Continue to next iteration for retry
			} else {
				return resp, nil // Success or non-retryable error, return response
			}
		} else {
			lastErr = err // Store the network error
			log.WithFields(log.Fields{"error": err, "attempt": i + 1, "maxRetries": maxRetries}).Warn("Retrying due to network error.")
			// Continue to next iteration for retry
		}

		// Check for context cancellation before next retry
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}

		// Calculate backoff with jitter
		backoff := initialBackoff * time.Duration(1<<uint(i))
		jitter := time.Duration(c.rng.Intn(1000)) * time.Millisecond // Add up to 1 second jitter
		time.Sleep(backoff + jitter)
	}

	// If loop finishes, all retries failed. Return a new error wrapping the last one.
	if lastErr != nil {
		return nil, fmt.Errorf("failed after %d retries, last error: %w", maxRetries, lastErr)
	}
	return nil, fmt.Errorf("failed after %d retries (no specific last error encountered)", maxRetries)
}

// GetMessages fetches a list of messages for a given mailbox and streams them to a channel.
func (c *O365Client) GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- Message) error {
	defer close(messagesChan) // Close the channel when the function finishes

	baseURL, err := url.Parse(fmt.Sprintf("%s/users/%s/mailFolders/%s/messages", graphAPIBaseURL, mailboxName, sourceFolderID))
	if err != nil {
		return fmt.Errorf("failed to parse base URL: %w", err)
	}

	params := url.Values{}
	// Always sort by receivedDateTime and then by id for deterministic ordering.
	params.Add("$orderby", "receivedDateTime asc, id asc")
	// Use $expand to fetch attachments along with the message data in a single call.
	params.Add("$expand", "attachments")
	// Use $select to specify exact fields, including To, From, and CC recipients.
	params.Add("$select", "id,subject,receivedDateTime,body,hasAttachments,from,toRecipients,ccRecipients")

	// If a timestamp is available from a previous run, use it to filter.
	if !state.LastRunTimestamp.IsZero() {
		timestamp := state.LastRunTimestamp.Format(time.RFC3339Nano)
		// Use 'ge' (greater than or equal) to include items with the same timestamp.
		// The calling function will be responsible for skipping the one with the matching ID.
		params.Add("$filter", fmt.Sprintf("receivedDateTime ge %s", timestamp))
	}
	baseURL.RawQuery = params.Encode()

	nextLink := baseURL.String()

	for nextLink != "" {
		select {
		case <-ctx.Done():
			log.WithField("error", ctx.Err()).Warn("Context cancelled during message fetching.")
			return ctx.Err() // Return context cancellation error
		default:
			// Continue
		}

		req, err := http.NewRequestWithContext(ctx, "GET", nextLink, nil) // Pass ctx to request
		if err != nil {
			return fmt.Errorf("failed to create HTTP request for messages: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)

		resp, err := c.DoRequestWithRetry(req)
		if err != nil {
			return fmt.Errorf("failed to fetch messages: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			errorBody, _ := io.ReadAll(resp.Body)
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body after reading error: %v", err)
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return &apperrors.AuthError{Msg: "invalid or expired access token"}
			}
			return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: string(errorBody)}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body after read failure: %v", err)
			}
			return fmt.Errorf("failed to read response body for messages: %w", err)
		}
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body in GetMessages: %v", err)
		}

		var response struct {
			Value         []Message `json:"value"`
			OdataNextLink string    `json:"@odata.nextLink"`
		}
		err = json.Unmarshal(body, &response)
		if err != nil {
			return fmt.Errorf("failed to unmarshal messages response: %w", err)
		}

		for _, msg := range response.Value {
			select {
			case messagesChan <- msg:
			case <-ctx.Done():
				log.WithField("error", ctx.Err()).Warn("Context cancelled during message streaming.")
				return ctx.Err()
			}
		}
		nextLink = response.OdataNextLink
	}

	return nil
}

type MailFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func (c *O365Client) GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error) {
	// First, try to get the folder by name
	url := fmt.Sprintf("%s/users/%s/mailFolders?$filter=displayName eq '%s'", graphAPIBaseURL, mailboxName, url.QueryEscape(folderName))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request to get folder by name: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return "", fmt.Errorf("failed to get folder by name: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close response body for folder check: %v", err)
		}
		if err != nil {
			return "", fmt.Errorf("failed to read response body for folder: %w", err)
		}

		var folderResponse struct {
			Value []MailFolder `json:"value"`
		}
		if err := json.Unmarshal(body, &folderResponse); err != nil {
			return "", fmt.Errorf("failed to unmarshal folder response: %w", err)
		}

		if len(folderResponse.Value) > 0 {
			log.WithField("folderName", folderName).Info("Found existing folder.")
			return folderResponse.Value[0].ID, nil
		}
	} else {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close error response body for folder check: %v", err)
		}
	}

	// If not found, create it
	log.WithField("folderName", folderName).Info("Folder not found, creating it.")
	return c.createFolder(ctx, mailboxName, folderName)
}

func (c *O365Client) createFolder(ctx context.Context, mailboxName, folderName string) (string, error) {
	url := fmt.Sprintf("%s/users/%s/mailFolders", graphAPIBaseURL, mailboxName)
	folderData := MailFolder{DisplayName: folderName}
	body, err := json.Marshal(folderData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal folder data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request to create folder: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return "", fmt.Errorf("failed to create folder: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close response body for create folder: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		errorBody, _ := io.ReadAll(resp.Body)
		return "", &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to create folder: %s", string(errorBody))}
	}

	var newFolder MailFolder
	if err := json.NewDecoder(resp.Body).Decode(&newFolder); err != nil {
		return "", fmt.Errorf("failed to unmarshal new folder response: %w", err)
	}

	log.WithFields(log.Fields{"folderName": folderName, "folderId": newFolder.ID}).Info("Successfully created folder.")
	return newFolder.ID, nil
}

// GetMailboxStatistics fetches the total message count for a given mailbox's Inbox.
func (c *O365Client) GetMailboxStatistics(ctx context.Context, mailboxName string) (int, error) {
	// URL to get the message count of the Inbox folder
	url := fmt.Sprintf("%s/users/%s/mailFolders/inbox/messages?$count=true", graphAPIBaseURL, mailboxName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create HTTP request for mailbox statistics: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("ConsistencyLevel", "eventual") // Required for $count

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch mailbox statistics: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body in GetMailboxStatistics: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		errorBody, _ := io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body after reading error in GetMailboxStatistics: %v", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return 0, &apperrors.AuthError{Msg: "invalid or expired access token"}
		}
		return 0, &apperrors.APIError{StatusCode: resp.StatusCode, Msg: string(errorBody)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body for mailbox statistics: %w", err)
	}

	var response struct {
		OdataCount int `json:"@odata.count"`
	}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal mailbox statistics response: %w", err)
	}

	return response.OdataCount, nil
}

// EmailAddress represents the email address of a user.
type EmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Recipient represents a sender or receiver of a message.
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

// Message represents an email message from O365
type Message struct {
	ID               string    `json:"id"`
	Subject          string    `json:"subject"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
	Body             struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	HasAttachments bool         `json:"hasAttachments"`
	Attachments    []Attachment `json:"attachments"`
	From           Recipient    `json:"from"`
	ToRecipients   []Recipient  `json:"toRecipients"`
	CcRecipients   []Recipient  `json:"ccRecipients"`
}

// Attachment represents an attachment from O365
type Attachment struct {
	ID           string `json:"id"`
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	Size         int    `json:"size"`
	ContentType  string `json:"contentType"`
	IsInline     bool   `json:"isInline"`
	DownloadURL  string `json:"@microsoft.graph.downloadUrl"`
	ContentBytes string `json:"contentBytes"`
}

// RunState represents the state of the last successful incremental run.
type RunState struct {
	LastRunTimestamp time.Time `json:"lastRunTimestamp"`
	LastMessageID    string    `json:"lastMessageId"`
}

// MoveMessage moves a message to a specified destination folder.
func (c *O365Client) MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error {
	url := fmt.Sprintf("%s/users/%s/messages/%s/move", graphAPIBaseURL, mailboxName, messageID)

	body := []byte(fmt.Sprintf(`{"destinationId": "%s"}`, destinationFolderID))
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request for moving message: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return fmt.Errorf("failed to move message %s: %w", messageID, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close response body for move message: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		errorBody, _ := io.ReadAll(resp.Body)
		return &apperrors.APIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("failed to move message: %s", string(errorBody))}
	}

	return nil
}

package o365client

import (
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
	GetMessages(ctx context.Context, mailboxName string, state *RunState) ([]Message, error)
	GetAttachments(ctx context.Context, mailboxName, messageID string) ([]Attachment, error)
	GetAttachmentDetails(ctx context.Context, mailboxName, messageID, attachmentID string) (*Attachment, error)
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

// GetMessages fetches a list of messages for a given mailbox.
func (c *O365Client) GetMessages(ctx context.Context, mailboxName string, state *RunState) ([]Message, error) { // ctx added
	var allMessages []Message

	baseURL, err := url.Parse(fmt.Sprintf("%s/users/%s/messages", graphAPIBaseURL, mailboxName))
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}

	params := url.Values{}
	// Always sort by receivedDateTime and then by id for deterministic ordering.
	params.Add("$orderby", "receivedDateTime asc, id asc")

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
			return nil, ctx.Err() // Return context cancellation error
		default:
			// Continue
		}

		req, err := http.NewRequestWithContext(ctx, "GET", nextLink, nil) // Pass ctx to request
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request for messages: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)

		resp, err := c.DoRequestWithRetry(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch messages: %w", err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body in GetMessages: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			errorBody, _ := io.ReadAll(resp.Body) // Read body for detailed error
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Error closing response body after reading error: %v", err)
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, &apperrors.AuthError{Msg: "invalid or expired access token"}
			}
			return nil, &apperrors.APIError{StatusCode: resp.StatusCode, Msg: string(errorBody)}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body for attachments: %w", err)
		}

		var response struct {
			Value         []Message `json:"value"`
			OdataNextLink string    `json:"@odata.nextLink"`
		}
		err = json.Unmarshal(body, &response)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal messages response: %w", err)
		}

		allMessages = append(allMessages, response.Value...)
		nextLink = response.OdataNextLink
	}

	return allMessages, nil
}

// GetAttachments fetches attachment metadata for a given message.
func (c *O365Client) GetAttachments(ctx context.Context, mailboxName, messageID string) ([]Attachment, error) { // ctx added
	url := fmt.Sprintf("%s/users/%s/messages/%s/attachments", graphAPIBaseURL, mailboxName, messageID)

	select {
	case <-ctx.Done():
		log.WithField("error", ctx.Err()).Warn("Context cancelled during attachment fetching.")
		return nil, ctx.Err() // Return context cancellation error
	default:
		// Continue
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil) // Pass ctx to request
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for attachments: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch attachments: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body in GetAttachments: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		errorBody, _ := io.ReadAll(resp.Body) // Read body for detailed error
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body after reading error in GetAttachments: %v", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, &apperrors.AuthError{Msg: "invalid or expired access token"}
		}
		return nil, &apperrors.APIError{StatusCode: resp.StatusCode, Msg: string(errorBody)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for attachments: %w", err)
	}

	var response struct {
		Value []Attachment `json:"value"`
	}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal attachments response: %w", err)
	}

	return response.Value, nil
}

// GetAttachmentDetails fetches the full details for a single attachment.
func (c *O365Client) GetAttachmentDetails(ctx context.Context, mailboxName, messageID, attachmentID string) (*Attachment, error) {
	url := fmt.Sprintf("%s/users/%s/messages/%s/attachments/%s", graphAPIBaseURL, mailboxName, messageID, attachmentID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for attachment details: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.DoRequestWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch attachment details: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body in GetAttachmentDetails: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		errorBody, _ := io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Error closing response body after reading error in GetAttachmentDetails: %v", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, &apperrors.AuthError{Msg: "invalid or expired access token"}
		}
		return nil, &apperrors.APIError{StatusCode: resp.StatusCode, Msg: string(errorBody)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for attachment details: %w", err)
	}

	var attachment Attachment
	err = json.Unmarshal(body, &attachment)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal attachment details response: %w", err)
	}

	return &attachment, nil
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

// Message represents an email message from O365
type Message struct {
	ID               string    `json:"id"`
	Subject          string    `json:"subject"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
	Body             struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	HasAttachments bool `json:"hasAttachments"`
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

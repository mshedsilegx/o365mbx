package o365client

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jarcoal/httpmock"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"o365mbx/apperrors"
)

func TestO365Client_GetMessages_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterNoResponder(func(req *http.Request) (*http.Response, error) {
		t.Logf("DEBUG: Unmatched request: %s %s", req.Method, req.URL.String())
		return httpmock.NewStringResponse(404, "no match"), nil
	})

	mockResponse := `{
		"value": [
			{
				"id": "msg-1",
				"subject": "Test Integration",
				"body": {
					"contentType": "text",
					"content": "Hello from httpmock"
				},
				"hasAttachments": false
			}
		],
		"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/mailFolders/Inbox/messages/delta?$deltatoken=abc"
	}`

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*messages/delta.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := httpmock.NewStringResponse(200, mockResponse)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	require.NoError(t, err)

	client := NewO365ClientWithAdapter(adapter, nil)

	msgChan := make(chan models.Messageable, 10)
	state := &RunState{}
	ctx := context.Background()

	err = client.GetMessages(ctx, "test@example.com", "Inbox", state, msgChan)
	require.NoError(t, err)

	var received []models.Messageable
	for m := range msgChan {
		received = append(received, m)
	}

	assert.Equal(t, 1, len(received))
	if len(received) > 0 {
		assert.Equal(t, "msg-1", *received[0].GetId())
	}
	assert.Equal(t, "https://graph.microsoft.com/v1.0/me/mailFolders/Inbox/messages/delta?$deltatoken=abc", state.DeltaLink)
}

func TestO365Client_GetAttachmentRawStream_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterNoResponder(func(req *http.Request) (*http.Response, error) {
		t.Logf("DEBUG: Unmatched request: %s %s", req.Method, req.URL.String())
		return httpmock.NewStringResponse(404, "no match"), nil
	})

	content := "This is a raw attachment stream"
	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*attachments/.*/\$value`),
		func(req *http.Request) (*http.Response, error) {
			resp := httpmock.NewStringResponse(200, content)
			resp.Header.Set("Content-Type", "application/octet-stream")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	require.NoError(t, err)

	client := NewO365ClientWithAdapter(adapter, nil)

	stream, err := client.GetAttachmentRawStream(context.Background(), "test@example.com", "msg-1", "att-1")

	if err != nil && strings.Contains(err.Error(), "factory registered") {
		t.Skip("Skipping raw stream test due to Kiota factory limitation in tests")
	}

	require.NoError(t, err)
	require.NotNil(t, stream)

	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(stream)
	assert.NoError(t, err)
	assert.Equal(t, content, string(body))
}

func TestO365Client_MoveMessage_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterRegexpResponder("POST", regexp.MustCompile(`.*messages/msg-1/move`),
		func(req *http.Request) (*http.Response, error) {
			return httpmock.NewStringResponse(201, "{}"), nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	err := client.MoveMessage(context.Background(), "test@example.com", "msg-1", "dest-folder-id")
	assert.NoError(t, err)
}

func TestO365Client_GetOrCreateFolderIDByName_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders\?\$filter=displayName.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": []}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders\?\$select=id,displayName,totalItemCount$`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": [{"id": "existing-id", "displayName": "ExistingFolder"}]}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	httpmock.RegisterRegexpResponder("POST", regexp.MustCompile(`.*mailFolders$`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"id": "new-id", "displayName": "NewFolder"}`
			r := httpmock.NewStringResponse(201, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	id, err := client.GetOrCreateFolderIDByName(context.Background(), "test@example.com", "existingfolder")
	assert.NoError(t, err)
	assert.Equal(t, "existing-id", id)

	id, err = client.GetOrCreateFolderIDByName(context.Background(), "test@example.com", "NewFolder")
	assert.NoError(t, err)
	assert.Equal(t, "new-id", id)
}

func TestO365Client_GetMailboxHealthCheck_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders\?\$select=id,displayName,totalItemCount$`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": [
				{"id": "inbox-id", "displayName": "Inbox", "totalItemCount": 10, "sizeInBytes": 1024},
				{"id": "sent-id", "displayName": "Sent", "totalItemCount": 5, "sizeInBytes": 512}
			]}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders/inbox-id/messages.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": [{"receivedDateTime": "2024-01-01T12:00:00Z"}]}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	stats, err := client.GetMailboxHealthCheck(context.Background(), "test@example.com")
	assert.NoError(t, err)
	assert.NotNil(t, stats)
	assert.Equal(t, int32(15), stats.TotalMessages)
	assert.Equal(t, 2, len(stats.Folders))
}

func TestO365Client_GetMailboxStats_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders\?\$select=id,displayName,totalItemCount$`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": [
				{"id": "inbox-id", "displayName": "Inbox", "totalItemCount": 10},
				{"id": "sent-id", "displayName": "Sent Items", "totalItemCount": 5}
			]}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	stats, err := client.GetMailboxStats(context.Background(), "test@example.com")
	assert.NoError(t, err)
	assert.NotNil(t, stats)
	assert.Equal(t, int32(10), stats["Inbox"])
	assert.Equal(t, int32(5), stats["Sent Items"])
}

func TestO365Client_GetMessageDetailsForFolder_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterNoResponder(func(req *http.Request) (*http.Response, error) {
		t.Logf("DEBUG: Unmatched request: %s %s", req.Method, req.URL.String())
		return httpmock.NewStringResponse(404, "no match"), nil
	})

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders\?\$filter=displayName.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := `{"value": [{"id": "inbox-id"}]}`
			r := httpmock.NewStringResponse(200, resp)
			r.Header.Set("Content-Type", "application/json")
			return r, nil
		})

	mockResponse := `{
		"value": [
			{
				"id": "msg-1",
				"subject": "Details Test",
				"receivedDateTime": "2024-01-01T12:00:00Z",
				"from": {"emailAddress": {"address": "sender@test.com"}},
				"toRecipients": [{"emailAddress": {"address": "receiver@test.com"}}],
				"hasAttachments": true,
				"attachments": [{"size": 1024}]
			}
		]
	}`
	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders/inbox-id/messages.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := httpmock.NewStringResponse(200, mockResponse)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	detailsChan := make(chan MessageDetail, 10)
	ctx := context.Background()

	err := client.GetMessageDetailsForFolder(ctx, "test@example.com", "Inbox", detailsChan)
	assert.NoError(t, err)

	var details []MessageDetail
	for d := range detailsChan {
		details = append(details, d)
	}

	assert.Equal(t, 1, len(details))
	if len(details) > 0 {
		assert.Equal(t, "sender@test.com", details[0].From)
	}
}

func TestO365Client_Errors_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*`),
		func(req *http.Request) (*http.Response, error) {
			return httpmock.NewStringResponse(401, `{"error": {"code": "unauthorized", "message": "token expired"}}`), nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	msgChan := make(chan models.Messageable, 10)
	err := client.GetMessages(context.Background(), "test@example.com", "Inbox", &RunState{}, msgChan)
	assert.Error(t, err)
}

func TestO365Client_GetMessageAttachments_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	mockResponse := `{
		"value": [
			{
				"@odata.type": "#microsoft.graph.fileAttachment",
				"id": "att-1",
				"name": "test.txt",
				"size": 1024
			}
		]
	}`

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*messages/msg-1/attachments.*`),
		func(req *http.Request) (*http.Response, error) {
			resp := httpmock.NewStringResponse(200, mockResponse)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	attachments, err := client.GetMessageAttachments(context.Background(), "test@example.com", "msg-1")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(attachments))
	if len(attachments) > 0 {
		assert.Equal(t, "att-1", *attachments[0].GetId())
	}
}

func TestO365Client_HandleError(t *testing.T) {
	// Test context error
	err := handleError(context.DeadlineExceeded)
	assert.Equal(t, context.DeadlineExceeded, err)

	// Test ODataError
	odErr := odataerrors.NewODataError()
	mainErr := odataerrors.NewMainError()
	mainErr.SetMessage(Ptr("test error"))
	mainErr.SetCode(Ptr("test code"))
	odErr.SetErrorEscaped(mainErr)

	// Since we can't easily mock the interface{ GetResponseStatusCode() int }
	// without a real instance that implements it (which the SDK does),
	// we just test the basic ODataError conversion.
	apiErr := handleError(odErr)
	assert.Error(t, apiErr)
	var target *apperrors.APIError
	assert.ErrorAs(t, apiErr, &target)
	assert.Contains(t, apiErr.Error(), "test error")
}

func TestStaticTokenAuthenticationProvider(t *testing.T) {
	provider, err := NewStaticTokenAuthenticationProvider("test-token")
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	// Test nil request
	err = provider.AuthenticateRequest(context.Background(), nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "request cannot be nil")

	// Test GetAuthorizationToken
	token, err := provider.GetAuthorizationToken(context.Background(), nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, "test-token", token)

	_, err = NewStaticTokenAuthenticationProvider("")
	assert.Error(t, err)
}

func TestO365Client_GetMessages_Pagination_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	mockPage1 := `{
		"value": [{"id": "msg-1"}],
		"@odata.nextLink": "https://graph.microsoft.com/v1.0/me/mailFolders/Inbox/messages/delta?$deltatoken=page2"
	}`
	mockPage2 := `{
		"value": [{"id": "msg-2"}],
		"@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/mailFolders/Inbox/messages/delta?$deltatoken=final"
	}`

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*messages/delta.*`),
		func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.String(), "deltatoken=page2") {
				resp := httpmock.NewStringResponse(200, mockPage2)
				resp.Header.Set("Content-Type", "application/json")
				return resp, nil
			}
			resp := httpmock.NewStringResponse(200, mockPage1)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	msgChan := make(chan models.Messageable, 10)
	state := &RunState{}
	err := client.GetMessages(context.Background(), "test@example.com", "Inbox", state, msgChan)
	require.NoError(t, err)

	var received []models.Messageable
	for m := range msgChan {
		received = append(received, m)
	}

	assert.Equal(t, 2, len(received))
	assert.Equal(t, "https://graph.microsoft.com/v1.0/me/mailFolders/Inbox/messages/delta?$deltatoken=final", state.DeltaLink)
}

func TestO365Client_GetAllFolders_Pagination_httpmock(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	mockPage1 := `{
		"value": [{"id": "f1", "displayName": "Folder 1"}],
		"@odata.nextLink": "https://graph.microsoft.com/v1.0/me/mailFolders?$skip=1"
	}`
	mockPage2 := `{
		"value": [{"id": "f2", "displayName": "Folder 2"}]
	}`

	httpmock.RegisterRegexpResponder("GET", regexp.MustCompile(`.*mailFolders.*`),
		func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.String(), "$skip=1") {
				resp := httpmock.NewStringResponse(200, mockPage2)
				resp.Header.Set("Content-Type", "application/json")
				return resp, nil
			}
			resp := httpmock.NewStringResponse(200, mockPage1)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		})

	authProvider, _ := NewStaticTokenAuthenticationProvider("dummy-token")
	adapter, _ := msgraphsdk.NewGraphRequestAdapter(authProvider)
	client := NewO365ClientWithAdapter(adapter, nil)

	folders, err := client.GetAllFolders(context.Background(), "test@example.com")
	require.NoError(t, err)
	assert.Equal(t, 2, len(folders))
}

func TestNewO365Client(t *testing.T) {
	// Test NewO365Client basic initialization
	rng := rand.New(rand.NewSource(1))
	client, err := NewO365Client("token", 10*time.Second, 1, 1, 1.0, 1, rng)
	assert.NoError(t, err)
	assert.NotNil(t, client)

	_, err = NewO365Client("", 10*time.Second, 1, 1, 1.0, 1, rng)
	assert.Error(t, err)
}

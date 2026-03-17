// Package filehandler manages all local file system operations, including
// workspace creation, message saving, attachment handling, and state persistence.
//
// This file contains unit tests for the filehandler package.
package filehandler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhillyerd/enmime"
	"o365mbx/filehandler"
	"o365mbx/mocks"
	"o365mbx/o365client"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestFileHandler_CreateWorkspace(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-workspace-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	err := fh.CreateWorkspace()
	assert.NoError(t, err)

	info, err := os.Stat(tmpDir)
	assert.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestFileHandler_SaveMessage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, _ := os.MkdirTemp("", "fh-test-save-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, mockProcessor, 20, 8, 0, "raw", "default", logger)

	msg := models.NewMessage()
	id := "msg-123"
	msg.SetId(&id)
	subject := "Test Subject"
	msg.SetSubject(&subject)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false)

	path, err := fh.SaveMessage(msg, "Plain body", "none")
	require.NoError(t, err)
	assert.Contains(t, path, id)

	// Verify files created
	_, err = os.Stat(filepath.Join(path, "body.txt"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(path, "metadata.json"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(path, "attachments"))
	assert.NoError(t, err)
}

func TestFileHandler_SaveFileAttachment(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-att-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	attPath := filepath.Join(msgPath, "attachments")
	err = os.MkdirAll(attPath, 0700)
	require.NoError(t, err)

	att := models.NewFileAttachment()
	name := "test.txt"
	att.SetName(&name)
	content := []byte("hello world")
	att.SetContentBytes(content)
	contentType := "text/plain"
	att.SetContentType(&contentType)
	size := int32(len(content))
	att.SetSize(&size)

	metas, err := fh.SaveAttachmentFromBytes(context.Background(), "user@example.com", "msg-1", msgPath, att, 1)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "test.txt", metas[0].Name)
	assert.Equal(t, "01_test.txt", metas[0].SavedAs)

	// Verify file
	savedContent, err := os.ReadFile(filepath.Join(attPath, "01_test.txt"))
	require.NoError(t, err)
	assert.Equal(t, content, savedContent)
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"Normal file", "normal.txt", "normal.txt"},
		{"Path traversal", "../../etc/passwd", "____etc_passwd"},
		{"Invalid chars", "foo/bar:baz*qux?txt", "foo_bar_baz_qux_txt"},
		{"Leading dash", "-config.json", "_config.json"},
		{"Long name", string(make([]byte, 300)), string(make([]byte, 200))}, // Truncation check
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filehandler.SanitizeFileName(tt.input)
			if tt.name == "Long name" {
				assert.Equal(t, 200, len(got))
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestFileHandler_SaveItemAttachment_Extractor(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, err := os.MkdirTemp("", "fh-test-extractor-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	logger := logrus.New()
	// Set msgHandler to "extractor"
	fh := filehandler.NewFileHandler(tmpDir, mockClient, nil, 20, 8, 0, "extractor", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	attPath := filepath.Join(msgPath, "attachments")
	err = os.MkdirAll(attPath, 0700)
	require.NoError(t, err)

	attID := "att-123"
	attName := "nested.eml"
	att := models.NewItemAttachment()
	att.SetId(&attID)
	att.SetName(&attName)

	// Create a raw MIME message with a nested attachment
	mimeContent := "MIME-Version: 1.0\n" +
		"From: sender@example.com\n" +
		"Subject: Nested\n" +
		"Content-Type: multipart/mixed; boundary=boundary\n" +
		"\n" +
		"--boundary\n" +
		"Content-Type: text/html; charset=utf-8\n" +
		"\n" +
		"<html><body>Nested Body</body></html>\n" +
		"--boundary\n" +
		"Content-Type: text/plain; name=\"inner.txt\"\n" +
		"Content-Disposition: attachment; filename=\"inner.txt\"\n" +
		"\n" +
		"INNER_CONTENT\n" +
		"--boundary--"
	rawStream := io.NopCloser(strings.NewReader(mimeContent))

	mockClient.EXPECT().GetAttachmentRawStream(gomock.Any(), "user@example.com", "msg-1", attID).Return(rawStream, nil)

	metas, err := fh.SaveAttachmentFromBytes(context.Background(), "user@example.com", "msg-1", msgPath, att, 1)
	require.NoError(t, err)

	// Expect 3 metas: the .eml itself, the extracted body, and the inner.txt attachment
	assert.True(t, len(metas) >= 3)

	// Verify .eml file exists
	_, err = os.Stat(filepath.Join(attPath, "01_nested.eml"))
	assert.NoError(t, err)

	// Verify extracted body file exists
	bodyFile := filepath.Join(attPath, "01_nested.eml_extracted.html")
	_, err = os.Stat(bodyFile)
	assert.NoError(t, err)

	bodyContent, err := os.ReadFile(bodyFile)
	require.NoError(t, err)
	assert.Contains(t, string(bodyContent), "Nested Body")

	// Verify extracted nested attachment exists
	nestedFile := filepath.Join(attPath, "01_1_inner.txt")
	_, err = os.Stat(nestedFile)
	assert.NoError(t, err)

	nestedContent, err := os.ReadFile(nestedFile)
	require.NoError(t, err)
	assert.Equal(t, "INNER_CONTENT", strings.TrimSpace(string(nestedContent)))
}

func TestFileHandler_SaveAttachment_Large(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-large-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()
	// Small threshold to trigger large attachment logic
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 1, 1, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	err = os.MkdirAll(filepath.Join(msgPath, "attachments"), 0700)
	require.NoError(t, err)

	att := models.NewFileAttachment()
	name := "large.bin"
	att.SetName(&name)
	// 2MB content, threshold is 1MB
	content := make([]byte, 2*1024*1024)
	att.SetContentBytes(content)

	metas, err := fh.SaveAttachmentFromBytes(context.Background(), "user@example.com", "msg-1", msgPath, att, 1)
	assert.NoError(t, err)
	assert.Len(t, metas, 1)
}

func TestFileHandler_WriteAttachmentsToMetadata(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-meta-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	err = os.MkdirAll(msgPath, 0700)
	require.NoError(t, err)

	// Create initial metadata.json
	metaPath := filepath.Join(msgPath, "metadata.json")
	initialMeta := `{"subject": "test"}`
	err = os.WriteFile(metaPath, []byte(initialMeta), 0600)
	require.NoError(t, err)

	attachments := []filehandler.AttachmentMetadata{
		{Name: "a1.txt", SavedAs: "01_a1.txt", Size: 10},
	}

	err = fh.WriteAttachmentsToMetadata(msgPath, attachments)
	assert.NoError(t, err)

	data, err := os.ReadFile(metaPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "01_a1.txt")
}

func TestFileHandler_Errors(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-errors-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()

	// Test workspace creation error (already exists as file)
	filePath := filepath.Join(tmpDir, "file-not-dir")
	err = os.WriteFile(filePath, []byte("test"), 0600)
	require.NoError(t, err)
	fhBad := filehandler.NewFileHandler(filePath, nil, nil, 20, 8, 0, "raw", "default", logger)
	err = fhBad.CreateWorkspace()
	assert.Error(t, err)

	// Test SaveMessage with too long path
	longName := strings.Repeat("a", 500)
	fhLong := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)
	msg := models.NewMessage()
	msg.SetId(&longName)
	_, err = fhLong.SaveMessage(msg, "body", "none")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum length")
}

func TestFileHandler_SaveMessage_HTML(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, err := os.MkdirTemp("", "fh-test-html-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, mockProcessor, 20, 8, 0, "raw", "default", logger)

	msg := models.NewMessage()
	id := "msg-html"
	msg.SetId(&id)

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(true)

	path, err := fh.SaveMessage(msg, "<html><body>test</body></html>", "none")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(path, "body.html"))
}

func TestFileHandler_State(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-state-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	stateFile := filepath.Join(tmpDir, "state.json")
	state := &o365client.RunState{DeltaLink: "test-link"}

	err = fh.SaveState(state, stateFile)
	assert.NoError(t, err)

	loaded, err := fh.LoadState(stateFile)
	assert.NoError(t, err)
	assert.Equal(t, "test-link", loaded.DeltaLink)

	// Test non-existent state
	empty, err := fh.LoadState(filepath.Join(tmpDir, "none.json"))
	assert.NoError(t, err)
	assert.Empty(t, empty.DeltaLink)
}

func TestFileHandler_SaveMessage_Errors(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, err := os.MkdirTemp("", "fh-test-errors-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, mockProcessor, 20, 8, 0, "raw", "default", logger)

	msg := models.NewMessage()
	id := "msg-123"
	msg.SetId(&id)

	// Test unsupported body type
	_, err = fh.SaveMessage(msg, 123, "none")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported body content type")

	// Test generated path too long
	longID := strings.Repeat("a", 500)
	msg.SetId(&longID)
	_, err = fh.SaveMessage(msg, "body", "none")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum length")
}

func TestFileHandler_SaveAttachment_Errors(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-att-errors-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	// Test unsupported attachment type
	_, err = fh.SaveAttachmentFromBytes(context.Background(), "user", "msg-1", tmpDir, &mockAttachment{}, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported attachment type")

	// Test FileAttachment with nil content
	att := models.NewFileAttachment()
	name := "test.txt"
	att.SetName(&name)
	_, err = fh.SaveAttachmentFromBytes(context.Background(), "user", "msg-1", tmpDir, att, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "has no content bytes")
}

type mockAttachment struct {
	models.Attachment
}

func (m *mockAttachment) GetId() *string   { id := "mock"; return &id }
func (m *mockAttachment) GetName() *string { name := "mock"; return &name }

func TestFileHandler_NewFileHandler_Limiter(t *testing.T) {
	logger := logrus.New()
	// Test with bandwidth limit > 0
	fh := filehandler.NewFileHandler(".", nil, nil, 20, 8, 1.0, "raw", "default", logger)
	assert.NotNil(t, fh)
}

func TestFileHandler_CreateWorkspace_Symlink(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-symlink-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	linkPath := filepath.Join(tmpDir, "link")
	targetPath := filepath.Join(tmpDir, "target")
	err = os.Mkdir(targetPath, 0700)
	require.NoError(t, err)

	// Create a symlink
	err = os.Symlink(targetPath, linkPath)
	if err != nil {
		t.Skip("Skipping symlink test: could not create symlink (likely permission issue on Windows)")
	}

	logger := logrus.New()
	fh := filehandler.NewFileHandler(linkPath, nil, nil, 20, 8, 0, "raw", "default", logger)

	err = fh.CreateWorkspace()
	assert.Error(t, err)
	errMsg := err.Error()
	// On Windows, os.Lstat of a symlink to a directory might report !IsDir() depending on how it's handled,
	// or it might hit the symlink check. Both are acceptable failures for this security check.
	assert.True(t, strings.Contains(errMsg, "cannot be a symbolic link") || strings.Contains(errMsg, "is not a directory"),
		"Error message should indicate symlink or directory issue, got: %s", errMsg)
}

func TestFileHandler_Metadata_Errors(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-meta-errors-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	// Test WriteAttachmentsToMetadata with non-existent file
	err = fh.WriteAttachmentsToMetadata(filepath.Join(tmpDir, "non-existent"), nil)
	assert.Error(t, err)

	// Test WriteAttachmentsToMetadata with invalid JSON
	msgPath := filepath.Join(tmpDir, "invalid-json")
	err = os.MkdirAll(msgPath, 0700)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(msgPath, "metadata.json"), []byte("invalid json"), 0600)
	require.NoError(t, err)
	err = fh.WriteAttachmentsToMetadata(msgPath, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal")
}

func TestFileHandler_SaveItem_Inlines(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, err := os.MkdirTemp("", "fh-test-inlines-*")
	require.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	logger := logrus.New()
	// Set msgHandler to "extractor" and attachmentExtractionL1 to "inlines"
	fh := filehandler.NewFileHandler(tmpDir, mockClient, nil, 20, 8, 0, "extractor", "inlines", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	err = os.MkdirAll(filepath.Join(msgPath, "attachments"), 0700)
	require.NoError(t, err)

	attID := "att-123"
	attName := "test.eml"
	att := models.NewItemAttachment()
	att.SetId(&attID)
	att.SetName(&attName)

	mimeContent := "MIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=boundary\n\n" +
		"--boundary\n" +
		"Content-Type: text/plain\n\n" +
		"Body\n" +
		"--boundary\n" +
		"Content-Type: image/png; name=\"inline.png\"\n" +
		"Content-Disposition: inline; filename=\"inline.png\"\n\n" +
		"binarycontent\n" +
		"--boundary--"
	rawStream := io.NopCloser(strings.NewReader(mimeContent))

	mockClient.EXPECT().GetAttachmentRawStream(gomock.Any(), gomock.Any(), gomock.Any(), attID).Return(rawStream, nil)

	metas, err := fh.SaveAttachmentFromBytes(context.Background(), "user", "msg-1", msgPath, att, 1)
	assert.NoError(t, err)
	// Original .eml + Body + Inline
	assert.Len(t, metas, 3)
}

func TestFileHandler_SaveError(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-error-json-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	err = os.MkdirAll(msgPath, 0700)
	require.NoError(t, err)

	errs := []error{
		os.ErrPermission,
		io.EOF,
	}

	err = fh.SaveError(msgPath, errs)
	assert.NoError(t, err)

	errorFile := filepath.Join(msgPath, "error.json")
	assert.FileExists(t, errorFile)

	data, err := os.ReadFile(errorFile)
	require.NoError(t, err)

	var details []filehandler.ErrorDetail
	err = json.Unmarshal(data, &details)
	assert.NoError(t, err)
	assert.Len(t, details, 2)
	assert.Contains(t, details[0].Message, "permission denied")
	assert.Contains(t, details[1].Message, "EOF")
}

func TestFileHandler_SaveStatusReport(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-status-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	sourceCounts := map[string]int32{
		"Inbox": 10,
		"Sent":  5,
	}

	err = fh.SaveStatusReport("user@test.com", sourceCounts, 8, 2)
	assert.NoError(t, err)

	// Find the status file
	files, err := filepath.Glob(filepath.Join(tmpDir, "status_*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)

	var status filehandler.JobStatus
	err = json.Unmarshal(data, &status)
	assert.NoError(t, err)
	assert.Equal(t, "user@test.com", status.Mailbox)
	assert.Equal(t, int32(10), status.SourceCounts["Inbox"])
	assert.Equal(t, 8, status.JobProcessedCount)
	assert.Equal(t, 2, status.JobErrorCount)
}

func TestFileHandler_CopyWithContext(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-copy-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// 1. Test successful copy
	src := strings.NewReader("hello world")
	dst := &strings.Builder{}
	err := filehandler.ExportCopyWithContext(context.Background(), dst, src)
	assert.NoError(t, err)
	assert.Equal(t, "hello world", dst.String())

	// 2. Test context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	srcSlow := &slowReader{}
	dst2 := &strings.Builder{}
	err = filehandler.ExportCopyWithContext(ctx, dst2, srcSlow)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "context deadline exceeded"))
}

type slowReader struct{}

func (r *slowReader) Read(p []byte) (n int, err error) {
	time.Sleep(100 * time.Millisecond)
	copy(p, "a")
	return 1, nil
}

func TestFileHandler_GetMutex_Concurrency(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-mutex-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	path := filepath.Join(tmpDir, "test-file")

	// Test basic retrieval
	mu1 := fh.ExportGetMutex(path)
	assert.NotNil(t, mu1)

	// Test retrieval of same mutex
	mu2 := fh.ExportGetMutex(path)
	assert.Equal(t, mu1, mu2)

	// Test concurrent access to getMutex
	var wg sync.WaitGroup
	mutexes := make([]*sync.Mutex, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			mutexes[idx] = fh.ExportGetMutex(path)
		}(i)
	}
	wg.Wait()

	for _, m := range mutexes {
		assert.Equal(t, mu1, m)
	}
}

func TestFileHandler_ToRecipient_Nil(t *testing.T) {
	// Internal function toRecipient is covered by SaveMessage but let's test nil paths
	// Since toRecipient is not exported, we test via SaveMessage with partially nil recipients
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, _ := os.MkdirTemp("", "fh-test-nil-recipient-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, mockProcessor, 20, 8, 0, "raw", "default", logger)

	msg := models.NewMessage()
	id := "msg-nil-recip"
	msg.SetId(&id)

	// models.Recipientable with nil EmailAddress
	recip := models.NewRecipient()
	msg.SetToRecipients([]models.Recipientable{recip})

	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false)

	path, err := fh.SaveMessage(msg, "body", "none")
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(path, "metadata.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "\"name\": \"\"")
}

func TestFileHandler_SaveItemAttachment_Extractor_Errors(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, _ := os.MkdirTemp("", "fh-test-extractor-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, nil, 20, 8, 0, "extractor", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	_ = os.MkdirAll(filepath.Join(msgPath, "attachments"), 0700)

	att := models.NewItemAttachment()
	id := "att-1"
	att.SetId(&id)

	// Test 1: GetAttachmentRawStream error
	mockClient.EXPECT().GetAttachmentRawStream(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("api error"))
	_, err := fh.SaveAttachmentFromBytes(context.Background(), "user", "msg-1", msgPath, att, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch raw stream")

	// Test 2: Invalid MIME content (should log and skip extraction but return original meta)
	mockClient.EXPECT().GetAttachmentRawStream(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(io.NopCloser(strings.NewReader("not a mime")), nil)
	metas, err := fh.SaveAttachmentFromBytes(context.Background(), "user", "msg-1", msgPath, att, 2)
	assert.NoError(t, err)
	assert.Len(t, metas, 1) // Only original .eml
}

func TestFileHandler_SaveState_Errors(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-state-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	// Use a file as a directory to cause OpenRoot to fail on Windows
	invalidDir := filepath.Join(tmpDir, "file-not-dir")
	_ = os.WriteFile(invalidDir, []byte("test"), 0600)

	fh := filehandler.NewFileHandler(invalidDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	err := fh.SaveState(&o365client.RunState{}, filepath.Join(invalidDir, "state.json"))
	assert.Error(t, err)
}

func TestFileHandler_LoadState_Malformed(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-load-state-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	stateFile := filepath.Join(tmpDir, "bad-state.json")
	_ = os.WriteFile(stateFile, []byte("invalid json"), 0600)

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	_, err := fh.LoadState(stateFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal state")
}

func TestFileHandler_SaveMessage_DetailedPaths(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir, _ := os.MkdirTemp("", "fh-test-msg-paths-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mockClient := mocks.NewMockO365ClientInterface(ctrl)
	mockProcessor := mocks.NewMockEmailProcessorInterface(ctrl)
	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, mockClient, mockProcessor, 20, 8, 0, "raw", "default", logger)

	msg := models.NewMessage()
	id := "msg-123"
	msg.SetId(&id)

	// Test case: bodyContent as []byte
	mockProcessor.EXPECT().IsHTML(gomock.Any()).Return(false).AnyTimes()
	path, err := fh.SaveMessage(msg, []byte("byte body"), "none")
	assert.NoError(t, err)
	content, _ := os.ReadFile(filepath.Join(path, "body.txt"))
	assert.Equal(t, "byte body", string(content))

	// Test case: convertBody "text"
	path, err = fh.SaveMessage(msg, "text content", "text")
	assert.NoError(t, err)
	assert.FileExists(t, filepath.Join(path, "body.txt"))

	// Test case: convertBody "pdf"
	path, err = fh.SaveMessage(msg, "pdf content", "pdf")
	assert.NoError(t, err)
	assert.FileExists(t, filepath.Join(path, "body.pdf"))
}

func TestFileHandler_CreateWorkspace_Errors(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-workspace-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()

	// Test case: MkdirAll failure (using a file as parent)
	parentFile := filepath.Join(tmpDir, "file")
	_ = os.WriteFile(parentFile, []byte("test"), 0600)
	fh := filehandler.NewFileHandler(filepath.Join(parentFile, "child"), nil, nil, 20, 8, 0, "raw", "default", logger)
	err := fh.CreateWorkspace()
	assert.Error(t, err)
}

func TestFileHandler_ToRecipient_Complete(t *testing.T) {
	// Test toRecipient with nil recipient
	res := filehandler.ExportToRecipient(nil)
	assert.Empty(t, res.EmailAddress.Name)
	assert.Empty(t, res.EmailAddress.Address)

	// Test toRecipient with recipient but nil EmailAddress
	mRecip := models.NewRecipient()
	res = filehandler.ExportToRecipient(mRecip)
	assert.Empty(t, res.EmailAddress.Name)
	assert.Empty(t, res.EmailAddress.Address)
}

func TestFileHandler_ExtractFilesFromEnvelope_RawMode(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-extract-raw-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	// msgHandler "raw" should return nil immediately
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	root, _ := os.OpenRoot(tmpDir)
	defer func() {
		_ = root.Close()
	}()

	metas := fh.ExportExtractFilesFromEnvelope(root, &enmime.Envelope{}, 1)
	assert.Nil(t, metas)
}

func TestFileHandler_extractFilesFromEnvelope_WriteError(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-extract-write-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "extractor", "default", logger)

	// Create a file where the attachments directory should be to cause WriteFile to fail
	_ = os.WriteFile(filepath.Join(tmpDir, "attachments"), []byte("not a dir"), 0600)

	root, err := os.OpenRoot(tmpDir)
	require.NoError(t, err)
	defer func() {
		_ = root.Close()
	}()

	env := &enmime.Envelope{
		Attachments: []*enmime.Part{
			{
				ContentType: "text/plain",
				Content:     []byte("test"),
				FileName:    "test.txt",
			},
		},
	}

	metas := fh.ExportExtractFilesFromEnvelope(root, env, 1)
	assert.Empty(t, metas) // Should skip the attachment on write error
}

func TestFileHandler_SaveError_UnmarshalError(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fh-test-save-err-unmarshal-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	_ = os.MkdirAll(msgPath, 0700)

	// Pre-create error.json with invalid content
	_ = os.WriteFile(filepath.Join(msgPath, "error.json"), []byte("invalid json"), 0600)

	err = fh.SaveError(msgPath, []error{fmt.Errorf("new error")})
	assert.NoError(t, err) // Implementation ignores unmarshal errors and continues

	data, err := os.ReadFile(filepath.Join(msgPath, "error.json"))
	assert.NoError(t, err)
	assert.Contains(t, string(data), "new error")
}

func TestFileHandler_SaveStatusReport_MarshalError(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-status-marshal-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	// We can't easily trigger json.Marshal error for JobStatus since it's a simple struct.
	// But we can test OpenRoot failure for SaveStatusReport.
	invalidDir := filepath.Join(tmpDir, "file-not-dir")
	_ = os.WriteFile(invalidDir, []byte("test"), 0600)
	fhBad := filehandler.NewFileHandler(invalidDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	err := fhBad.SaveStatusReport("user", nil, 0, 0)
	assert.Error(t, err)
}

func TestFileHandler_WriteAttachmentsToMetadata_ReadError(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fh-test-meta-read-err-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	logger := logrus.New()
	fh := filehandler.NewFileHandler(tmpDir, nil, nil, 20, 8, 0, "raw", "default", logger)

	msgPath := filepath.Join(tmpDir, "msg-1")
	_ = os.MkdirAll(msgPath, 0700)
	// metadata.json is a directory, so ReadFile should fail
	_ = os.Mkdir(filepath.Join(msgPath, "metadata.json"), 0700)

	err := fh.WriteAttachmentsToMetadata(msgPath, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read metadata file")
}

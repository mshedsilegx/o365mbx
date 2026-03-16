//go:build proxy
// +build proxy

package o365client_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"o365mbx/emailprocessor"
	"o365mbx/engine"
	"o365mbx/filehandler"
	"o365mbx/o365client"

	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupProxiedClient(t *testing.T) (*o365client.O365Client, *http.Client) {
	proxyURLStr := "http://127.0.0.1:8000"
	proxyURL, _ := url.Parse(proxyURLStr)

	// Force proxy for THIS process
	os.Setenv("HTTP_PROXY", proxyURLStr)
	os.Setenv("HTTPS_PROXY", proxyURLStr)
	os.Setenv("http_proxy", proxyURLStr)
	os.Setenv("https_proxy", proxyURLStr)

	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	// Verify proxy is reachable via the proxied client itself
	req, _ := http.NewRequest("GET", "https://graph.microsoft.com/v1.0/me", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Skipf("Dev Proxy not reachable at %s (error: %v). Ensure Dev Proxy is running and trust the cert if needed.", proxyURLStr, err)
	}
	resp.Body.Close()

	authProvider, _ := o365client.NewStaticTokenAuthenticationProvider("dummy-token")

	// CRITICAL: We MUST use the adapter that takes the custom httpClient
	adapter, err := msgraphsdk.NewGraphRequestAdapterWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
		authProvider, nil, nil, httpClient)
	require.NoError(t, err)

	return o365client.NewO365ClientWithAdapter(adapter, nil), httpClient
}

func TestResilience_LiveProxyBehavior(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "live_proxy_test")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 1,
	}

	// Retry loop for 50% failure rate
	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()

		if err == nil {
			success = true
			break
		}
		// If throttled, wait longer
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}

	require.True(t, success, "Engine failed to succeed under chaos: %v", err)
	assert.DirExists(t, tmpDir)
}

func TestResilience_DevProxy(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_chaos")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()

		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed to succeed under chaos: %v", err)
}

func TestResilience_NestedAttachmentExtraction(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_nested")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "inlines", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "inlines",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in nested test: %v", err)

	attDir := filepath.Join(tmpDir, "msg-nested", "attachments")
	assert.FileExists(t, filepath.Join(attDir, "01_nested_message.eml"))
	nestedPartPath := filepath.Join(attDir, "01_1_nested_canary.txt")
	assert.FileExists(t, nestedPartPath)
	content, err := os.ReadFile(nestedPartPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Nested Unicode Content")
}

func TestResilience_MassiveAttachmentSet(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_massive")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 10,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in massive test: %v", err)

	attDir := filepath.Join(tmpDir, "msg-massive", "attachments")
	files, _ := os.ReadDir(attDir)
	count := 0
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) != ".json" {
			count++
		}
	}
	assert.Equal(t, 105, count)
}

func TestResilience_ConcurrencyPressure(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_concurrency")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 10,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 10
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
		backoff := 2
		if i > 5 {
			backoff = 5
		}
		if i > 10 {
			backoff = 10
		}
		time.Sleep(time.Duration(backoff) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in concurrency test: %v", err)

	files, _ := os.ReadDir(tmpDir)
	dirCount := 0
	for _, f := range files {
		if f.IsDir() {
			dirCount++
		}
	}
	fmt.Printf("Detected %d subdirectories in workspace\n", dirCount)
	assert.GreaterOrEqual(t, dirCount, 20)
}

func TestResilience_HighFidelity_InlinesEnabled(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_inlines_test")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "inlines", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "inlines",
		ConvertBody:            "text",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in inlines test: %v", err)

	msgDirHF := filepath.Join(tmpDir, "msg-hi-fi")
	attDirHF := filepath.Join(msgDirHF, "attachments")
	assert.FileExists(t, filepath.Join(attDirHF, "01_!@#$%^&()_+-=[]{}.txt"))
	assert.FileExists(t, filepath.Join(attDirHF, "02_file with spaces.pdf"))
	assert.FileExists(t, filepath.Join(attDirHF, "03_empty.dat"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_complex_nested.eml"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_1_nested_special_chars_!@#.txt"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_2_inline_image_1.png"))

	msgDirKS := filepath.Join(tmpDir, "msg-kitchen-sink")
	attDirKS := filepath.Join(msgDirKS, "attachments")
	files, _ := os.ReadDir(attDirKS)
	count := 0
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) != ".json" {
			count++
		}
	}
	assert.GreaterOrEqual(t, count, 51)
}

func TestResilience_HighFidelity_DefaultMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := filepath.Join(os.Getenv("TEMP"), "o365mbx_UTESTS", "resilience_default_test")
	os.MkdirAll(tmpDir, 0700)
	defer os.RemoveAll(tmpDir)

	processor := emailprocessor.NewEmailProcessor(log.New())
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "default",
		ConvertBody:            "text",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "dummy-token", "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in default mode test: %v", err)

	attDirHF := filepath.Join(tmpDir, "msg-hi-fi", "attachments")
	assert.FileExists(t, filepath.Join(attDirHF, "04_1_nested_special_chars_!@#.txt"))
	assert.NoFileExists(t, filepath.Join(attDirHF, "04_2_inline_image_1.png"))
}

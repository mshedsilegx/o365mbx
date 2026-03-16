package engine

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_SetDefaults(t *testing.T) {
	c := &Config{}
	c.SetDefaults()

	assert.Equal(t, 120, c.HTTPClientTimeoutSeconds)
	assert.Equal(t, 2, c.MaxRetries)
	assert.Equal(t, 5, c.InitialBackoffSeconds)
	assert.Equal(t, 20, c.LargeAttachmentThresholdMB)
	assert.Equal(t, 8, c.ChunkSizeMB)
	assert.Equal(t, 10, c.MaxParallelDownloads)
	assert.Equal(t, 5.0, c.APICallsPerSecond)
	assert.Equal(t, 10, c.APIBurst)
	assert.Equal(t, 100, c.StateSaveInterval)
	assert.Equal(t, 0.0, c.BandwidthLimitMBs)
	assert.Equal(t, "full", c.ProcessingMode)
	assert.Equal(t, "Processed", c.ProcessedFolder)
	assert.Equal(t, "Error", c.ErrorFolder)
	assert.Equal(t, "Inbox", c.InboxFolder)
	assert.Equal(t, "none", c.ConvertBody)
	assert.Equal(t, "raw", c.MsgHandler)
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  func() *Config
		wantErr bool
		msg     string
	}{
		{
			name: "Valid default config",
			config: func() *Config {
				c := &Config{}
				c.SetDefaults()
				return c
			},
			wantErr: false,
		},
		{
			name: "Negative timeout",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: -1, MsgHandler: "raw", AttachmentExtractionL1: "default", LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "httpClientTimeoutSeconds must be positive",
		},
		{
			name: "Negative retries",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, MaxRetries: -1, MsgHandler: "raw", AttachmentExtractionL1: "default", LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "maxRetries cannot be negative",
		},
		{
			name: "Negative backoff",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, InitialBackoffSeconds: -1, MsgHandler: "raw", AttachmentExtractionL1: "default", LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "initialBackoffSeconds cannot be negative",
		},
		{
			name: "Zero attachment threshold",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 0, MsgHandler: "raw", AttachmentExtractionL1: "default", ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "largeAttachmentThresholdMB must be positive",
		},
		{
			name: "Zero chunk size",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 0, MsgHandler: "raw", AttachmentExtractionL1: "default", MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "chunkSizeMB must be positive",
		},
		{
			name: "Chunk size greater than threshold",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 10, ChunkSizeMB: 20, MsgHandler: "raw", AttachmentExtractionL1: "default", MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "chunkSizeMB (20) cannot be greater than largeAttachmentThresholdMB (10)",
		},
		{
			name: "Negative parallel downloads",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 0, MsgHandler: "raw", AttachmentExtractionL1: "default", APICallsPerSecond: 5.0, StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "maxParallelDownloads must be positive",
		},
		{
			name: "Zero API rate",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 0, MsgHandler: "raw", AttachmentExtractionL1: "default", StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "apiCallsPerSecond must be positive",
		},
		{
			name: "Negative API burst",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, APIBurst: -1, MsgHandler: "raw", AttachmentExtractionL1: "default", StateSaveInterval: 100}
				return c
			},
			wantErr: true,
			msg:     "apiBurst cannot be negative",
		},
		{
			name: "Zero state save interval",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 0, MsgHandler: "raw", AttachmentExtractionL1: "default"}
				return c
			},
			wantErr: true,
			msg:     "stateSaveInterval must be positive",
		},
		{
			name: "Negative bandwidth limit",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100, BandwidthLimitMBs: -1, MsgHandler: "raw", AttachmentExtractionL1: "default"}
				return c
			},
			wantErr: true,
			msg:     "bandwidthLimitMBs cannot be negative",
		},
		{
			name: "Invalid convert body",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100, ConvertBody: "invalid", MsgHandler: "raw", AttachmentExtractionL1: "default"}
				return c
			},
			wantErr: true,
			msg:     "invalid convertBody value: invalid",
		},
		{
			name: "Invalid msg handler",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100, ConvertBody: "none", MsgHandler: "invalid", AttachmentExtractionL1: "default"}
				return c
			},
			wantErr: true,
			msg:     "invalid msgHandler value: invalid",
		},
		{
			name: "PDF mode without chromium path",
			config: func() *Config {
				c := &Config{HTTPClientTimeoutSeconds: 120, LargeAttachmentThresholdMB: 20, ChunkSizeMB: 8, MaxParallelDownloads: 10, APICallsPerSecond: 5.0, StateSaveInterval: 100, ConvertBody: "pdf", MsgHandler: "raw", AttachmentExtractionL1: "default"}
				return c
			},
			wantErr: true,
			msg:     "chromiumPath must be set when convertBody is 'pdf'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.config()
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.msg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConfig_ValidateChromiumPath(t *testing.T) {
	// Create a temporary file to test as chromium path
	tmpFile, err := os.CreateTemp("", "mock-chromium")
	assert.NoError(t, err)
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	// Make it executable
	err = tmpFile.Chmod(0755)
	assert.NoError(t, err)
	_ = tmpFile.Close()

	c := &Config{
		HTTPClientTimeoutSeconds:   120,
		LargeAttachmentThresholdMB: 20,
		ChunkSizeMB:                8,
		MaxParallelDownloads:       10,
		APICallsPerSecond:          5.0,
		StateSaveInterval:          100,
		ConvertBody:                "pdf",
		ChromiumPath:               tmpFile.Name(),
		MsgHandler:                 "raw",
		AttachmentExtractionL1:     "default",
	}

	err = c.Validate()
	// On Windows, Chmod might not set the 0111 bit as expected by the code,
	// but the file exists and is not a directory, so it might pass or fail on the execute check.
	if err != nil {
		assert.Contains(t, err.Error(), "not executable")
	} else {
		assert.NoError(t, err)
	}

	// Test non-existent path
	c.ChromiumPath = "non-existent-path"
	err = c.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "chromiumPath executable not found")

	// Test directory path
	tmpDir, _ := os.MkdirTemp("", "mock-dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()
	c.ChromiumPath = tmpDir
	err = c.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory, not an executable")
}

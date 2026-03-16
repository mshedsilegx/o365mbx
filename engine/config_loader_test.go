package engine

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadConfig(t *testing.T) {
	// Test loading with non-existent file
	cfg, err := LoadConfig("non-existent-file.json")
	assert.Error(t, err)
	assert.Nil(t, cfg)

	// Test loading with empty path (should return defaults)
	cfg, err = LoadConfig("")
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Equal(t, 120, cfg.HTTPClientTimeoutSeconds)

	// Test loading with valid JSON
	tmpFile, err := os.CreateTemp("", "config-*.json")
	assert.NoError(t, err)
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	jsonContent := `{
		"mailboxName": "test@example.com",
		"httpClientTimeoutSeconds": 60,
		"maxParallelDownloads": 5
	}`
	_, err = tmpFile.WriteString(jsonContent)
	assert.NoError(t, err)
	_ = tmpFile.Close()

	cfg, err = LoadConfig(tmpFile.Name())
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Equal(t, "test@example.com", cfg.MailboxName)
	assert.Equal(t, 60, cfg.HTTPClientTimeoutSeconds)
	assert.Equal(t, 5, cfg.MaxParallelDownloads)
	// Should still have other defaults
	assert.Equal(t, 2, cfg.MaxRetries)

	// Test loading with invalid JSON
	tmpFile2, err := os.CreateTemp("", "invalid-config-*.json")
	assert.NoError(t, err)
	defer func() { _ = os.Remove(tmpFile2.Name()) }()

	_, err = tmpFile2.WriteString(`{invalid-json}`)
	assert.NoError(t, err)
	_ = tmpFile2.Close()

	cfg, err = LoadConfig(tmpFile2.Name())
	assert.Error(t, err)
	assert.Nil(t, cfg)
}

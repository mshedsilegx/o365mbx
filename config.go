package main

import (
	"encoding/json"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	HTTPClientTimeoutSeconds   int     `json:"httpClientTimeoutSeconds"`
	MaxRetries                 int     `json:"maxRetries"`
	InitialBackoffSeconds      int     `json:"initialBackoffSeconds"`
	LargeAttachmentThresholdMB int     `json:"largeAttachmentThresholdMB"`
	ChunkSizeMB                int     `json:"chunkSizeMB"`
	MaxParallelDownloads       int     `json:"maxParallelDownloads"`
	APICallsPerSecond          float64 `json:"apiCallsPerSecond"`
	APIBurst                   int     `json:"apiBurst"`
	StateSaveInterval          int     `json:"stateSaveInterval"`
	BandwidthLimitMBs          float64 `json:"bandwidthLimitMBs"`
}

// SetDefaults sets default values for the configuration parameters.
func (c *Config) SetDefaults() {
	if c.HTTPClientTimeoutSeconds == 0 {
		c.HTTPClientTimeoutSeconds = 120
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 2
	}
	if c.InitialBackoffSeconds == 0 {
		c.InitialBackoffSeconds = 5
	}
	if c.LargeAttachmentThresholdMB == 0 {
		c.LargeAttachmentThresholdMB = 20
	}
	if c.ChunkSizeMB == 0 {
		c.ChunkSizeMB = 8
	}
	if c.MaxParallelDownloads == 0 {
		c.MaxParallelDownloads = 10
	}
	if c.APICallsPerSecond == 0 {
		c.APICallsPerSecond = 5.0 // Default: 5 calls per second
	}
	if c.APIBurst == 0 {
		c.APIBurst = 10 // Default: burst of 10 calls
	}
	if c.StateSaveInterval == 0 {
		c.StateSaveInterval = 100 // Default: save state every 100 messages
	}
	if c.BandwidthLimitMBs == 0 {
		c.BandwidthLimitMBs = 0 // Default: 0 means disabled
	}
}

// Validate checks if the configuration parameters are valid.
func (c *Config) Validate() error {
	if c.HTTPClientTimeoutSeconds <= 0 {
		return fmt.Errorf("httpClientTimeoutSeconds must be positive")
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("maxRetries cannot be negative")
	}
	if c.InitialBackoffSeconds < 0 {
		return fmt.Errorf("initialBackoffSeconds cannot be negative")
	}
	if c.LargeAttachmentThresholdMB <= 0 {
		return fmt.Errorf("largeAttachmentThresholdMB must be positive")
	}
	if c.ChunkSizeMB <= 0 {
		return fmt.Errorf("chunkSizeMB must be positive")
	}
	if c.MaxParallelDownloads <= 0 {
		return fmt.Errorf("maxParallelDownloads must be positive")
	}
	if c.APICallsPerSecond <= 0 {
		return fmt.Errorf("apiCallsPerSecond must be positive")
	}
	if c.APIBurst < 0 {
		return fmt.Errorf("apiBurst cannot be negative")
	}
	if c.StateSaveInterval <= 0 {
		return fmt.Errorf("stateSaveInterval must be positive")
	}
	if c.BandwidthLimitMBs < 0 {
		return fmt.Errorf("bandwidthLimitMBs cannot be negative")
	}
	// Add more complex validation if needed, e.g., chunkSizeMB < LargeAttachmentThresholdMB
	if c.ChunkSizeMB > c.LargeAttachmentThresholdMB {
		return fmt.Errorf("chunkSizeMB (%d) cannot be greater than largeAttachmentThresholdMB (%d)", c.ChunkSizeMB, c.LargeAttachmentThresholdMB)
	}
	return nil
}

func LoadConfig(filePath string) (*Config, error) {
	config := &Config{}
	config.SetDefaults() // Apply defaults first

	if filePath == "" {
		return config, nil // Return defaults if no config file specified
	}

	// New validation for file path using os.Stat
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infof("Config file not found at %s, using default configuration.", filePath)
			return config, nil // Return defaults if file doesn't exist
		}
		// For other errors from os.Stat (e.g., permission denied, invalid path characters)
		return nil, fmt.Errorf("failed to stat config file %s: %w", filePath, err)
	}
	// Check if the path points to a directory instead of a file
	if fileInfo.IsDir() {
		return nil, fmt.Errorf("config path %s is a directory, not a file", filePath)
	}

	bytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", filePath, err)
	}

	if err := json.Unmarshal(bytes, config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config file %s: %w", filePath, err)
	}

	return config, nil
}

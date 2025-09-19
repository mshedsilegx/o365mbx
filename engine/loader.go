package engine

import (
	"encoding/json"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
)

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

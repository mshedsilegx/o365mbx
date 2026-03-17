// Package engine implements the core business logic and orchestrates the parallelized
// email download and processing pipeline.
//
// This file provides functionality for loading and merging configuration.
package engine

import (
	"encoding/json"
	"os"
)

// LoadConfig reads a JSON configuration file and merges it with default values.
// Command-line flags usually take precedence over these values in main.go.
func LoadConfig(filePath string) (*Config, error) {
	// Start with a configuration object with default values.
	config := &Config{}
	config.SetDefaults()

	// If a file path is provided, read the file and override the defaults.
	if filePath != "" {
		// #nosec G304 - filePath is provided via command-line or internal logic, not direct user input.
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}

		// Unmarshal the JSON data into the config object.
		// This will overwrite default values with any values present in the file.
		if err := json.Unmarshal(data, config); err != nil {
			return nil, err
		}
	}

	return config, nil
}

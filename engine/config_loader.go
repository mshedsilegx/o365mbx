package engine

import (
	"encoding/json"
	"os"
)

// LoadConfig loads the configuration from a JSON file, applying defaults first.
func LoadConfig(filePath string) (*Config, error) {
	// Start with a configuration object with default values.
	config := &Config{}
	config.SetDefaults()

	// If a file path is provided, read the file and override the defaults.
	if filePath != "" {
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

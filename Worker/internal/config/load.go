package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads, parses, defaults, and validates a config file (YAML or JSON,
// chosen by extension). Use LoadBytes with an explicit format for in-memory
// data.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return LoadBytes(path, data)
}

// LoadBytes parses raw config bytes. The name is only used to pick the format
// by extension (".json" → JSON, anything else → YAML).
func LoadBytes(name string, data []byte) (*Config, error) {
	var c Config
	switch {
	case hasExt(name, ".json"):
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse json: %w", err)
		}
	default:
		if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.postLoad(); err != nil {
		return nil, err
	}
	return &c, nil
}

// postLoad performs cross-field normalization that isn't a default or a
// validation error. Currently: ensures global mode has no replicas pointer so
// the translator never emits one.
func (c *Config) postLoad() error {
	if c.Service.Mode == "global" {
		c.Service.Replicas = nil
	}
	return nil
}

func hasExt(name, ext string) bool {
	return len(name) >= len(ext) && name[len(name)-len(ext):] == ext
}

// ErrInvalid is returned when config fails validation. Use errors.Is to check.
var ErrInvalid = errors.New("invalid config")

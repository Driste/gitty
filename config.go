package main

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

const (
	ConfigDir  = ".gitty"
	ConfigPath = ".gitty/config"
)

type Config struct {
	URL  string `toml:"url"`
	HTTP bool   `toml:"http"`
}

func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", ConfigPath, err)
	}

	return &cfg, nil
}

func SaveConfig(cfg *Config) error {
	if err := os.MkdirAll(ConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to generate TOML: %w", err)
	}

	return os.WriteFile(ConfigPath, data, 0644)
}

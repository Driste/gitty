package main

import (
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const (
	ConfigDir  = ".gitty"
	ConfigName = "config"
)

// Config anchors a workspace. It is the source of truth for how that
// workspace clones: HTTP records the transport chosen at init time, and is
// always written explicitly, so changing gitty's default never re-points an
// existing workspace.
type Config struct {
	URL      string `toml:"url"`
	HTTP     bool   `toml:"http"`
	RootPath string `toml:"root_path"`
}

// LoadLocalConfig only looks in the IMMEDIATE current directory for .gitty/config
func LoadLocalConfig() (*Config, error) {
	curr, _ := os.Getwd()
	path := filepath.Join(curr, ConfigDir, ConfigName)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfigTo writes the config and creates the .gitty folder
func SaveConfigTo(dir string, cfg *Config) error {
	confDir := filepath.Join(dir, ConfigDir)
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(confDir, ConfigName), data, 0644)
}

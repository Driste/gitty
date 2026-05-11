// Package config persists Gitty workspace configuration on disk.
//
// A workspace is anchored by a TOML file at <workspaceDir>/.gitty/config
// holding the GitLab base URL, the SSH-vs-HTTP preference, the GitLab
// namespace path the directory is bound to, and the default concurrency
// for `gitty sync`.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const (
	// Dir is the directory under the workspace root that holds gitty state.
	Dir = ".gitty"
	// File is the config filename inside Dir; full path is <workspace>/.gitty/config.
	File = "config"
	// DefaultJobs is the default concurrency for `gitty sync` when the
	// jobs field is missing or non-positive in .gitty/config.
	DefaultJobs = 4
)

// Config is the on-disk workspace config.
type Config struct {
	URL      string `toml:"url"`
	HTTP     bool   `toml:"http"`
	RootPath string `toml:"root_path"`
	Jobs     int    `toml:"jobs"`
}

// Load reads <workspaceDir>/.gitty/config and returns its parsed Config.
//
// If the file does not exist, Load returns an error whose message contains
// the substring "no .gitty/config" and a hint to run `gitty init`. Other
// errors (unreadable file, malformed TOML, missing required URL) are wrapped
// with the file path so the caller can surface them verbatim.
//
// Jobs is normalized post-Unmarshal: a value <= 0 (including the Go zero
// value, which is what toml.Unmarshal produces when the key is absent) is
// rewritten to DefaultJobs. Callers never see a non-positive Jobs.
func Load(workspaceDir string) (*Config, error) {
	path := filepath.Join(workspaceDir, Dir, File)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no .gitty/config in %s; run 'gitty init' first", workspaceDir)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("invalid .gitty/config in %s: url is required", workspaceDir)
	}
	if cfg.Jobs <= 0 {
		cfg.Jobs = DefaultJobs
	}
	return &cfg, nil
}

// Save writes cfg to <workspaceDir>/.gitty/config, creating the .gitty
// directory if it does not exist.
func Save(workspaceDir string, cfg *Config) error {
	confDir := filepath.Join(workspaceDir, Dir)
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(confDir, File), data, 0644)
}

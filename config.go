package main

import (
	"fmt"
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

	// RespectGitConfig honours the local git configuration's
	// url.<base>.insteadOf rewrites instead of pinning every clone and fetch
	// to the URL gitty selected. Off by default, because the most common such
	// rule silently turns an HTTP clone into an SSH one; on for setups that
	// deliberately rewrite the instance's advertised URL to a reachable one.
	RespectGitConfig bool `toml:"respect_git_config,omitempty"`

	// CloneHosts names extra hosts whose repositories this workspace may
	// clone, fetch, and send its token to, beyond the instance's own host.
	// Needed when the API advertises clone URLs on a different host than the
	// one gitty talks to (a split API/git deployment, or a mirror).
	CloneHosts []string `toml:"clone_hosts,omitempty"`
}

// AllowCloneHosts adds hosts from a command's --allow-clone-host flags to the
// ones the workspace config already lists. It only ever widens the set, so a
// one-off run cannot silently contradict the workspace's own settings.
func (c *Config) AllowCloneHosts(hosts []string) {
	c.CloneHosts = append(c.CloneHosts, hosts...)
}

// AllowsHost reports whether a URL the GitLab API advertised may be contacted
// with this workspace's credentials: its host must be the configured
// instance's, or one the user listed in clone_hosts. An empty host on either
// side is an error, which callers treat as "not allowed".
//
// This applies only to URLs that reached gitty from the API. A destination the
// local git config rewrote to is the user's own choice and is not checked here
// — see syncer.checkRemoteHost.
func (c *Config) AllowsHost(rawURL string) (bool, error) {
	host := extractHost(rawURL)
	instance := extractHost(c.URL)
	if host == "" || instance == "" {
		return false, fmt.Errorf("could not determine host (config %q, remote %q)", c.URL, redactURL(rawURL))
	}
	if host == instance {
		return true, nil
	}
	for _, allowed := range c.CloneHosts {
		if extractHost(allowed) == host {
			return true, nil
		}
	}
	return false, nil
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

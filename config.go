package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

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

	// CloneHosts names extra hosts this workspace expects to clone and fetch
	// from, beyond the instance's own. Listing one records that an API
	// advertising repositories on another host (a split API/git deployment, or
	// a mirror) is intended, which silences the note gitty otherwise prints.
	CloneHosts []string `toml:"clone_hosts,omitempty"`
}

// AllowCloneHosts adds hosts from a command's --allow-clone-host flags to the
// ones the workspace config already lists. It only ever widens the set, so a
// one-off run cannot silently contradict the workspace's own settings.
func (c *Config) AllowCloneHosts(hosts []string) {
	c.CloneHosts = append(c.CloneHosts, hosts...)
}

// AllowsHost reports whether a remote URL's host is one this workspace expects
// to talk to: the configured instance's, or one the user listed in
// clone_hosts. An empty host on either side is an error, which callers treat
// as "not expected".
//
// It answers "should gitty say something about this host", not "may git go
// there" — git's own configuration decides that, and gitty does not override
// it. See syncer.noteForeignHost.
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

// DiscoverWorkspace locates the workspace from anywhere inside it. It walks
// up from the current directory to the nearest .gitty/config, works out
// which group the current directory corresponds to, and returns the config
// with root_path pointing at that group — so every command behaves as if it
// were run from a directory that had its own config for that group.
//
// The group is read off the path: root_path names the group of the directory
// holding the config, and each directory below it down to the current one is
// a subgroup, until a git checkout (or a moved-aside broken one) is reached —
// a checkout is a project, not a group, so a command run from inside one acts
// on the group that contains it.
//
// The process then changes into that group's directory. Every path gitty
// computes is relative to the directory it runs in, and this keeps that
// invariant true wherever the command was started from.
func DiscoverWorkspace() (*Config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining working directory: %w", err)
	}
	cfgDir, err := findConfigDir(wd)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, ConfigDir, ConfigName))
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Join(cfgDir, ConfigDir, ConfigName), err)
	}

	groupDir, below := contextBelow(cfgDir, wd)
	if below != "" {
		cfg.RootPath = path.Join(cfg.RootPath, below)
	}
	if groupDir != wd {
		if err := os.Chdir(groupDir); err != nil {
			return nil, fmt.Errorf("entering %s: %w", groupDir, err)
		}
	}
	return &cfg, nil
}

// findConfigDir returns the nearest ancestor of dir (dir included) holding a
// .gitty/config — the way git finds a repository from a subdirectory, and
// with the same two guards git applies:
//
// The search does not cross a filesystem boundary. A workspace lives on one
// filesystem; what is mounted above it is not part of it.
//
// A config is accepted only if the user running gitty owns it (root's count
// as theirs). The workspace config names the GitLab instance gitty talks to
// and sends its token to, so a config planted by another user in a shared
// parent directory — /tmp, a shared build root — must never be picked up on
// their behalf. This is git's safe.directory rule, applied to the one file
// where it matters here.
func findConfigDir(dir string) (string, error) {
	dev := deviceOf(dir)
	for d := dir; ; {
		if dev != 0 && deviceOf(d) != dev {
			break
		}
		conf := filepath.Join(d, ConfigDir, ConfigName)
		if fi, err := os.Stat(conf); err == nil && !fi.IsDir() {
			if !ownedByCurrentUser(conf) {
				return "", usageErrf("refusing to use %s: it is owned by another user (a workspace config decides which instance receives your token)", conf)
			}
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", usageErrf("no .gitty/config found in %s or any parent directory on the same filesystem; run 'gitty init' at the workspace root first", dir)
}

// contextBelow walks from cfgDir down towards wd and returns the deepest
// directory that is still a group, plus the group path it adds (forward
// slashes) relative to cfgDir. A git checkout or a moved-aside broken one
// ends the walk: what lies at or below it is a project's contents.
func contextBelow(cfgDir, wd string) (groupDir, below string) {
	rel, err := filepath.Rel(cfgDir, wd)
	if err != nil || rel == "." {
		return cfgDir, ""
	}
	groupDir = cfgDir
	var parts []string
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		next := filepath.Join(groupDir, seg)
		if seg == ConfigDir || strings.Contains(seg, brokenAsideMarker) || classifyDest(next) == destRepo {
			break
		}
		groupDir = next
		parts = append(parts, seg)
	}
	return groupDir, strings.Join(parts, "/")
}

package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// validateInstanceURL checks that a GitLab base URL is usable before it is
// persisted to the workspace config, so a typo fails at init time instead of
// at first sync.
func validateInstanceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return usageErrf("invalid --url %q: %v", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return usageErrf("invalid --url %q: must be an http(s) URL like https://gitlab.example.com", raw)
	}
	return nil
}

func runInit(rawURL string, useHTTP, force bool) error {
	if err := validateInstanceURL(rawURL); err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determining working directory: %w", err)
	}

	// Refuse to clobber an existing workspace config: overwriting resets
	// root_path, which silently re-anchors a managed subgroup directory to the
	// workspace root and makes the next sync re-clone the entire namespace.
	confPath := filepath.Join(wd, ConfigDir, ConfigName)
	if _, statErr := os.Stat(confPath); statErr == nil && !force {
		if existing, loadErr := LoadLocalConfig(); loadErr == nil {
			fmt.Fprintf(os.Stderr, "existing config: url=%s http=%t root_path=%q\n",
				existing.URL, existing.HTTP, existing.RootPath)
		}
		return usageErrf("refusing to overwrite existing .gitty/config (use --force)")
	}

	cfg := &Config{
		URL:      rawURL,
		HTTP:     useHTTP,
		RootPath: "", // The base of your workspace
	}

	if err := SaveConfigTo(wd, cfg); err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}

	fmt.Printf("Initialized gitty root at %s\n", wd)
	fmt.Println("You can now run 'gitty sync --path=<path>' to pull down repositories.")
	return nil
}

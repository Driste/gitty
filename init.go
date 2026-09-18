package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// initOptions bundles the init command's flags.
type initOptions struct {
	URL        string
	HTTP       bool
	Force      bool
	Verify     bool
	Token      string
	CloneHosts []string
}

func runInit(opts initOptions) error {
	if err := validateInstanceURL(opts.URL); err != nil {
		return err
	}
	for _, h := range opts.CloneHosts {
		if extractHost(h) == "" {
			return usageErrf("invalid --allow-clone-host %q: expected a hostname like git.example.com", h)
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determining working directory: %w", err)
	}

	// Refuse to clobber an existing workspace config: overwriting resets
	// root_path, which silently re-anchors a managed subgroup directory to the
	// workspace root and makes the next sync re-clone the entire namespace.
	confPath := filepath.Join(wd, ConfigDir, ConfigName)
	if _, statErr := os.Stat(confPath); statErr == nil && !opts.Force {
		if existing, loadErr := LoadLocalConfig(); loadErr == nil {
			fmt.Fprintf(os.Stderr, "existing config: url=%s http=%t root_path=%q\n",
				existing.URL, existing.HTTP, existing.RootPath)
		}
		return usageErrf("refusing to overwrite existing .gitty/config (use --force)")
	}

	cfg := &Config{
		URL:        opts.URL,
		HTTP:       opts.HTTP,
		RootPath:   "", // The base of your workspace
		CloneHosts: opts.CloneHosts,
	}

	if err := SaveConfigTo(wd, cfg); err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}

	transport := "HTTP(S), authenticated with your token"
	if !opts.HTTP {
		transport = "SSH, using your local SSH keys"
	}
	fmt.Printf("Initialized gitty root at %s\n", wd)
	fmt.Printf("Cloning over %s\n", transport)
	if len(cfg.CloneHosts) > 0 {
		fmt.Printf("Also cloning from: %s\n", strings.Join(cfg.CloneHosts, ", "))
	}
	fmt.Println("You can now run 'gitty sync --path=<path>' to pull down repositories.")

	// Check the credential now, while there is somewhere to put the answer.
	// Finding out that a token is missing, expired or missing a scope here is
	// far cheaper than discovering it part-way through a sync. Advisory only:
	// the workspace exists either way, and it comes last so the guidance is
	// the final thing on screen.
	if opts.Verify {
		fmt.Fprintln(os.Stderr)
		checkAuthForInit(os.Stderr, cfg, opts.Token)
	}
	return nil
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/api/client-go"
)

// readFileString reads a file that the test requires to exist.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func TestRunInitDefaultsToHTTP(t *testing.T) {
	t.Chdir(t.TempDir())

	// runInit's useHTTP argument is what main derives from --ssh; the default
	// invocation (no --ssh) must produce an HTTP workspace.
	if err := runInit(initOptions{URL: "https://gitlab.com", HTTP: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	cfg, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("LoadLocalConfig: %v", err)
	}
	if !cfg.HTTP {
		t.Error("a default workspace should clone over HTTP")
	}
}

func TestRunInitSSHIsRecordedExplicitly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := runInit(initOptions{URL: "https://gitlab.com"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	cfg, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("LoadLocalConfig: %v", err)
	}
	if cfg.HTTP {
		t.Error("--ssh should record an SSH workspace")
	}
	// The choice must be written explicitly, so that flipping gitty's default
	// can never silently re-point an existing workspace.
	raw := readFileString(t, filepath.Join(dir, ConfigDir, ConfigName))
	if !strings.Contains(raw, "http = false") {
		t.Errorf("transport should be stored explicitly, got:\n%s", raw)
	}
}

// TestLegacyWorkspaceTransportIsHonored pins the compatibility contract for
// workspaces initialized before HTTP became the default: the stored config is
// the source of truth, so flipping gitty's default must not re-point an
// existing workspace's clones.
func TestLegacyWorkspaceTransportIsHonored(t *testing.T) {
	cases := []struct {
		name     string
		config   string
		wantHTTP bool
		wantURL  string
	}{
		{
			// Written by `gitty init` before the default flipped: SSH.
			name:     "legacy ssh workspace stays ssh",
			config:   "url = 'https://gitlab.com'\nhttp = false\nroot_path = ''\n",
			wantHTTP: false,
			wantURL:  "git@gitlab.com:acme/repo.git",
		},
		{
			// Written by `gitty init --http`.
			name:     "legacy http workspace stays http",
			config:   "url = 'https://gitlab.com'\nhttp = true\nroot_path = ''\n",
			wantHTTP: true,
			wantURL:  "https://gitlab.com/acme/repo.git",
		},
		{
			// Hand-written or truncated config with no http key at all: it
			// must resolve the same way the old binary resolved it.
			name:     "config without an http key resolves as before",
			config:   "url = 'https://gitlab.com'\nroot_path = ''\n",
			wantHTTP: false,
			wantURL:  "git@gitlab.com:acme/repo.git",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.MkdirAll(ConfigDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ConfigDir, ConfigName), []byte(tc.config), 0644); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadLocalConfig()
			if err != nil {
				t.Fatalf("LoadLocalConfig: %v", err)
			}
			if cfg.HTTP != tc.wantHTTP {
				t.Errorf("cfg.HTTP = %v, want %v", cfg.HTTP, tc.wantHTTP)
			}

			// The behavioral half: prove which URL a sync actually clones.
			rec := &recordingGit{}
			s, _, _ := newTestSyncer(cfg, fakeSource{
				projects: map[string][]*gitlab.Project{
					"acme": {{
						PathWithNamespace: "acme/repo",
						HTTPURLToRepo:     "https://gitlab.com/acme/repo.git",
						SSHURLToRepo:      "git@gitlab.com:acme/repo.git",
					}},
				},
			}, rec.run)
			s.jobs = 1
			s.syncRepos(context.Background(), "acme")

			if net := rec.networkCalls(); len(net) != 1 || gitSubArgs(net[0])[0] != "fetch" {
				t.Fatalf("expected one fetch, got %v", rec.calls)
			}
			if got := rec.remoteAddURL(); got != tc.wantURL {
				t.Errorf("origin URL = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestValidateInstanceURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https url", raw: "https://gitlab.com", wantErr: false},
		{name: "http url with port", raw: "http://gitlab.internal:8080", wantErr: false},
		{name: "missing scheme", raw: "gitlab.com", wantErr: true},
		{name: "unsupported scheme", raw: "ssh://gitlab.com", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "garbage", raw: "not a url at all", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstanceURL(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateInstanceURL(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestRunInitRefusesClobber(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := runInit(initOptions{URL: "https://first.example.com", HTTP: true}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	err := runInit(initOptions{URL: "https://second.example.com"})
	if err == nil {
		t.Fatal("second init without --force should fail")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force, got: %v", err)
	}

	// The original config must be untouched.
	cfg, loadErr := LoadLocalConfig()
	if loadErr != nil {
		t.Fatalf("LoadLocalConfig: %v", loadErr)
	}
	if cfg.URL != "https://first.example.com" || !cfg.HTTP {
		t.Errorf("config was modified by refused init: %+v", cfg)
	}
}

func TestRunInitForceOverwrites(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := runInit(initOptions{URL: "https://first.example.com", HTTP: true}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := runInit(initOptions{URL: "https://second.example.com", Force: true}); err != nil {
		t.Fatalf("forced init: %v", err)
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("LoadLocalConfig: %v", err)
	}
	if cfg.URL != "https://second.example.com" || cfg.HTTP {
		t.Errorf("forced init did not overwrite: %+v", cfg)
	}
}

func TestRunInitOverwritesCorruptConfigOnlyWithForce(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// A present-but-corrupt config still counts as "exists" for the guard.
	if err := os.MkdirAll(ConfigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigDir+"/"+ConfigName, []byte("not [valid toml"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := runInit(initOptions{URL: "https://gitlab.com"}); err == nil {
		t.Fatal("init over corrupt config without --force should fail")
	}
	if err := runInit(initOptions{URL: "https://gitlab.com", Force: true}); err != nil {
		t.Fatalf("forced init over corrupt config: %v", err)
	}
}

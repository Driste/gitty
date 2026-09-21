package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSaveAndLoadConfigRoundtrip(t *testing.T) {
	dir := t.TempDir()

	want := &Config{
		URL:        "https://gitlab.example.com",
		HTTP:       true,
		RootPath:   "acme/team",
		CloneHosts: []string{"git.example.com", "mirror.example.com"},
	}
	if err := SaveConfigTo(dir, want); err != nil {
		t.Fatalf("SaveConfigTo returned error: %v", err)
	}

	// The config must live at <dir>/.gitty/config.
	confPath := filepath.Join(dir, ConfigDir, ConfigName)
	if _, err := os.Stat(confPath); err != nil {
		t.Fatalf("expected config at %s: %v", confPath, err)
	}

	// LoadLocalConfig reads from the current working directory, so change into
	// the temp dir for the read. t.Chdir restores the previous dir on cleanup.
	t.Chdir(dir)
	got, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("LoadLocalConfig returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roundtrip mismatch:\n got  %+v\n want %+v", *got, *want)
	}
}

func TestLoadLocalConfigMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := LoadLocalConfig(); err == nil {
		t.Fatal("expected an error when no .gitty/config exists, got nil")
	}
}

func TestRunSyncErrorsWithoutConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runSync(context.Background(), syncOptions{Path: "acme", Repos: true, Jobs: 1})
	if err == nil {
		t.Fatal("expected an error when no .gitty/config exists")
	}
	if exitCode(err) != 2 {
		t.Errorf("missing config should be a usage error (exit 2), got %d", exitCode(err))
	}
}

func TestRunSyncErrorsWithoutTokenOrAnon(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := runInit(initOptions{URL: "https://gitlab.com", HTTP: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	// Ensure no ambient tokens satisfy the requirement.
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")

	err := runSync(context.Background(), syncOptions{Path: "acme", Repos: true, Jobs: 1})
	if err == nil {
		t.Fatal("expected an error when no token is provided and --anon is not set")
	}
	if exitCode(err) != 2 {
		t.Errorf("missing token should be a usage error (exit 2), got %d", exitCode(err))
	}
}

func TestRunInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := runInit(initOptions{URL: "https://gitlab.custom.io", HTTP: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("LoadLocalConfig after runInit: %v", err)
	}
	if cfg.URL != "https://gitlab.custom.io" {
		t.Errorf("URL = %q, want %q", cfg.URL, "https://gitlab.custom.io")
	}
	if !cfg.HTTP {
		t.Error("HTTP = false, want true")
	}
	if cfg.RootPath != "" {
		t.Errorf("RootPath = %q, want empty for a freshly initialized workspace", cfg.RootPath)
	}
}

// DiscoverWorkspace finds the nearest config above the current directory and
// reads the current group off the path below it, stopping at a checkout.
func TestDiscoverWorkspace(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := SaveConfigTo(root, &Config{URL: "https://gitlab.example.com", HTTP: true}); err != nil {
		t.Fatal(err)
	}
	// acme/team is a plain group directory; acme/team/app is a checkout
	// with a src/ directory inside it.
	mkRepo(t, filepath.Join(root, "acme", "team", "app"))
	if err := os.MkdirAll(filepath.Join(root, "acme", "team", "app", "src"), 0755); err != nil {
		t.Fatal(err)
	}
	// acme/sub has its own config, as --groups writes them.
	if err := SaveConfigTo(filepath.Join(root, "acme", "sub"), &Config{URL: "https://gitlab.example.com", HTTP: true, RootPath: "acme/sub"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "acme", "sub", "deep"), 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		start    string
		wantRoot string
		wantDir  string
	}{
		{start: root, wantRoot: "", wantDir: root},
		{start: filepath.Join(root, "acme"), wantRoot: "acme", wantDir: filepath.Join(root, "acme")},
		{start: filepath.Join(root, "acme", "team"), wantRoot: "acme/team", wantDir: filepath.Join(root, "acme", "team")},
		// Inside a checkout: the group that contains it.
		{start: filepath.Join(root, "acme", "team", "app"), wantRoot: "acme/team", wantDir: filepath.Join(root, "acme", "team")},
		{start: filepath.Join(root, "acme", "team", "app", "src"), wantRoot: "acme/team", wantDir: filepath.Join(root, "acme", "team")},
		// A subgroup config takes over from there, and extends the same way.
		{start: filepath.Join(root, "acme", "sub"), wantRoot: "acme/sub", wantDir: filepath.Join(root, "acme", "sub")},
		{start: filepath.Join(root, "acme", "sub", "deep"), wantRoot: "acme/sub/deep", wantDir: filepath.Join(root, "acme", "sub", "deep")},
	}
	for _, tc := range tests {
		t.Run(strings.TrimPrefix(tc.start, root), func(t *testing.T) {
			t.Chdir(tc.start)
			cfg, err := DiscoverWorkspace()
			if err != nil {
				t.Fatalf("DiscoverWorkspace: %v", err)
			}
			if cfg.RootPath != tc.wantRoot {
				t.Errorf("RootPath = %q, want %q", cfg.RootPath, tc.wantRoot)
			}
			if cfg.URL != "https://gitlab.example.com" || !cfg.HTTP {
				t.Errorf("config fields not carried: %+v", cfg)
			}
			wd, _ := os.Getwd()
			wd, _ = filepath.EvalSymlinks(wd)
			if wd != tc.wantDir {
				t.Errorf("working directory = %q, want %q", wd, tc.wantDir)
			}
		})
	}

	t.Run("outside any workspace", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := DiscoverWorkspace()
		if err == nil || exitCode(err) != 2 {
			t.Errorf("want a usage error, got %v", err)
		}
	})
}

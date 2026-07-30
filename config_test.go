package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadConfigRoundtrip(t *testing.T) {
	dir := t.TempDir()

	want := &Config{
		URL:      "https://gitlab.example.com",
		HTTP:     true,
		RootPath: "acme/team",
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
	if *got != *want {
		t.Errorf("roundtrip mismatch:\n got  %+v\n want %+v", *got, *want)
	}
}

func TestLoadLocalConfigMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := LoadLocalConfig(); err == nil {
		t.Fatal("expected an error when no .gitty/config exists, got nil")
	}
}

func TestRunInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	runInit("https://gitlab.custom.io", true)

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

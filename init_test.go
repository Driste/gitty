package main

import (
	"os"
	"strings"
	"testing"
)

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

	if err := runInit("https://first.example.com", true, false); err != nil {
		t.Fatalf("first init: %v", err)
	}

	err := runInit("https://second.example.com", false, false)
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

	if err := runInit("https://first.example.com", true, false); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := runInit("https://second.example.com", false, true); err != nil {
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

	if err := runInit("https://gitlab.com", false, false); err == nil {
		t.Fatal("init over corrupt config without --force should fail")
	}
	if err := runInit("https://gitlab.com", false, true); err != nil {
		t.Fatalf("forced init over corrupt config: %v", err)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &Config{URL: "https://example.com", HTTP: true, RootPath: "tenant/images", Jobs: 8}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.URL != in.URL || out.HTTP != in.HTTP || out.RootPath != in.RootPath || out.Jobs != in.Jobs {
		t.Fatalf("round-trip mismatch:\n in = %+v\nout = %+v", *in, *out)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load on empty dir: expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no .gitty/config") {
		t.Errorf("error missing 'no .gitty/config': %q", msg)
	}
	if !strings.Contains(msg, "gitty init") {
		t.Errorf("error missing 'gitty init' hint: %q", msg)
	}
}

// A .gitty/config written by a pre-feature binary has no `jobs` line.
// Load MUST treat the missing field as DefaultJobs (FR-001b, SC-010).
func TestLoadCoalescesMissingJobs(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, Dir)
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	contents := `url = "https://gl.example"
http = false
root_path = "tenant"
`
	if err := os.WriteFile(filepath.Join(confDir, File), []byte(contents), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jobs != DefaultJobs {
		t.Errorf("Jobs = %d, want %d (DefaultJobs)", cfg.Jobs, DefaultJobs)
	}
}

// A non-positive jobs value on disk is treated the same as a missing one.
func TestLoadCoalescesNegativeJobs(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, Dir)
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	contents := `url = "https://gl.example"
jobs = -1
`
	if err := os.WriteFile(filepath.Join(confDir, File), []byte(contents), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jobs != DefaultJobs {
		t.Errorf("Jobs = %d, want %d (DefaultJobs)", cfg.Jobs, DefaultJobs)
	}
}

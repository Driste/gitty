package config

import (
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &Config{URL: "https://example.com", HTTP: true, RootPath: "tenant/images"}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.URL != in.URL || out.HTTP != in.HTTP || out.RootPath != in.RootPath {
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

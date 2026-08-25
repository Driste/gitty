package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestVersionStringUsesLinkerValue(t *testing.T) {
	// Release builds set `version` via -ldflags "-X main.version=<tag>"; the
	// tag must be reported back byte-for-byte, because the release workflow
	// asserts `gitty version` equals the tag it built.
	original := version
	t.Cleanup(func() { version = original })

	version = "v1.2.3"
	if got := versionString(); got != "v1.2.3" {
		t.Errorf("versionString() = %q, want %q", got, "v1.2.3")
	}
}

func TestVersionStringDevBuild(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = devVersion
	got := versionString()
	if !strings.HasPrefix(got, devVersion) {
		t.Errorf("versionString() = %q, want it to start with %q", got, devVersion)
	}
	// Inside this repository Go stamps VCS info in, so the dev version should
	// carry a commit suffix. Accept a bare "dev" too, since builds made
	// outside a repository (or with -buildvcs=false) legitimately have none.
	devRe := regexp.MustCompile(`^dev(\+[0-9a-f]{7,12}(-dirty)?)?$`)
	if !devRe.MatchString(got) {
		t.Errorf("versionString() = %q, want dev or dev+<commit>[-dirty]", got)
	}
}

func TestBuildRevisionSuffixIsWellFormed(t *testing.T) {
	// Whatever the build environment, the suffix is either empty or a short
	// commit — never a raw 40-character hash pasted into the version string.
	got := buildRevisionSuffix()
	if got == "" {
		return
	}
	if !strings.HasPrefix(got, "+") {
		t.Fatalf("buildRevisionSuffix() = %q, want it to start with '+'", got)
	}
	rev := strings.TrimSuffix(strings.TrimPrefix(got, "+"), "-dirty")
	if len(rev) > 12 {
		t.Errorf("revision %q is longer than the 12-character short form", rev)
	}
}

func TestVersionToolInAgentSchema(t *testing.T) {
	s := buildAgentSchema()
	tool, ok := findTool(s, "version")
	if !ok {
		t.Fatal("schema is missing the version tool")
	}
	if len(tool.InputSchema.Properties) != 0 {
		t.Errorf("version tool should take no arguments, got %v", tool.InputSchema.Properties)
	}
	if len(tool.Invocation.BaseArgs) != 1 || tool.Invocation.BaseArgs[0] != "version" {
		t.Errorf("version tool baseArgs = %v", tool.Invocation.BaseArgs)
	}
}

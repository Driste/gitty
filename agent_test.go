package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func findTool(s AgentSchema, name string) (AgentTool, bool) {
	for _, tool := range s.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return AgentTool{}, false
}

func TestBuildAgentSchemaShape(t *testing.T) {
	s := buildAgentSchema()

	if s.Name != "gitty" {
		t.Errorf("schema Name = %q, want %q", s.Name, "gitty")
	}
	if s.Version != AgentSchemaVersion {
		t.Errorf("schema Version = %q, want %q", s.Version, AgentSchemaVersion)
	}

	for _, name := range []string{"init", "sync"} {
		if _, ok := findTool(s, name); !ok {
			t.Errorf("schema is missing the %q tool", name)
		}
	}

	for _, code := range []string{"0", "1", "2", "130"} {
		if _, ok := s.ExitCodes[code]; !ok {
			t.Errorf("schema exitCodes missing %q", code)
		}
	}
}

func TestSyncToolAdvertisesAnon(t *testing.T) {
	s := buildAgentSchema()
	sync, ok := findTool(s, "sync")
	if !ok {
		t.Fatal("sync tool not found in schema")
	}

	anon, ok := sync.InputSchema.Properties["anon"]
	if !ok {
		t.Fatal("sync tool does not advertise the 'anon' property")
	}
	if anon.Type != "boolean" {
		t.Errorf("anon.Type = %q, want %q", anon.Type, "boolean")
	}
	if anon.Default != false {
		t.Errorf("anon.Default = %v, want false", anon.Default)
	}

	// path must remain a required argument for sync.
	foundPath := false
	for _, r := range sync.InputSchema.Required {
		if r == "path" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("sync tool Required = %v, want it to include %q", sync.InputSchema.Required, "path")
	}
}

func TestAgentSchemaRendersValidJSON(t *testing.T) {
	s := buildAgentSchema()

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshaling schema: %v", err)
	}

	// Must decode back into the same structure without loss.
	var round AgentSchema
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshaling schema: %v", err)
	}
	if len(round.Tools) != len(s.Tools) {
		t.Errorf("round-tripped tool count = %d, want %d", len(round.Tools), len(s.Tools))
	}

	// The rendered hints keep '<' and '>' literal, so ensure they are not
	// HTML-escaped when we mimic runAgentSchema's encoder settings.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatalf("encoding schema: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("\\u003c")) {
		t.Error("schema output HTML-escaped '<' as \\u003c; flag-style hints should stay literal")
	}
	if !bytes.Contains(buf.Bytes(), []byte("--<name>")) {
		t.Error("expected literal flag-style hint '--<name>' in schema output")
	}
}

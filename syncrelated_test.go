package main

import (
	"strings"
	"testing"

	"gitty/internal/contract"
)

func TestRelatedProjects(t *testing.T) {
	idx := newIndex(&Config{URL: "https://gitlab.example.com"})
	idx.Links = []contract.Link{
		{From: "acme/app", To: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:5"},
		{From: "acme/app", To: "acme/lib", Kind: contract.KindMentions, Evidence: "README.md:3"},
		{From: "acme/app", To: "platform/ci", Kind: contract.KindCIInclude, Evidence: ".gitlab-ci.yml:2"},
		// Only ever mentioned: a sentence, not a dependency.
		{From: "acme/app", To: "acme/handbook", Kind: contract.KindMentions, Evidence: "README.md:9"},
		{From: "acme/web", To: "acme/app", Kind: contract.KindCITrigger, Evidence: ".gitlab-ci.yml:7"},
		{From: "acme/docs", To: "acme/app", Kind: contract.KindMentions, Evidence: "README.md:1"},
		{From: "acme/app-fork", To: "acme/app", Kind: contract.KindFork, Evidence: "api:forked_from_project"},
		{From: "other/svc", To: "other/base", Kind: contract.KindImage, Evidence: "Dockerfile:1"},
	}
	app := subject{Path: "acme/app"}
	acme := subject{Group: true, Path: "acme"}
	everything := subject{Group: true}
	tests := []struct {
		name      string
		subj      subject
		direction string
		want      string
	}{
		{"what a project uses", app, directionUses, "acme/lib platform/ci"},
		{"what uses a project", app, directionUsedBy, "acme/app-fork acme/web"},
		{"both sides of a project", app, directionBoth, "acme/app-fork acme/lib acme/web platform/ci"},
		{"a project nothing links to", subject{Path: "acme/handbook"}, directionBoth, ""},
		// A member of the group that another member uses counts: it is a
		// dependency of something here whichever group it belongs to.
		{"what a group's projects use", acme, directionUses, "acme/app acme/lib platform/ci"},
		{"what uses a group's projects", acme, directionUsedBy, "acme/app acme/app-fork acme/web"},
		{"everything checked out, and what it uses", everything, directionUses, "acme/app acme/lib other/base platform/ci"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(relatedProjects(idx, tt.subj, tt.direction), " "); got != tt.want {
				t.Errorf("relatedProjects = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelatedFlag(t *testing.T) {
	for in, want := range map[string]string{
		"true":    directionUses, // `--related`, bare
		"false":   "",
		"uses":    directionUses,
		"used-by": directionUsedBy,
		"both":    directionBoth,
	} {
		var f relatedFlag
		if err := f.Set(in); err != nil || string(f) != want {
			t.Errorf("Set(%q) = %q, %v; want %q", in, f, err, want)
		}
	}
	var f relatedFlag
	if err := f.Set("sideways"); err == nil {
		t.Error("an unknown direction was accepted")
	}
	if !f.IsBoolFlag() {
		t.Error("--related must be accepted without a value")
	}
}

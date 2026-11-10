package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitty/internal/contract"
)

// briefingWorkspace is a workspace rooted at group "acme" with real
// directories for the checkouts named, started in startRel (relative to the
// root). No git is involved: a briefing is built from the index alone.
func briefingWorkspace(t *testing.T, startRel string, checkouts ...string) (*workspace, *contract.Index) {
	t.Helper()
	root := realDir(t, t.TempDir())
	cfg := &Config{URL: "https://gitlab.example.com", RootPath: "acme"}
	s, _, _ := newTestSyncer(cfg, fakeSource{}, nil)
	w := &workspace{s: s, Root: root, StartDir: filepath.Join(root, filepath.FromSlash(startRel)), Group: "acme"}
	if err := os.MkdirAll(w.StartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := newIndex(cfg)
	for i, dir := range checkouts {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
		idx.Projects["acme/"+dir] = &contract.Project{
			ID:          int64(i + 1),
			Description: "The " + dir + " project",
			Local:       &contract.Local{Dir: dir, Head: "h", Docs: []string{"README.md"}},
		}
	}
	return w, idx
}

func TestBuildBriefingProject(t *testing.T) {
	w, idx := briefingWorkspace(t, "app/src", "app", "lib", "web", "platform/svc")
	idx.Projects["acme/app"].DefaultBranch = "main"
	idx.Projects["acme/app"].Topics = []string{"go"}
	idx.Projects["acme/lib"].Local.Docs = []string{"AGENTS.md", "README.md"}
	idx.Projects["acme/base-images"] = &contract.Project{ID: 50, Description: "Base images", Archived: true}
	idx.Projects["acme/vendor/mirror"] = &contract.Project{ID: 51}
	w.s.cfg.Exclude = []string{"acme/vendor"}
	idx.Links = []contract.Link{
		// Two references to the same project are one related project.
		{From: "acme/app", To: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:12"},
		{From: "acme/app", To: "acme/lib", Kind: contract.KindCIInclude, Evidence: ".gitlab-ci.yml:3"},
		{From: "acme/app", To: "acme/base-images", Kind: contract.KindImage, Evidence: "Dockerfile:1"},
		{From: "acme/web", To: "acme/app", Kind: contract.KindDepends, Evidence: "package.json:21"},
		{From: "acme/platform/svc", To: "acme/app", Kind: contract.KindCITrigger, Evidence: ".gitlab-ci.yml:40"},
		{From: "acme/web", To: "acme/lib", Kind: contract.KindDepends, Evidence: "package.json:22"}, // nothing to do with app
	}
	idx.Unresolved = []contract.Unresolved{
		{From: "acme/app", Ref: "acme/old-lib", Kind: contract.KindDepends, Evidence: "go.mod:14"},
		{From: "acme/web", Ref: "acme/gone", Kind: contract.KindDepends, Evidence: "package.json:30"},
	}

	b, err := w.buildBriefing(idx, subject{Path: "acme/app"}, directionBoth)
	if err != nil {
		t.Fatal(err)
	}
	if b.Schema != contract.Schema || b.Workspace != w.Root {
		t.Errorf("header = %+v", b)
	}
	s := b.Subject
	if s.Kind != "project" || s.Project != "acme/app" || !s.Present || s.Path != filepath.Join(w.Root, "app") ||
		s.RelativePath != ".." || s.DefaultBranch != "main" || s.LocalOnly {
		t.Errorf("subject = %+v", s)
	}

	if b.UsesTotal != 2 || len(b.Uses) != 2 {
		t.Fatalf("uses = %+v", b.Uses)
	}
	base, lib := b.Uses[0], b.Uses[1]
	if base.Project != "acme/base-images" || base.Present || !base.Archived || base.Path != "" ||
		base.FetchCommand != "gitty fetch acme/base-images" || len(base.Links) != 1 {
		t.Errorf("acme/base-images = %+v", base)
	}
	if lib.Project != "acme/lib" || !lib.Present || lib.Path != filepath.Join(w.Root, "lib") ||
		lib.RelativePath != "../../lib" || len(lib.Links) != 2 || lib.FetchCommand != "" || lib.Fetched {
		t.Errorf("acme/lib = %+v", lib)
	}
	if lib.Links[0].Kind != contract.KindCIInclude || lib.Links[1].Evidence != "go.mod:12" {
		t.Errorf("acme/lib links = %+v, want both, in a stable order", lib.Links)
	}

	if b.UsedByTotal != 2 || len(b.UsedBy) != 2 || b.UsedBy[0].Project != "acme/platform/svc" ||
		b.UsedBy[0].RelativePath != "../../platform/svc" || b.UsedBy[1].Project != "acme/web" {
		t.Errorf("used by = %+v", b.UsedBy)
	}
	if len(b.Unresolved) != 1 || b.Unresolved[0].Ref != "acme/old-lib" {
		t.Errorf("unresolved = %+v, want only the subject's own", b.Unresolved)
	}
	if got := strings.Join(absentUses(b), ","); got != "acme/base-images" {
		t.Errorf("absentUses = %q", got)
	}

	t.Run("one side only", func(t *testing.T) {
		b, err := w.buildBriefing(idx, subject{Path: "acme/app"}, directionUses)
		if err != nil {
			t.Fatal(err)
		}
		// The other side is not listed, but its size is still told.
		if len(b.Uses) != 2 || len(b.UsedBy) != 0 || b.UsedBy == nil || b.UsedByTotal != 2 {
			t.Errorf("uses only: uses %d, used by %d (total %d)", len(b.Uses), len(b.UsedBy), b.UsedByTotal)
		}
		b, _ = w.buildBriefing(idx, subject{Path: "acme/app"}, directionUsedBy)
		if len(b.Uses) != 0 || b.Uses == nil || len(b.UsedBy) != 2 || b.UsesTotal != 2 || len(b.Unresolved) != 0 {
			t.Errorf("used-by only: uses %d (total %d), used by %d, unresolved %d", len(b.Uses), b.UsesTotal, len(b.UsedBy), len(b.Unresolved))
		}
	})

	t.Run("a project that is not checked out", func(t *testing.T) {
		b, err := w.buildBriefing(idx, subject{Path: "acme/base-images"}, directionBoth)
		if err != nil {
			t.Fatal(err)
		}
		if b.Subject.Present || b.Subject.Path != "" || len(b.UsedBy) != 1 || b.UsedBy[0].Project != "acme/app" {
			t.Errorf("briefing = %+v", b)
		}
	})

	t.Run("a subject the workspace leaves out", func(t *testing.T) {
		delete(idx.Projects, "acme/vendor/mirror")
		if _, err := w.buildBriefing(idx, subject{Path: "acme/vendor/mirror"}, directionBoth); err == nil || exitCode(err) != 2 || !strings.Contains(err.Error(), "exclude") {
			t.Errorf("err = %v, want a usage error naming the exclude list", err)
		}
		if _, err := w.buildBriefing(idx, subject{Path: "acme/nope"}, directionBoth); err == nil || exitCode(err) != 2 {
			t.Errorf("err = %v, want a usage error", err)
		}
	})

	t.Run("a project outside the workspace's namespace cannot be fetched into it", func(t *testing.T) {
		idx.Projects["other/tool"] = &contract.Project{ID: 77}
		idx.Links = append(idx.Links, contract.Link{From: "acme/app", To: "other/tool", Kind: contract.KindDepends, Evidence: "go.mod:20"})
		b, err := w.buildBriefing(idx, subject{Path: "acme/app"}, directionUses)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range b.Uses {
			if r.Project == "other/tool" && r.FetchCommand != "" {
				t.Errorf("other/tool has a fetch command (%q) though it has no place in this workspace", r.FetchCommand)
			}
		}
	})
}

func TestBuildBriefingGroup(t *testing.T) {
	w, idx := briefingWorkspace(t, "platform", "app", "platform/svc", "platform/lib")
	idx.Projects["acme/platform/tools"] = &contract.Project{ID: 60, Description: "Tooling"}
	idx.Links = []contract.Link{
		{From: "acme/platform/svc", To: "acme/platform/lib", Kind: contract.KindDepends, Evidence: "go.mod:3"},
		{From: "acme/platform/svc", To: "acme/app", Kind: contract.KindCITrigger, Evidence: ".gitlab-ci.yml:9"},
		{From: "acme/app", To: "acme/platform/lib", Kind: contract.KindDepends, Evidence: "go.mod:7"},
	}

	b, err := w.buildBriefing(idx, subject{Group: true, Path: "acme/platform"}, directionBoth)
	if err != nil {
		t.Fatal(err)
	}
	s := b.Subject
	if s.Kind != "group" || s.Group != "acme/platform" || s.Project != "" || !s.Present || s.RelativePath != "." {
		t.Errorf("subject = %+v", s)
	}
	var members []string
	for _, m := range s.Projects {
		members = append(members, fmt.Sprintf("%s:%v", m.Project, m.Present))
	}
	if got := strings.Join(members, " "); got != "acme/platform/lib:true acme/platform/svc:true acme/platform/tools:false" {
		t.Errorf("members = %s", got)
	}
	if len(b.Within) != 1 || b.Within[0].To != "acme/platform/lib" {
		t.Errorf("within = %+v", b.Within)
	}
	if len(b.Uses) != 1 || b.Uses[0].Project != "acme/app" || b.Uses[0].Links[0].From != "acme/platform/svc" {
		t.Errorf("uses = %+v", b.Uses)
	}
	if len(b.UsedBy) != 1 || b.UsedBy[0].Project != "acme/app" || b.UsedBy[0].Links[0].To != "acme/platform/lib" {
		t.Errorf("used by = %+v", b.UsedBy)
	}

	var sb strings.Builder
	renderBriefing(&sb, b)
	out := sb.String()
	for _, want := range []string{
		"# acme/platform (group)\n",
		"gitlab.example.com · `.` · 3 projects, 2 checked out\n",
		"- acme/platform/svc → acme/platform/lib · depends (go.mod:3)\n",
		// Which of the group's projects a link touches is part of why it matters.
		"- **acme/app** — The app project · `../app` · ci-trigger (acme/platform/svc: .gitlab-ci.yml:9)\n",
		"- **acme/app** — The app project · `../app` · depends (go.mod:7 → acme/platform/lib)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("group briefing missing %q:\n%s", want, out)
		}
	}

	// The whole workspace as a group: everything is inside.
	b, err = w.buildBriefing(idx, subject{Group: true, Path: "acme"}, directionBoth)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Within) != 3 || len(b.Uses) != 0 || len(b.UsedBy) != 0 || b.Subject.RelativePath != ".." {
		t.Errorf("root group: within %d, uses %d, used by %d, at %q", len(b.Within), len(b.Uses), len(b.UsedBy), b.Subject.RelativePath)
	}
}

// TestBriefingStaysReadable: a project with far more relations than anyone
// would read is capped — with the real totals, a pointer to the full list,
// and a Markdown rendering short enough to hand to an agent.
func TestBriefingStaysReadable(t *testing.T) {
	w, idx := briefingWorkspace(t, "app", "app")
	for i := 0; i < 150; i++ {
		dep := fmt.Sprintf("acme/dep-%03d", i)
		user := fmt.Sprintf("acme/user-%03d", i)
		idx.Projects[dep] = &contract.Project{ID: int64(1000 + i)}
		idx.Projects[user] = &contract.Project{ID: int64(2000 + i)}
		idx.Links = append(idx.Links,
			contract.Link{From: "acme/app", To: dep, Kind: contract.KindDepends, Evidence: fmt.Sprintf("go.mod:%d", i+5)},
			contract.Link{From: user, To: "acme/app", Kind: contract.KindDepends, Evidence: "go.mod:9"},
		)
	}
	b, err := w.buildBriefing(idx, subject{Path: "acme/app"}, directionBoth)
	if err != nil {
		t.Fatal(err)
	}
	if b.UsesTotal != 150 || len(b.Uses) != maxBriefingUses || b.UsedByTotal != 150 || len(b.UsedBy) != maxBriefingUsedBy {
		t.Errorf("uses %d/%d, used by %d/%d", len(b.Uses), b.UsesTotal, len(b.UsedBy), b.UsedByTotal)
	}
	notes := strings.Join(b.Notes, "\n")
	for _, want := range []string{"gitty related acme/app --direction=uses", "gitty related acme/app --direction=used-by"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should point at the full list (%q):\n%s", want, notes)
		}
	}
	var sb strings.Builder
	renderBriefing(&sb, b)
	out := sb.String()
	if !strings.Contains(out, "## Uses (150)\n") || !strings.Contains(out, "- … and 90 more\n") || !strings.Contains(out, "- … and 120 more\n") {
		t.Errorf("the rendering should give the totals and say what was left out:\n%s", out)
	}
	if lines := strings.Count(out, "\n"); lines > 200 {
		t.Errorf("briefing is %d lines, want at most 200", lines)
	}

	// Twenty related projects, each with documentation: well under the limit.
	var checkouts []string
	for i := 0; i < 20; i++ {
		checkouts = append(checkouts, fmt.Sprintf("dep-%02d", i))
	}
	w, idx = briefingWorkspace(t, "app", append([]string{"app"}, checkouts...)...)
	for _, c := range checkouts {
		idx.Links = append(idx.Links, contract.Link{From: "acme/app", To: "acme/" + c, Kind: contract.KindDepends, Evidence: "go.mod:3"})
	}
	b, err = w.buildBriefing(idx, subject{Path: "acme/app"}, directionBoth)
	if err != nil {
		t.Fatal(err)
	}
	sb.Reset()
	renderBriefing(&sb, b)
	if lines := strings.Count(sb.String(), "\n"); lines > 60 {
		t.Errorf("a briefing with 20 related projects is %d lines; one line per project and per set of docs is the budget", lines)
	}
}

func TestRenderBriefingDetails(t *testing.T) {
	b := &contract.Briefing{
		Instance: "https://gitlab.example.com",
		Subject:  contract.Subject{Kind: "project", Project: "acme/app", LocalOnly: true, Archived: true},
		Uses: []contract.Related{
			{Project: "acme/a", Description: "Line one\nline two   with   spaces", Present: true, RelativePath: "../a", Fetched: true,
				Links: []contract.Link{{Kind: "depends", Evidence: "go.mod:1"}, {Kind: "ci-include", Evidence: ".gitlab-ci.yml:2"}}},
			{Project: "acme/b", FetchCommand: "gitty fetch acme/b", FetchError: "git fetch failed", Archived: true,
				Links: []contract.Link{{Kind: "image", Evidence: "Dockerfile:1"}}},
			{Project: "other/c", Links: []contract.Link{{Kind: "depends", Evidence: "go.mod:9"}}},
		},
		UsesTotal: 3,
		UsedBy:    []contract.Related{},
		Coverage:  contract.Coverage{Projects: 9, CheckedOut: 2},
		Notes:     []string{"something the reader should know"},
	}
	var sb strings.Builder
	renderBriefing(&sb, b)
	want := strings.Join([]string{
		"# acme/app",
		"gitlab.example.com · *not checked out* · archived · no project upstream",
		"",
		"## Uses (3)",
		"- **acme/a** — Line one line two with spaces · `../a` (fetched) · depends (go.mod:1), ci-include (.gitlab-ci.yml:2)",
		"- **acme/b** · *not checked out* (`gitty fetch acme/b`) · archived · image (Dockerfile:1)",
		"  - fetching it failed: git fetch failed",
		"- **other/c** · *not checked out* · depends (go.mod:9)",
		"",
		"## Used by (0)",
		"- No checked-out project refers to it.",
		"",
		"## Notes",
		"- Only checked-out projects are read for links: 2 of 9 here. \"Used by\" may be incomplete.",
		"- something the reader should know",
		"",
	}, "\n")
	if got := sb.String(); got != want {
		t.Errorf("rendering:\n%s\n--- want:\n%s", got, want)
	}
}

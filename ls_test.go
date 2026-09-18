package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/api/client-go"
)

// lsFixture builds a syncer over a fake source with two groups and three
// projects, one of which is already checked out locally.
func lsFixture(t *testing.T) (*syncer, *bytes.Buffer) {
	t.Helper()
	t.Chdir(t.TempDir())
	mkRepo(t, filepath.Join("acme", "one"))

	src := fakeSource{
		groups: map[string]*gitlab.Group{
			"acme": {FullPath: "acme"},
		},
		subgroups: map[string][]*gitlab.Group{
			"acme": {{FullPath: "acme/team"}},
		},
		projects: map[string][]*gitlab.Project{
			"acme": {
				{PathWithNamespace: "acme/two"},
				{PathWithNamespace: "acme/one"},
				{PathWithNamespace: "acme/team/three"},
			},
		},
	}
	s, stdout, _ := newTestSyncer(&Config{URL: "https://gitlab.com", HTTP: true}, src, (&recordingGit{}).run)
	return s, stdout
}

func TestBuildLsReport(t *testing.T) {
	s, _ := lsFixture(t)

	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatalf("buildLsReport: %v", err)
	}

	if report.Summary.Projects != 3 || report.Summary.Present != 1 || report.Summary.New != 2 {
		t.Errorf("summary = %+v, want projects=3 present=1 new=2", report.Summary)
	}
	if report.Summary.Groups != 2 {
		t.Errorf("groups = %d, want 2", report.Summary.Groups)
	}

	// Groups are sorted, and projects sorted within each group.
	if report.Groups[0].Path != "acme" || report.Groups[1].Path != "acme/team" {
		t.Errorf("group order = %q, %q", report.Groups[0].Path, report.Groups[1].Path)
	}
	if got := report.Groups[0].Projects; len(got) != 2 || got[0].Path != "acme/one" || got[1].Path != "acme/two" {
		t.Errorf("acme projects = %+v, want sorted one,two", got)
	}

	// The locally-present checkout is marked, the others are not.
	if !report.Groups[0].Projects[0].Present {
		t.Error("acme/one exists locally and should be marked present")
	}
	if report.Groups[0].Projects[1].Present {
		t.Error("acme/two does not exist locally and should not be marked present")
	}
}

func TestBuildLsReportListsEmptyGroups(t *testing.T) {
	t.Chdir(t.TempDir())
	src := fakeSource{
		groups:    map[string]*gitlab.Group{"acme": {FullPath: "acme"}},
		subgroups: map[string][]*gitlab.Group{"acme": {{FullPath: "acme/empty"}}},
		projects:  map[string][]*gitlab.Project{},
	}
	s, _, _ := newTestSyncer(&Config{URL: "https://gitlab.com"}, src, (&recordingGit{}).run)

	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatalf("buildLsReport: %v", err)
	}
	if report.Summary.Groups != 2 || report.Summary.Projects != 0 {
		t.Errorf("summary = %+v, want groups=2 projects=0", report.Summary)
	}
	for _, g := range report.Groups {
		if len(g.Projects) != 0 {
			t.Errorf("group %s should have no projects", g.Path)
		}
	}
}

func TestWriteLsTextIsGreppable(t *testing.T) {
	s, stdout := lsFixture(t)
	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	writeLsText(s, report)

	out := stdout.String()
	for _, want := range []string{
		"group acme projects=2\n",
		"project acme/one present\n",
		"project acme/two new\n",
		"group acme/team projects=1\n",
		"project acme/team/three new\n",
		"summary groups=2 projects=3 new=2 present=1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls text output missing %q:\n%s", want, out)
		}
	}
	assertEventLines(t, stdout)
}

func TestWriteLsTreeNestsAndMarks(t *testing.T) {
	s, stdout := lsFixture(t)
	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	writeLsTree(stdout, report, newPalette(false))

	out := stdout.String()
	for _, want := range []string{
		"acme/ (2 projects)",    // the root group, with its project count
		"├── team/ (1 project)", // a subgroup nested beneath it
		"│   └── three  new",    // its project, indented one level further
		"├── one  present",      // a project of the root group
		"└── two  new",          // ...and one that would be cloned
		"2 groups, 3 projects",  // the summary
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls tree output missing %q:\n%s", want, out)
		}
	}
	// Plain output must carry no ANSI escapes at all.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("uncolored tree contained ANSI escapes:\n%q", out)
	}
}

func TestWriteLsTreeColors(t *testing.T) {
	s, stdout := lsFixture(t)
	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	writeLsTree(stdout, report, newPalette(true))

	out := stdout.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("colored tree should contain ANSI escapes:\n%q", out)
	}
	// Every escape sequence opened must be closed.
	if strings.Count(out, "\x1b[") != strings.Count(out, "\x1b[0m")*2 {
		t.Errorf("unbalanced colour codes:\n%q", out)
	}
}

func TestLsReportJSONRoundtrip(t *testing.T) {
	s, _ := lsFixture(t)
	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round lsReport
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Target != "acme" || round.Summary.Projects != 3 || len(round.Groups) != 2 {
		t.Errorf("round-tripped report = %+v", round)
	}
}

func TestResolveLsTarget(t *testing.T) {
	tests := []struct {
		name         string
		arg          string
		rootPath     string
		wantTarget   string
		wantTopLevel bool
	}{
		// At the workspace root there is no current group, so a bare ls (or
		// "." or "/") lists the instance's top-level groups.
		{name: "bare ls at workspace root", arg: "", rootPath: "", wantTopLevel: true},
		{name: "dot at workspace root", arg: ".", rootPath: "", wantTopLevel: true},
		{name: "slash at workspace root", arg: "/", rootPath: "", wantTopLevel: true},

		// Inside a managed subgroup, the current context is that subgroup.
		{name: "bare ls in a subgroup", arg: "", rootPath: "acme/team", wantTarget: "acme/team"},
		{name: "dot in a subgroup", arg: ".", rootPath: "acme/team", wantTarget: "acme/team"},
		{name: "slash from a subgroup is still the root", arg: "/", rootPath: "acme/team", wantTopLevel: true},

		// Relative and absolute arguments, as a shell resolves them.
		{name: "relative from root", arg: "acme", rootPath: "", wantTarget: "acme"},
		{name: "relative from a subgroup", arg: "sub", rootPath: "acme/team", wantTarget: "acme/team/sub"},
		{name: "absolute ignores the context", arg: "/other/group", rootPath: "acme/team", wantTarget: "other/group"},
		{name: "absolute with trailing slash", arg: "/other/", rootPath: "", wantTarget: "other"},

		// Parent traversal, clamped at the instance root.
		{name: "parent of a nested group", arg: "..", rootPath: "acme/team", wantTarget: "acme"},
		{name: "parent of a top-level group is the root", arg: "..", rootPath: "acme", wantTopLevel: true},
		{name: "parent past the root stays at the root", arg: "..", rootPath: "", wantTopLevel: true},
		{name: "dotdot then down", arg: "../other", rootPath: "acme/team", wantTarget: "acme/other"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, topLevel := resolveLsTarget(tc.arg, tc.rootPath)
			if topLevel != tc.wantTopLevel {
				t.Errorf("resolveLsTarget(%q, %q) topLevel = %v, want %v",
					tc.arg, tc.rootPath, topLevel, tc.wantTopLevel)
			}
			if !topLevel && target != tc.wantTarget {
				t.Errorf("resolveLsTarget(%q, %q) target = %q, want %q",
					tc.arg, tc.rootPath, target, tc.wantTarget)
			}
		})
	}
}

func TestBuildTopLevelReport(t *testing.T) {
	t.Chdir(t.TempDir())
	src := fakeSource{topLevel: []*gitlab.Group{
		{FullPath: "zeta"}, {FullPath: "acme"},
	}}
	s, _, _ := newTestSyncer(&Config{URL: "https://gitlab.com", HTTP: true}, src, (&recordingGit{}).run)

	report, err := buildTopLevelReport(s)
	if err != nil {
		t.Fatalf("buildTopLevelReport: %v", err)
	}
	if report.Target != "/" {
		t.Errorf("target = %q, want /", report.Target)
	}
	if report.Summary.Groups != 2 || report.Summary.Projects != 0 {
		t.Errorf("summary = %+v, want groups=2 projects=0", report.Summary)
	}
	// Sorted, and each rendered as its own root in the tree.
	if report.Groups[0].Path != "acme" || report.Groups[1].Path != "zeta" {
		t.Errorf("groups = %+v, want sorted acme,zeta", report.Groups)
	}
	if roots := buildLsTree(report); len(roots) != 2 {
		t.Errorf("top-level groups should each be a tree root, got %d", len(roots))
	}
}

func TestResolveLsFormatAndColor(t *testing.T) {
	// A non-terminal (a pipe) must get the greppable format and no colour,
	// so redirected output stays parseable — the ls(1) convention.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	t.Setenv("NO_COLOR", "")
	os.Unsetenv("NO_COLOR")

	format, err := resolveLsFormat("auto", w)
	if err != nil {
		t.Fatal(err)
	}
	if format != "text" {
		t.Errorf("piped auto format = %q, want text", format)
	}
	on, err := resolveColor("auto", w)
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Error("piped output must not be colorized")
	}

	// Explicit choices win over detection.
	if on, _ := resolveColor("always", w); !on {
		t.Error("--color=always should force colour even when piped")
	}
	if f, _ := resolveLsFormat("tree", w); f != "tree" {
		t.Error("--format=tree should win over detection")
	}

	// NO_COLOR disables colour regardless of destination.
	t.Setenv("NO_COLOR", "1")
	if on, _ := resolveColor("auto", w); on {
		t.Error("NO_COLOR must disable colour")
	}

	// Invalid values are usage errors.
	if _, err := resolveColor("mauve", w); err == nil || exitCode(err) != 2 {
		t.Errorf("bad --color should be a usage error, got %v", err)
	}
	if _, err := resolveLsFormat("yaml", w); err == nil || exitCode(err) != 2 {
		t.Errorf("bad --format should be a usage error, got %v", err)
	}
}

func TestRunLsRejectsBadFormat(t *testing.T) {
	err := runLs(t.Context(), lsOptions{Target: "acme", Format: "yaml"})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("bad --format should be a usage error (exit 2), got %v", err)
	}
}

func TestRunLsRequiresWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runLs(t.Context(), lsOptions{Target: "acme", Format: "text", Anon: true})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("ls without a workspace should be a usage error (exit 2), got %v", err)
	}
}

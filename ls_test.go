package main

import (
	"bytes"
	"encoding/json"
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

func TestWriteLsTreeIndentsByDepth(t *testing.T) {
	s, stdout := lsFixture(t)
	report, err := buildLsReport(s, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	writeLsTree(s, report)

	out := stdout.String()
	for _, want := range []string{
		"acme (2 projects)\n",
		"  = one\n",
		"  + two\n",
		"  team (1 project)\n",
		"    + three\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls tree output missing %q:\n%s", want, out)
		}
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

func TestRunLsRejectsBadFormat(t *testing.T) {
	err := runLs(t.Context(), lsOptions{Path: "acme", Format: "yaml"})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("bad --format should be a usage error (exit 2), got %v", err)
	}
}

func TestRunLsRequiresWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runLs(t.Context(), lsOptions{Path: "acme", Format: "text", Anon: true})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("ls without a workspace should be a usage error (exit 2), got %v", err)
	}
}

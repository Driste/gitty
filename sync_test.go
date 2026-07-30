package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/gitlab-org/api/client-go"
)

func TestGetLocalRelPath(t *testing.T) {
	tests := []struct {
		name        string
		apiFullPath string
		configRoot  string
		want        string
	}{
		{
			name:        "empty root returns full path",
			apiFullPath: "acme/team/repo",
			configRoot:  "",
			want:        "acme/team/repo",
		},
		{
			name:        "strips root prefix on segment boundary",
			apiFullPath: "acme/team/repo",
			configRoot:  "acme/team",
			want:        "repo",
		},
		{
			name:        "root equal to path yields empty",
			apiFullPath: "acme/team",
			configRoot:  "acme/team",
			want:        "",
		},
		{
			name:        "nested remainder is preserved",
			apiFullPath: "acme/team/sub/repo",
			configRoot:  "acme",
			want:        "team/sub/repo",
		},
		{
			// Regression: a prefix that is not a full path segment must not
			// be stripped. "acme/team" is not a parent of "acme/team-x".
			name:        "does not strip a non-boundary prefix",
			apiFullPath: "acme/team-x/repo",
			configRoot:  "acme/team",
			want:        "acme/team-x/repo",
		},
		{
			name:        "unrelated root leaves path untouched",
			apiFullPath: "other/group/repo",
			configRoot:  "acme",
			want:        "other/group/repo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getLocalRelPath(tc.apiFullPath, tc.configRoot)
			if got != tc.want {
				t.Errorf("getLocalRelPath(%q, %q) = %q, want %q", tc.apiFullPath, tc.configRoot, got, tc.want)
			}
		})
	}
}

func TestResolveToken(t *testing.T) {
	tests := []struct {
		name      string
		flagToken string
		gitlabEnv string
		ciEnv     string
		want      string
	}{
		{name: "flag wins over everything", flagToken: "flagtok", gitlabEnv: "envtok", ciEnv: "citok", want: "flagtok"},
		{name: "gitlab env used when no flag", flagToken: "", gitlabEnv: "envtok", ciEnv: "citok", want: "envtok"},
		{name: "ci token used as last resort", flagToken: "", gitlabEnv: "", ciEnv: "citok", want: "citok"},
		{name: "empty when nothing is set", flagToken: "", gitlabEnv: "", ciEnv: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", tc.gitlabEnv)
			t.Setenv("CI_JOB_TOKEN", tc.ciEnv)
			if got := resolveToken(tc.flagToken); got != tc.want {
				t.Errorf("resolveToken(%q) = %q, want %q", tc.flagToken, got, tc.want)
			}
		})
	}
}

func TestIsWithinWorkspace(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{name: "empty is allowed (workspace root)", rel: "", want: true},
		{name: "simple nested path", rel: "acme/team/repo", want: true},
		{name: "current dir marker", rel: ".", want: true},
		{name: "parent escape", rel: "../evil", want: false},
		{name: "deep parent escape", rel: "acme/../../evil", want: false},
		{name: "bare parent", rel: "..", want: false},
		{name: "absolute path", rel: "/etc/passwd", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWithinWorkspace(tc.rel); got != tc.want {
				t.Errorf("isWithinWorkspace(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "https url", raw: "https://gitlab.com/acme/repo.git", want: "gitlab.com"},
		{name: "https url with port", raw: "https://gitlab.example.com:8443/acme/repo.git", want: "gitlab.example.com"},
		{name: "scp-like ssh", raw: "git@gitlab.com:acme/repo.git", want: "gitlab.com"},
		{name: "ssh url scheme", raw: "ssh://git@gitlab.com/acme/repo.git", want: "gitlab.com"},
		{name: "uppercase is normalized", raw: "https://GitLab.COM/acme/repo.git", want: "gitlab.com"},
		{name: "empty", raw: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractHost(tc.raw); got != tc.want {
				t.Errorf("extractHost(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestHostsMatch(t *testing.T) {
	tests := []struct {
		name      string
		configURL string
		cloneURL  string
		want      bool
		wantErr   bool
	}{
		{name: "matching https", configURL: "https://gitlab.com", cloneURL: "https://gitlab.com/acme/repo.git", want: true},
		{name: "matching ssh", configURL: "https://gitlab.com", cloneURL: "git@gitlab.com:acme/repo.git", want: true},
		{name: "mismatched host is rejected", configURL: "https://gitlab.com", cloneURL: "https://evil.example.com/acme/repo.git", want: false},
		{name: "unparseable clone host errors", configURL: "https://gitlab.com", cloneURL: "", want: false, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostsMatch(tc.configURL, tc.cloneURL)
			if tc.wantErr && err == nil {
				t.Errorf("hostsMatch(%q, %q) expected an error, got nil", tc.configURL, tc.cloneURL)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("hostsMatch(%q, %q) unexpected error: %v", tc.configURL, tc.cloneURL, err)
			}
			if got != tc.want {
				t.Errorf("hostsMatch(%q, %q) = %v, want %v", tc.configURL, tc.cloneURL, got, tc.want)
			}
		})
	}
}

// --- Test doubles for the sync logic ---

// fakeSource is an in-memory gitlabSource for exercising syncGroups/syncRepos
// without a live GitLab instance.
type fakeSource struct {
	subgroups map[string][]*gitlab.Group
	groups    map[string]*gitlab.Group
	projects  map[string][]*gitlab.Project
	subErr    error
	projErr   error
}

func (f fakeSource) Subgroups(target string, nested bool) ([]*gitlab.Group, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.subgroups[target], nil
}

func (f fakeSource) Group(target string) (*gitlab.Group, error) {
	if g, ok := f.groups[target]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("group %q not found", target)
}

func (f fakeSource) Projects(target string, nested bool) ([]*gitlab.Project, error) {
	if f.projErr != nil {
		return nil, f.projErr
	}
	return f.projects[target], nil
}

// recordingGit is a gitRunner that records invocations and can be told to fail
// on matching commands.
type recordingGit struct {
	calls  [][]string
	failOn func(dir string, args []string) bool
}

func (r *recordingGit) run(dir string, args ...string) error {
	r.calls = append(r.calls, append([]string{dir}, args...))
	if r.failOn != nil && r.failOn(dir, args) {
		return fmt.Errorf("simulated git failure")
	}
	return nil
}

func TestSyncReposClonesNewProjects(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		git: rec.run,
	}

	if failures := s.syncRepos("acme"); failures != 0 {
		t.Fatalf("syncRepos failures = %d, want 0", failures)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", len(rec.calls), rec.calls)
	}
	got := rec.calls[0]
	// dir, "clone", url, dest
	if got[1] != "clone" || got[2] != "https://gitlab.com/acme/repo.git" || got[3] != "acme/repo" {
		t.Errorf("unexpected clone invocation: %v", got)
	}
}

func TestSyncReposPullsExistingProjects(t *testing.T) {
	t.Chdir(t.TempDir())
	// Pre-create the destination so syncRepos takes the pull branch.
	if err := os.MkdirAll(filepath.Join("acme", "repo"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := &recordingGit{}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		git: rec.run,
	}

	if failures := s.syncRepos("acme"); failures != 0 {
		t.Fatalf("syncRepos failures = %d, want 0", failures)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", len(rec.calls), rec.calls)
	}
	got := rec.calls[0]
	if got[1] != "pull" || got[2] != "--ff-only" {
		t.Errorf("expected 'git pull --ff-only', got: %v", got)
	}
}

func TestSyncReposCountsGitFailures(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{failOn: func(dir string, args []string) bool { return true }}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		git: rec.run,
	}

	if failures := s.syncRepos("acme"); failures != 1 {
		t.Errorf("syncRepos failures = %d, want 1 (git clone failed)", failures)
	}
}

func TestSyncReposRejectsForeignHost(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://evil.example.com/acme/repo.git"},
				},
			},
		},
		git: rec.run,
	}

	failures := s.syncRepos("acme")
	if failures != 1 {
		t.Errorf("syncRepos failures = %d, want 1 (foreign host rejected)", failures)
	}
	if len(rec.calls) != 0 {
		t.Errorf("git should not run for a foreign host, got calls: %v", rec.calls)
	}
}

func TestSyncReposSkipsWorkspaceEscape(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "../evil", HTTPURLToRepo: "https://gitlab.com/evil.git"},
				},
			},
		},
		git: rec.run,
	}

	failures := s.syncRepos("acme")
	if failures != 1 {
		t.Errorf("syncRepos failures = %d, want 1 (escape skipped)", failures)
	}
	if len(rec.calls) != 0 {
		t.Errorf("git should not run for an escaping path, got calls: %v", rec.calls)
	}
}

func TestSyncReposReportsListError(t *testing.T) {
	t.Chdir(t.TempDir())
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{projErr: fmt.Errorf("boom")},
		git: (&recordingGit{}).run,
	}
	if failures := s.syncRepos("acme"); failures != 1 {
		t.Errorf("syncRepos failures = %d, want 1 on list error", failures)
	}
}

func TestSyncGroupsCreatesDirsAndConfigs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			subgroups: map[string][]*gitlab.Group{
				"acme": {{FullPath: "acme/team"}},
			},
			groups: map[string]*gitlab.Group{
				"acme": {FullPath: "acme"},
			},
		},
		git: (&recordingGit{}).run,
	}

	if failures := s.syncGroups("acme"); failures != 0 {
		t.Fatalf("syncGroups failures = %d, want 0", failures)
	}

	// The subgroup directory and its nested config must exist.
	confPath := filepath.Join(dir, "acme", "team", ConfigDir, ConfigName)
	if _, err := os.Stat(confPath); err != nil {
		t.Errorf("expected nested config at %s: %v", confPath, err)
	}
}

func TestSyncGroupsReportsListError(t *testing.T) {
	t.Chdir(t.TempDir())
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com"},
		src: fakeSource{subErr: fmt.Errorf("boom")},
		git: (&recordingGit{}).run,
	}
	if failures := s.syncGroups("acme"); failures != 1 {
		t.Errorf("syncGroups failures = %d, want 1 on list error", failures)
	}
}

func TestSyncReposDryRunMakesNoGitCalls(t *testing.T) {
	t.Chdir(t.TempDir())
	rec := &recordingGit{}
	s := &syncer{
		cfg: &Config{URL: "https://gitlab.com", HTTP: true},
		src: fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		},
		git:    rec.run,
		dryRun: true,
	}
	if failures := s.syncRepos("acme"); failures != 0 {
		t.Errorf("dry-run syncRepos failures = %d, want 0", failures)
	}
	if len(rec.calls) != 0 {
		t.Errorf("dry-run must not invoke git, got: %v", rec.calls)
	}
}

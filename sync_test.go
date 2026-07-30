package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
// on matching commands. Safe for concurrent use.
type recordingGit struct {
	mu     sync.Mutex
	calls  [][]string
	envs   [][]string
	failOn func(dir string, args []string) bool
}

func (r *recordingGit) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{dir}, args...))
	r.envs = append(r.envs, extraEnv)
	r.mu.Unlock()
	if r.failOn != nil && r.failOn(dir, args) {
		return []byte("simulated git output"), fmt.Errorf("simulated git failure")
	}
	return nil, nil
}

func (r *recordingGit) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingGit) lastEnv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.envs) == 0 {
		return nil
	}
	return r.envs[len(r.envs)-1]
}

// newTestSyncer builds a syncer over the given fakes with buffered streams.
func newTestSyncer(cfg *Config, src gitlabSource, git gitRunner) (*syncer, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	s := &syncer{
		cfg:    cfg,
		src:    src,
		git:    git,
		out:    stdout,
		errOut: stderr,
	}
	return s, stdout, stderr
}

var eventLineRe = regexp.MustCompile(`^(clone|pull|group|reclone|skip|error|plan|summary) `)

// assertEventLines fails if any stdout line does not match the event grammar.
func assertEventLines(t *testing.T, stdout *bytes.Buffer) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !eventLineRe.MatchString(line) {
			t.Errorf("stdout line does not match event grammar: %q", line)
		}
	}
}

func TestSyncReposClonesNewProjects(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 0 {
		t.Fatalf("errors = %d, want 0", s.counts.errors)
	}
	if s.counts.cloned != 1 {
		t.Errorf("cloned = %d, want 1", s.counts.cloned)
	}
	if !strings.Contains(stdout.String(), "clone acme/repo\n") {
		t.Errorf("missing clone event:\n%s", stdout.String())
	}
	if rec.callCount() != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", rec.callCount(), rec.calls)
	}
	got := rec.calls[0]
	// dir, "clone", url, dest
	if got[1] != "clone" || got[2] != "https://gitlab.com/acme/repo.git" || got[3] != "acme/repo" {
		t.Errorf("unexpected clone invocation: %v", got)
	}
	assertEventLines(t, stdout)
}

func TestSyncReposPullsExistingProjects(t *testing.T) {
	t.Chdir(t.TempDir())
	// Pre-create a usable checkout (.git/HEAD present) so syncRepos takes the
	// pull branch — a bare empty dir would be classified as clone recovery.
	if err := os.MkdirAll(filepath.Join("acme", "repo", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("acme", "repo", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 0 {
		t.Fatalf("errors = %d, want 0", s.counts.errors)
	}
	if s.counts.pulled != 1 {
		t.Errorf("pulled = %d, want 1", s.counts.pulled)
	}
	if !strings.Contains(stdout.String(), "pull acme/repo\n") {
		t.Errorf("missing pull event:\n%s", stdout.String())
	}
	if rec.callCount() != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", rec.callCount(), rec.calls)
	}
	got := rec.calls[0]
	if got[1] != "pull" || got[2] != "--ff-only" {
		t.Errorf("expected 'git pull --ff-only', got: %v", got)
	}
}

func TestSyncReposCountsGitFailures(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{failOn: func(dir string, args []string) bool { return true }}
	s, stdout, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 1 {
		t.Errorf("errors = %d, want 1 (git clone failed)", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error acme/repo git clone failed\n") {
		t.Errorf("missing error event:\n%s", stdout.String())
	}
	// The captured git output must land on stderr as an attributed block.
	if !strings.Contains(stderr.String(), "--- git clone") || !strings.Contains(stderr.String(), "simulated git output") {
		t.Errorf("missing attributed git failure block on stderr:\n%s", stderr.String())
	}
}

func TestSyncReposRejectsForeignHost(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://evil.example.com/acme/repo.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 1 {
		t.Errorf("errors = %d, want 1 (foreign host rejected)", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error acme/repo clone URL host does not match") {
		t.Errorf("missing host-mismatch error event:\n%s", stdout.String())
	}
	if rec.callCount() != 0 {
		t.Errorf("git should not run for a foreign host, got calls: %v", rec.calls)
	}
}

func TestSyncReposSkipsWorkspaceEscape(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "../evil", HTTPURLToRepo: "https://gitlab.com/evil.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 1 {
		t.Errorf("errors = %d, want 1 (escape skipped)", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error ../evil resolved path escapes the workspace") {
		t.Errorf("missing escape error event:\n%s", stdout.String())
	}
	if rec.callCount() != 0 {
		t.Errorf("git should not run for an escaping path, got calls: %v", rec.calls)
	}
}

func TestSyncReposReportsListError(t *testing.T) {
	t.Chdir(t.TempDir())
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{projErr: fmt.Errorf("boom")},
		(&recordingGit{}).run,
	)
	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 1 {
		t.Errorf("errors = %d, want 1 on list error", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error acme listing projects failed") {
		t.Errorf("missing listing error event:\n%s", stdout.String())
	}
}

func TestSyncGroupsCreatesDirsAndConfigs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			subgroups: map[string][]*gitlab.Group{
				"acme": {{FullPath: "acme/team"}},
			},
			groups: map[string]*gitlab.Group{
				"acme": {FullPath: "acme"},
			},
		},
		(&recordingGit{}).run,
	)

	s.syncGroups(context.Background(), "acme")
	if s.counts.errors != 0 {
		t.Fatalf("errors = %d, want 0", s.counts.errors)
	}
	for _, want := range []string{"group acme\n", "group acme/team\n"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("missing %q event:\n%s", want, stdout.String())
		}
	}

	// The subgroup directory and its nested config must exist.
	confPath := filepath.Join(dir, "acme", "team", ConfigDir, ConfigName)
	if _, err := os.Stat(confPath); err != nil {
		t.Errorf("expected nested config at %s: %v", confPath, err)
	}
}

func TestSyncGroupsReportsListError(t *testing.T) {
	t.Chdir(t.TempDir())
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com"},
		fakeSource{subErr: fmt.Errorf("boom")},
		(&recordingGit{}).run,
	)
	s.syncGroups(context.Background(), "acme")
	if s.counts.errors != 1 {
		t.Errorf("errors = %d, want 1 on list error", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error acme listing subgroups failed") {
		t.Errorf("missing listing error event:\n%s", stdout.String())
	}
}

func TestSyncReposDryRunMakesNoGitCalls(t *testing.T) {
	t.Chdir(t.TempDir())
	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		},
		rec.run,
	)
	s.dryRun = true
	s.syncRepos(context.Background(), "acme")
	if s.counts.errors != 0 {
		t.Errorf("dry-run errors = %d, want 0", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "plan clone acme/repo\n") {
		t.Errorf("missing plan event:\n%s", stdout.String())
	}
	if s.counts.cloned != 1 {
		t.Errorf("dry-run cloned counter = %d, want 1 (summary parity)", s.counts.cloned)
	}
	if rec.callCount() != 0 {
		t.Errorf("dry-run must not invoke git, got: %v", rec.calls)
	}
}

// TestDryRunPlanParity: stripping the "plan " prefix from a dry run's events
// must yield exactly the real run's action events on an identical workspace.
func TestDryRunPlanParity(t *testing.T) {
	src := fakeSource{
		projects: map[string][]*gitlab.Project{
			"acme": {
				{PathWithNamespace: "acme/one", HTTPURLToRepo: "https://gitlab.com/acme/one.git"},
				{PathWithNamespace: "acme/two", HTTPURLToRepo: "https://gitlab.com/acme/two.git"},
			},
		},
	}
	cfg := &Config{URL: "https://gitlab.com", HTTP: true}

	t.Chdir(t.TempDir())
	dry, dryOut, _ := newTestSyncer(cfg, src, (&recordingGit{}).run)
	dry.dryRun = true
	dry.syncRepos(context.Background(), "acme")

	real, realOut, _ := newTestSyncer(cfg, src, (&recordingGit{}).run)
	real.syncRepos(context.Background(), "acme")

	stripped := strings.ReplaceAll(dryOut.String(), "plan ", "")
	if stripped != realOut.String() {
		t.Errorf("dry-run plan does not match real actions:\ndry (stripped):\n%s\nreal:\n%s", stripped, realOut.String())
	}
	if dry.counts != real.counts {
		t.Errorf("dry-run counters %+v differ from real %+v", dry.counts, real.counts)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "pat password masked", raw: "https://oauth2:glpat-secret123@gitlab.com/a/b.git", want: "https://oauth2:xxxxx@gitlab.com/a/b.git"},
		{name: "ci token masked", raw: "https://gitlab-ci-token:citok@gitlab.com/a/b.git", want: "https://gitlab-ci-token:xxxxx@gitlab.com/a/b.git"},
		{name: "credential-free url unchanged", raw: "https://gitlab.com/a/b.git", want: "https://gitlab.com/a/b.git"},
		{name: "scp-like passthrough", raw: "git@gitlab.com:a/b.git", want: "git@gitlab.com:a/b.git"},
		{name: "plain arg passthrough", raw: "--ff-only", want: "--ff-only"},
		{name: "unparseable url replaced", raw: "https://%zz.example.com^bad", want: "<unparseable-url>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURL(tc.raw)
			if got != tc.want {
				t.Errorf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if strings.Contains(got, "glpat-secret123") || strings.Contains(got, "citok") {
				t.Errorf("redactURL(%q) leaked a secret: %q", tc.raw, got)
			}
		})
	}
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"clone", "https://user:tok@gitlab.com/x.git", "x"})
	if got[1] != "https://user:xxxxx@gitlab.com/x.git" {
		t.Errorf("redactArgs did not mask the URL: %v", got)
	}
	if got[0] != "clone" || got[2] != "x" {
		t.Errorf("redactArgs changed non-URL args: %v", got)
	}
}

func TestVerboseExecLinesGoToStderr(t *testing.T) {
	t.Chdir(t.TempDir())
	rec := &recordingGit{}
	s, stdout, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		},
		rec.run,
	)
	s.verbose = true

	s.syncRepos(context.Background(), "acme")
	if !strings.Contains(stderr.String(), "exec git clone") {
		t.Errorf("verbose exec line missing from stderr:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "exec git") {
		t.Errorf("verbose exec line leaked to stdout:\n%s", stdout.String())
	}
	assertEventLines(t, stdout)
}

func TestClassifyDest(t *testing.T) {
	base := t.TempDir()
	mk := func(parts ...string) string { return filepath.Join(append([]string{base}, parts...)...) }

	// repo with .git/HEAD
	repo := mk("repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// worktree-style .git file
	worktree := mk("worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: elsewhere\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// .git dir without HEAD (killed clone)
	headless := mk("headless")
	if err := os.MkdirAll(filepath.Join(headless, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	// empty dir
	empty := mk("empty")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	// non-empty non-repo
	junk := mk("junk")
	if err := os.MkdirAll(junk, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// plain file where a repo should be
	plain := mk("plainfile")
	if err := os.WriteFile(plain, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dest string
		want destState
	}{
		{name: "missing", dest: mk("nope"), want: destMissing},
		{name: "repo with HEAD", dest: repo, want: destRepo},
		{name: "worktree .git file", dest: worktree, want: destRepo},
		{name: "git dir without HEAD is broken", dest: headless, want: destBroken},
		{name: "empty dir", dest: empty, want: destEmptyDir},
		{name: "non-empty non-repo", dest: junk, want: destBroken},
		{name: "plain file", dest: plain, want: destBroken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDest(tc.dest); got != tc.want {
				t.Errorf("classifyDest(%s) = %d, want %d", tc.dest, got, tc.want)
			}
		})
	}
}

func TestRenameAside(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "repo")

	// First aside gets -1; with -1 occupied the next gets -2.
	if err := os.MkdirAll(dest, 0755); err != nil {
		t.Fatal(err)
	}
	aside1, err := renameAside(dest)
	if err != nil {
		t.Fatalf("renameAside: %v", err)
	}
	if aside1 != dest+".gitty-broken-1" {
		t.Errorf("first aside = %q", aside1)
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		t.Fatal(err)
	}
	aside2, err := renameAside(dest)
	if err != nil {
		t.Fatalf("renameAside second: %v", err)
	}
	if aside2 != dest+".gitty-broken-2" {
		t.Errorf("second aside = %q", aside2)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("dest should be gone after renameAside")
	}
}

func TestSyncOneRepoBrokenCheckout(t *testing.T) {
	newBrokenFixture := func(t *testing.T) fakeSource {
		t.Helper()
		if err := os.MkdirAll(filepath.Join("acme", "repo"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("acme", "repo", "junk.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		return fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		}
	}
	cfg := &Config{URL: "https://gitlab.com", HTTP: true}

	t.Run("without flag reports error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		src := newBrokenFixture(t)
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(cfg, src, rec.run)
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "error acme/repo broken checkout") {
			t.Errorf("missing broken-checkout error:\n%s", stdout.String())
		}
		if rec.callCount() != 0 {
			t.Errorf("git must not run on a broken checkout without the flag: %v", rec.calls)
		}
	})

	t.Run("with flag renames aside and reclones", func(t *testing.T) {
		t.Chdir(t.TempDir())
		src := newBrokenFixture(t)
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(cfg, src, rec.run)
		s.recloneBroken = true
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "reclone acme/repo\n") {
			t.Errorf("missing reclone event:\n%s", stdout.String())
		}
		if rec.callCount() != 1 || rec.calls[0][1] != "clone" {
			t.Errorf("expected one clone call, got: %v", rec.calls)
		}
		// The junk must be preserved in the aside dir, not deleted.
		if _, err := os.Stat(filepath.Join("acme", "repo.gitty-broken-1", "junk.txt")); err != nil {
			t.Errorf("aside dir should preserve original contents: %v", err)
		}
	})

	t.Run("dry-run plans the reclone without touching anything", func(t *testing.T) {
		t.Chdir(t.TempDir())
		src := newBrokenFixture(t)
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(cfg, src, rec.run)
		s.recloneBroken = true
		s.dryRun = true
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "plan reclone acme/repo\n") {
			t.Errorf("missing plan reclone event:\n%s", stdout.String())
		}
		if rec.callCount() != 0 {
			t.Errorf("dry-run must not run git: %v", rec.calls)
		}
		if _, err := os.Stat(filepath.Join("acme", "repo", "junk.txt")); err != nil {
			t.Errorf("dry-run must not move the broken dir: %v", err)
		}
	})

	t.Run("empty dir is recovered by cloning", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(filepath.Join("acme", "repo"), 0755); err != nil {
			t.Fatal(err)
		}
		src := fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		}
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(cfg, src, rec.run)
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "clone acme/repo\n") {
			t.Errorf("empty dir should be recovered via clone:\n%s", stdout.String())
		}
		if rec.callCount() != 1 || rec.calls[0][1] != "clone" {
			t.Errorf("expected one clone call, got: %v", rec.calls)
		}
	})
}

func TestSyncReposStopsOnCancelledContext(t *testing.T) {
	t.Chdir(t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingGit{}
	// Cancel the context as soon as the first git call happens.
	rec.failOn = func(dir string, args []string) bool {
		cancel()
		return false
	}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/one", HTTPURLToRepo: "https://gitlab.com/acme/one.git"},
					{PathWithNamespace: "acme/two", HTTPURLToRepo: "https://gitlab.com/acme/two.git"},
					{PathWithNamespace: "acme/three", HTTPURLToRepo: "https://gitlab.com/acme/three.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(ctx, "acme")
	if rec.callCount() != 1 {
		t.Errorf("expected the loop to stop after cancellation: %d git calls", rec.callCount())
	}
}

func TestSyncReposParallelCountsAndEvents(t *testing.T) {
	t.Chdir(t.TempDir())

	var projects []*gitlab.Project
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("acme/repo%02d", i)
		projects = append(projects, &gitlab.Project{
			PathWithNamespace: name,
			HTTPURLToRepo:     "https://gitlab.com/" + name + ".git",
		})
	}

	// Fail every repo whose two-digit suffix ends in 3 (repo03, repo13).
	rec := &recordingGit{failOn: func(dir string, args []string) bool {
		time.Sleep(time.Millisecond) // force worker overlap
		return strings.HasSuffix(args[len(args)-1], "3")
	}}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{projects: map[string][]*gitlab.Project{"acme": projects}},
		rec.run,
	)
	s.jobs = 8

	s.syncRepos(context.Background(), "acme")

	if s.counts.cloned != 18 || s.counts.errors != 2 {
		t.Errorf("counts = %+v, want cloned=18 errors=2", s.counts)
	}
	if rec.callCount() != 20 {
		t.Errorf("git calls = %d, want 20", rec.callCount())
	}
	// Every stdout line must be a complete, well-formed event — torn or
	// interleaved lines fail the grammar check.
	assertEventLines(t, stdout)
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 20 {
		t.Errorf("expected 20 event lines, got %d:\n%s", len(lines), stdout.String())
	}
}

func TestJobsValidation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := runInit("https://gitlab.com", true, false); err != nil {
		t.Fatal(err)
	}

	for _, jobs := range []int{-1, 0, 17} {
		err := runSync(context.Background(), syncOptions{Path: "acme", Anon: true, Jobs: jobs})
		if err == nil || exitCode(err) != 2 {
			t.Errorf("jobs=%d: expected usage error (exit 2), got %v", jobs, err)
		}
	}
}

func TestEventLineFormat(t *testing.T) {
	s, stdout, _ := newTestSyncer(&Config{}, fakeSource{}, (&recordingGit{}).run)
	s.event("clone", "acme/repo")
	s.event("error", "acme/other", "something went wrong")
	s.event("skip", "acme/third", "up-to-date")

	want := "clone acme/repo\nerror acme/other something went wrong\nskip acme/third up-to-date\n"
	if stdout.String() != want {
		t.Errorf("event output:\n%q\nwant:\n%q", stdout.String(), want)
	}
	assertEventLines(t, stdout)
}

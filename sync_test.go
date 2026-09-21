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

func TestConfigAllowsHost(t *testing.T) {
	tests := []struct {
		name       string
		configURL  string
		cloneHosts []string
		remoteURL  string
		want       bool
		wantErr    bool
	}{
		{name: "matching https", configURL: "https://gitlab.com", remoteURL: "https://gitlab.com/acme/repo.git", want: true},
		{name: "matching ssh", configURL: "https://gitlab.com", remoteURL: "git@gitlab.com:acme/repo.git", want: true},
		{name: "mismatched host is rejected", configURL: "https://gitlab.com", remoteURL: "https://evil.example.com/acme/repo.git", want: false},
		{name: "unparseable clone host errors", configURL: "https://gitlab.com", remoteURL: "", want: false, wantErr: true},
		{
			name:       "allowed clone host is accepted",
			configURL:  "https://gitlab.example.com",
			cloneHosts: []string{"git.internal"},
			remoteURL:  "https://git.internal/acme/repo.git",
			want:       true,
		},
		{
			name:       "allowed clone host may be given as a URL",
			configURL:  "https://gitlab.example.com",
			cloneHosts: []string{"https://git.internal/"},
			remoteURL:  "git@git.internal:acme/repo.git",
			want:       true,
		},
		{
			name:       "a host outside the allow list is still rejected",
			configURL:  "https://gitlab.example.com",
			cloneHosts: []string{"git.internal"},
			remoteURL:  "https://evil.example.com/acme/repo.git",
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{URL: tc.configURL, CloneHosts: tc.cloneHosts}
			got, err := cfg.AllowsHost(tc.remoteURL)
			if tc.wantErr && err == nil {
				t.Errorf("AllowsHost(%q) expected an error, got nil", tc.remoteURL)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("AllowsHost(%q) unexpected error: %v", tc.remoteURL, err)
			}
			if got != tc.want {
				t.Errorf("AllowsHost(%q) = %v, want %v", tc.remoteURL, got, tc.want)
			}
		})
	}
}

func TestIsSSHURL(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "https://gitlab.com/acme/repo.git", want: false},
		{raw: "http://gitlab.com/acme/repo.git", want: false},
		{raw: "https://gitlab.example.com:8443/acme/repo.git", want: false},
		{raw: "ssh://git@gitlab.com/acme/repo.git", want: true},
		{raw: "git@gitlab.com:acme/repo.git", want: true},
		{raw: "gitlab.com:acme/repo.git", want: true},
		{raw: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			if got := isSSHURL(tc.raw); got != tc.want {
				t.Errorf("isSSHURL(%q) = %v, want %v", tc.raw, got, tc.want)
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
	topLevel  []*gitlab.Group
	projects  map[string][]*gitlab.Project
	subErr    error
	projErr   error
	topErr    error
}

func (f fakeSource) TopLevelGroups(ctx context.Context) ([]*gitlab.Group, error) {
	if f.topErr != nil {
		return nil, f.topErr
	}
	return f.topLevel, nil
}

func (f fakeSource) Subgroups(ctx context.Context, target string, nested, archived bool) ([]*gitlab.Group, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	if archived {
		return f.subgroups[target], nil
	}
	// The fake has no archived flag on groups (client-go's Group lacks one);
	// a group named with an "-archived" suffix stands in for one.
	var out []*gitlab.Group
	for _, g := range f.subgroups[target] {
		if !strings.HasSuffix(g.FullPath, "-archived") {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f fakeSource) Group(ctx context.Context, target string) (*gitlab.Group, error) {
	if g, ok := f.groups[target]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("group %q not found", target)
}

func (f fakeSource) Projects(ctx context.Context, target string, nested, archived bool) ([]*gitlab.Project, error) {
	if f.projErr != nil {
		return nil, f.projErr
	}
	if !archived {
		var live []*gitlab.Project
		for _, p := range f.projects[target] {
			if !p.Archived {
				live = append(live, p)
			}
		}
		for _, p := range live {
			if p.DefaultBranch == "" {
				p.DefaultBranch = "main"
			}
		}
		return live, nil
	}
	// Real projects report their default branch; fixtures that leave it out
	// get the common one, so a bring-up does not have to ask the remote.
	for _, p := range f.projects[target] {
		if p.DefaultBranch == "" {
			p.DefaultBranch = "main"
		}
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
	// "ls-remote --get-url" is the read-only probe that resolves the local git
	// config's insteadOf rewrites; it runs no network operation and is not a
	// git action, so it is answered (unchanged, i.e. no rewrite configured)
	// without being recorded. Tests that care about rewrites use
	// configAwareGit instead.
	if len(args) == 3 && args[0] == "ls-remote" && args[1] == "--get-url" {
		return []byte(args[2] + "\n"), nil
	}

	r.mu.Lock()
	r.calls = append(r.calls, append([]string{dir}, args...))
	r.envs = append(r.envs, extraEnv)
	r.mu.Unlock()
	if r.failOn != nil && r.failOn(dir, args) {
		return []byte("simulated git output"), fmt.Errorf("simulated git failure")
	}
	return nil, nil
}

// isNetworkGit reports whether a recorded git subcommand contacts a remote.
// Bringing a repository up is several local steps (init, remote add,
// symbolic-ref, checkout) around one fetch; tests reason about the fetch.
func isNetworkGit(sub []string) bool {
	if len(sub) == 0 {
		return false
	}
	switch sub[0] {
	case "fetch", "pull", "clone":
		return true
	case "remote":
		return len(sub) > 1 && sub[1] == "set-head"
	}
	return false
}

// networkCalls returns the recorded invocations that contact a remote.
func (r *recordingGit) networkCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if isNetworkGit(gitSubArgs(c)) {
			out = append(out, c)
		}
	}
	return out
}

// remoteAddURL returns the URL origin was pointed at, or "".
func (r *recordingGit) remoteAddURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if sub := gitSubArgs(c); len(sub) == 4 && sub[0] == "remote" && (sub[1] == "add" || sub[1] == "set-url") {
			return sub[3]
		}
	}
	return ""
}

func (r *recordingGit) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// envOf returns the extra environment of the first recorded invocation of
// the given subcommand, or nil.
func (r *recordingGit) envOf(subcommand string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.calls {
		if sub := gitSubArgs(c); len(sub) > 0 && sub[0] == subcommand {
			return r.envs[i]
		}
	}
	return nil
}

func (r *recordingGit) lastEnv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.envs) == 0 {
		return nil
	}
	return r.envs[len(r.envs)-1]
}

// gitSubArgs strips a recorded invocation down to the git subcommand and its
// arguments: element 0 is the working directory, and gitty may prepend any
// number of "-c key=value" option pairs (the credential-helper reset). Tests
// assert on the subcommand, not on option ordering.
func gitSubArgs(call []string) []string {
	args := call[1:]
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args
}

// findCall returns the first recorded invocation whose subcommand matches.
func (r *recordingGit) findCall(subcommand string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if sub := gitSubArgs(c); len(sub) > 0 && sub[0] == subcommand {
			return c
		}
	}
	return nil
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

var eventLineRe = regexp.MustCompile(`^(clone|pull|group|project|reclone|skip|status|error|plan|summary) `)

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
	// Bring-up: one fetch, inside the new repository, after origin was
	// pointed at the advertised URL.
	net := rec.networkCalls()
	if len(net) != 1 || gitSubArgs(net[0])[0] != "fetch" || net[0][0] != "acme/repo" {
		t.Fatalf("expected one fetch inside acme/repo, got %v (all calls: %v)", net, rec.calls)
	}
	if got := rec.remoteAddURL(); got != "https://gitlab.com/acme/repo.git" {
		t.Errorf("origin URL = %q, want the advertised https URL", got)
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
	// The pull is preceded by a local "config --get remote.origin.url" read,
	// which supplies both the credential host check and the insteadOf pin.
	pull := rec.findCall("pull")
	if pull == nil {
		t.Fatalf("no pull invocation recorded: %v", rec.calls)
	}
	if got := gitSubArgs(pull); len(got) != 2 || got[0] != "pull" || got[1] != "--ff-only" {
		t.Errorf("expected 'git pull --ff-only', got: %v", pull)
	}
}

func TestSyncReposCountsGitFailures(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{failOn: func(dir string, args []string) bool { return isNetworkGit(gitSubArgs(append([]string{dir}, args...))) }}
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
		t.Errorf("errors = %d, want 1 (git fetch failed)", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "error acme/repo git fetch failed\n") {
		t.Errorf("missing error event:\n%s", stdout.String())
	}
	// The captured git output must land on stderr as an attributed block.
	if !strings.Contains(stderr.String(), "fetch --tags origin") || !strings.Contains(stderr.String(), "simulated git output") {
		t.Errorf("missing attributed git failure block on stderr:\n%s", stderr.String())
	}
	// A bring-up that failed is removed again, as a failed git clone is.
	if _, err := os.Stat(filepath.Join("acme", "repo")); !os.IsNotExist(err) {
		t.Errorf("failed bring-up should leave no directory behind (stat err: %v)", err)
	}
}

func TestSyncReposNotesForeignHost(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s, stdout, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {
					{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://other.example.com/acme/repo.git"},
				},
			},
		},
		rec.run,
	)

	s.syncRepos(context.Background(), "acme")

	// gitty reports the unexpected host but does not override git: where a URL
	// ends up is the local git configuration's decision, and gitty cannot see
	// a gitdir-conditional rewrite from outside the repository anyway.
	if s.counts.errors != 0 {
		t.Errorf("errors = %d, want 0 (a foreign host is a note, not a refusal)", s.counts.errors)
	}
	if !strings.Contains(stdout.String(), "clone acme/repo") {
		t.Errorf("expected the clone to proceed:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "other.example.com") {
		t.Errorf("expected a note naming the host:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--allow-clone-host") {
		t.Errorf("note should say how to silence it:\n%s", stderr.String())
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
	if !strings.Contains(stderr.String(), "exec git ") || !strings.Contains(stderr.String(), " fetch ") {
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
		if net := rec.networkCalls(); len(net) != 1 || gitSubArgs(net[0])[0] != "fetch" {
			t.Errorf("expected one fetch for the reclone, got: %v", rec.calls)
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
		if net := rec.networkCalls(); len(net) != 1 || gitSubArgs(net[0])[0] != "fetch" {
			t.Errorf("expected one fetch for the clone, got: %v", rec.calls)
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
	// The repository already being brought up runs its steps to completion
	// (the fake never fails), but no further repository may be started.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, c := range rec.calls {
		for _, a := range c {
			if strings.Contains(a, "acme/two") || strings.Contains(a, "acme/three") {
				t.Fatalf("a repository was started after cancellation: %v", rec.calls)
			}
		}
	}
	if len(rec.calls) == 0 {
		t.Fatal("expected the first repository to have been started")
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
	// The two failing repositories fail at their first (local) step and never
	// reach the network; the other 18 fetch exactly once.
	if n := len(rec.networkCalls()); n != 18 {
		t.Errorf("network git calls = %d, want 18", n)
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
	if err := runInit(initOptions{URL: "https://gitlab.com", HTTP: true}); err != nil {
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

// A repository whose bring-up was cut short — initialised, origin configured,
// perhaps even fetched, but never checked out — is finished on the next run
// rather than pulled, and re-pointed if the project's URL changed meanwhile.
func TestUnbornRepositoryIsResumed(t *testing.T) {
	t.Chdir(t.TempDir())
	dest := filepath.Join("acme", "repo")
	// What `git init` leaves behind: a HEAD, an empty refs/heads, no
	// packed-refs.
	for _, d := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(dest, ".git", d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dest, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !isUnborn(dest) {
		t.Fatal("fixture should classify as unborn")
	}

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{projects: map[string][]*gitlab.Project{
			"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
		}},
		rec.run,
	)
	s.syncRepos(context.Background(), "acme")

	if !strings.Contains(stdout.String(), "pull acme/repo\n") {
		t.Errorf("a resumed bring-up reports as a pull:\n%s", stdout.String())
	}
	for _, c := range rec.calls {
		if sub := gitSubArgs(c); len(sub) > 0 && (sub[0] == "init" || sub[0] == "pull") {
			t.Errorf("resume must neither re-init nor pull an unborn repository: %v", c)
		}
	}
	if net := rec.networkCalls(); len(net) != 1 || gitSubArgs(net[0])[0] != "fetch" {
		t.Errorf("expected exactly one fetch, got %v", rec.calls)
	}
	if u := rec.remoteAddURL(); u != "https://gitlab.com/acme/repo.git" {
		t.Errorf("origin should be (re)pointed at the advertised URL, got %q", u)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("a resumed repository must never be removed: %v", err)
	}
}

func TestIsUnborn(t *testing.T) {
	t.Chdir(t.TempDir())
	mk := func(name string, packed bool, heads ...string) string {
		dir := filepath.Join(name, ".git")
		if err := os.MkdirAll(filepath.Join(dir, "refs", "heads"), 0755); err != nil {
			t.Fatal(err)
		}
		for _, h := range heads {
			if err := os.WriteFile(filepath.Join(dir, "refs", "heads", h), []byte("0000\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if packed {
			if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte("# pack-refs\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return name
	}
	if !isUnborn(mk("fresh", false)) {
		t.Error("no refs at all: want unborn")
	}
	if isUnborn(mk("loose", false, "main")) {
		t.Error("loose branch ref: want born")
	}
	if isUnborn(mk("packed", true)) {
		t.Error("packed refs only (after gc): want born")
	}
	if isUnborn("does-not-exist") {
		t.Error("not a repository: must not be reported unborn")
	}
}

// listPages fetches page 1, then the rest concurrently, keeping API order —
// and follows next-page links serially when the server reports no total.
func TestListPages(t *testing.T) {
	type fetchLog struct {
		mu    sync.Mutex
		pages []int
		max   int
		cur   int
	}
	mk := func(total int, reportTotal bool, failPage int) (func(context.Context, int) ([]string, *gitlab.Response, error), *fetchLog) {
		log := &fetchLog{}
		return func(ctx context.Context, page int) ([]string, *gitlab.Response, error) {
			log.mu.Lock()
			log.pages = append(log.pages, page)
			log.cur++
			if log.cur > log.max {
				log.max = log.cur
			}
			log.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			log.mu.Lock()
			log.cur--
			log.mu.Unlock()
			if page == failPage {
				return nil, nil, fmt.Errorf("page %d exploded", page)
			}
			resp := &gitlab.Response{}
			if page < total {
				resp.NextPage = int64(page + 1)
			}
			if reportTotal {
				resp.TotalPages = int64(total)
			}
			return []string{fmt.Sprintf("p%d-a", page), fmt.Sprintf("p%d-b", page)}, resp, nil
		}, log
	}

	t.Run("parallel with a total, ordered", func(t *testing.T) {
		fetch, log := mk(20, true, 0)
		got, err := listPages(context.Background(), fetch)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 40 || got[0] != "p1-a" || got[2] != "p2-a" || got[39] != "p20-b" {
			t.Errorf("order or count wrong: %v", got)
		}
		if log.max < 2 {
			t.Errorf("pages should be fetched concurrently, max in flight = %d", log.max)
		}
		if log.max > listConcurrency {
			t.Errorf("concurrency %d exceeds bound %d", log.max, listConcurrency)
		}
	})

	t.Run("serial fallback without a total", func(t *testing.T) {
		fetch, log := mk(4, false, 0)
		got, err := listPages(context.Background(), fetch)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 8 || log.max != 1 {
			t.Errorf("want 8 items fetched serially, got %d items, max in flight %d", len(got), log.max)
		}
		if fmt.Sprint(log.pages) != "[1 2 3 4]" {
			t.Errorf("pages = %v, want [1 2 3 4]", log.pages)
		}
	})

	t.Run("single page", func(t *testing.T) {
		fetch, log := mk(1, true, 0)
		got, err := listPages(context.Background(), fetch)
		if err != nil || len(got) != 2 || len(log.pages) != 1 {
			t.Errorf("got %v, err %v, pages %v", got, err, log.pages)
		}
	})

	t.Run("an error surfaces", func(t *testing.T) {
		fetch, _ := mk(10, true, 7)
		if _, err := listPages(context.Background(), fetch); err == nil || !strings.Contains(err.Error(), "page 7") {
			t.Errorf("want the page error, got %v", err)
		}
	})
}

// Archived projects are retired by their owners; a sync leaves them alone
// unless asked, and then treats them like any other project.
func TestSyncSkipsArchivedByDefault(t *testing.T) {
	src := fakeSource{projects: map[string][]*gitlab.Project{
		"acme": {
			{PathWithNamespace: "acme/live", HTTPURLToRepo: "https://gitlab.com/acme/live.git"},
			{PathWithNamespace: "acme/retired", HTTPURLToRepo: "https://gitlab.com/acme/retired.git", Archived: true},
		},
	}}

	t.Run("default", func(t *testing.T) {
		t.Chdir(t.TempDir())
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(&Config{URL: "https://gitlab.com", HTTP: true}, src, rec.run)
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "clone acme/live\n") || strings.Contains(stdout.String(), "acme/retired") {
			t.Errorf("want only the live project:\n%s", stdout.String())
		}
	})

	t.Run("--archived", func(t *testing.T) {
		t.Chdir(t.TempDir())
		rec := &recordingGit{}
		s, stdout, _ := newTestSyncer(&Config{URL: "https://gitlab.com", HTTP: true}, src, rec.run)
		s.includeArchived = true
		s.syncRepos(context.Background(), "acme")
		if !strings.Contains(stdout.String(), "clone acme/retired\n") {
			t.Errorf("--archived should include the archived project:\n%s", stdout.String())
		}
	})
}

// A .gitty/config owned by someone else is refused, since it decides which
// instance receives the user's token.
func TestDiscoverWorkspaceRefusesForeignConfig(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a file owned by another user")
	}
	root := t.TempDir()
	if err := SaveConfigTo(root, &Config{URL: "https://evil.example.com", HTTP: true}); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, ConfigDir, ConfigName)
	if err := os.Chown(conf, 65534, 65534); err != nil {
		t.Skipf("chown: %v", err)
	}
	sub := filepath.Join(root, "acme")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	_, err := DiscoverWorkspace()
	if err == nil || !strings.Contains(err.Error(), "owned by another user") || exitCode(err) != 2 {
		t.Errorf("want a refusal naming the owner problem, got %v", err)
	}
}

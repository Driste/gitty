package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitlab.com/gitlab-org/api/client-go"
)

// configAwareGit is a gitRunner that answers the two read-only queries gitty
// uses to interrogate the local git configuration — "config --get
// remote.origin.url" and "ls-remote --get-url <url>" — and records every
// invocation. rewrite stands in for the user's url.<base>.insteadOf rules.
type configAwareGit struct {
	mu      sync.Mutex
	calls   [][]string
	envs    [][]string
	origin  string
	rewrite func(string) string
}

func (g *configAwareGit) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string{dir}, args...))
	g.envs = append(g.envs, extraEnv)
	origin, rewrite := g.origin, g.rewrite
	g.mu.Unlock()

	switch {
	case len(args) == 3 && args[0] == "config" && args[2] == "remote.origin.url":
		return []byte(origin + "\n"), nil
	case len(args) == 3 && args[0] == "ls-remote" && args[1] == "--get-url":
		if rewrite == nil {
			return []byte(args[2] + "\n"), nil
		}
		return []byte(rewrite(args[2]) + "\n"), nil
	}
	return nil, nil
}

// network returns the recorded invocation that actually contacts a remote,
// skipping the read-only configuration probes.
func (g *configAwareGit) network(t *testing.T) ([]string, []string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, c := range g.calls {
		sub := gitSubArgs(c)
		if len(sub) == 0 {
			continue
		}
		if sub[0] == "config" || sub[0] == "ls-remote" {
			continue
		}
		return c, g.envs[i]
	}
	t.Fatalf("no network git invocation recorded, calls: %v", g.calls)
	return nil, nil
}

func (g *configAwareGit) sawNetworkCall() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if sub := gitSubArgs(c); len(sub) > 0 && sub[0] != "config" && sub[0] != "ls-remote" {
			return true
		}
	}
	return false
}

// hasPin reports whether a recorded invocation carries gitty's identity
// insteadOf override, which defeats the user's own rewrites.
func hasPin(call []string) bool {
	for _, a := range call {
		if strings.HasPrefix(a, "url.") && strings.Contains(a, ".insteadOf=") {
			return true
		}
	}
	return false
}

func oneProject(httpURL string) fakeSource {
	return fakeSource{projects: map[string][]*gitlab.Project{
		"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: httpURL}},
	}}
}

// By default gitty pins the URL it selected, so a stray global insteadOf rule
// cannot turn an HTTP clone into an SSH one.
func TestCloneIsPinnedByDefault(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		oneProject("https://gitlab.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.syncRepos(context.Background(), "acme")

	call, _ := git.network(t)
	if !hasPin(call) {
		t.Errorf("clone should carry the insteadOf pin by default, got %v", call)
	}
	// Nothing should probe git's rewrites when gitty is overriding them.
	git.mu.Lock()
	defer git.mu.Unlock()
	for _, c := range git.calls {
		if sub := gitSubArgs(c); len(sub) > 0 && sub[0] == "ls-remote" {
			t.Errorf("unexpected rewrite probe while pinning: %v", c)
		}
	}
}

// --respect-git-config hands the URL to git unpinned, so the user's
// url.<base>.insteadOf rules apply as they do for a hand-run git clone.
func TestRespectGitConfigDropsThePin(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{
		// The user's rule maps the instance's advertised host onto the one
		// they can actually reach.
		rewrite: func(u string) string {
			return strings.Replace(u, "https://gitlab.example.com/", "https://git.internal/", 1)
		},
	}
	s, _, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true, RespectGitConfig: true, CloneHosts: []string{"git.internal"}},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.syncRepos(context.Background(), "acme")

	call, _ := git.network(t)
	if hasPin(call) {
		t.Errorf("--respect-git-config must not pin the URL, got %v", call)
	}
	sub := gitSubArgs(call)
	if len(sub) < 2 || sub[0] != "clone" || sub[1] != "https://gitlab.example.com/acme/repo.git" {
		t.Errorf("clone should still be handed the advertised URL for git to rewrite, got %v", sub)
	}
	if strings.Contains(stderr.String(), "does not match") {
		t.Errorf("rewritten host should be accepted, stderr: %s", stderr.String())
	}
}

// The clone URL's own host may legitimately differ from the instance's — a
// split API/git deployment — but only for hosts the user named.
func TestCloneHostAllowList(t *testing.T) {
	tests := []struct {
		name       string
		cloneHosts []string
		wantClone  bool
	}{
		{name: "rejected without an allow list", wantClone: false},
		{name: "accepted once allowed", cloneHosts: []string{"git.internal"}, wantClone: true},
		{name: "a different allowed host does not help", cloneHosts: []string{"other.internal"}, wantClone: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			git := &configAwareGit{}
			s, stdout, _ := newTestSyncer(
				&Config{URL: "https://gitlab.example.com", HTTP: true, CloneHosts: tc.cloneHosts},
				oneProject("https://git.internal/acme/repo.git"),
				git.run,
			)
			s.jobs = 1
			s.syncRepos(context.Background(), "acme")

			assertEventLines(t, stdout)
			cloned := git.sawNetworkCall()
			if cloned != tc.wantClone {
				t.Errorf("clone attempted = %v, want %v (stdout: %q)", cloned, tc.wantClone, stdout.String())
			}
			if !tc.wantClone && !strings.Contains(stdout.String(), "error acme/repo clone URL host") {
				t.Errorf("expected a clone URL host error event, got %q", stdout.String())
			}
		})
	}
}

// Honouring the user's git config must not become a way to smuggle the token
// to an unnamed host: the rewritten URL is what gets checked.
func TestRespectGitConfigStillChecksTheRewrittenHost(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{
		rewrite: func(string) string { return "https://evil.example.com/acme/repo.git" },
	}
	s, stdout, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true, RespectGitConfig: true},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.syncRepos(context.Background(), "acme")

	assertEventLines(t, stdout)
	if git.sawNetworkCall() {
		t.Error("a rewrite to an unnamed host must not be cloned")
	}
	if !strings.Contains(stdout.String(), "error acme/repo clone URL host") {
		t.Errorf("expected a host error event, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "evil.example.com") {
		t.Errorf("diagnostic should name the rewritten host, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "local git config rewrote") {
		t.Errorf("diagnostic should say the URL was rewritten, got %q", stderr.String())
	}
}

// A host mismatch should say how to allow the host, once per run.
func TestHostMismatchHintIsPrintedOnce(t *testing.T) {
	t.Chdir(t.TempDir())

	var ps []*gitlab.Project
	for _, name := range []string{"a", "b", "c"} {
		ps = append(ps, &gitlab.Project{
			PathWithNamespace: "acme/" + name,
			HTTPURLToRepo:     "https://git.internal/acme/" + name + ".git",
		})
	}

	git := &configAwareGit{}
	s, _, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true},
		fakeSource{projects: map[string][]*gitlab.Project{"acme": ps}},
		git.run,
	)
	s.jobs = 1
	s.syncRepos(context.Background(), "acme")

	if n := strings.Count(stderr.String(), "--allow-clone-host"); n != 1 {
		t.Errorf("hint printed %d times, want exactly 1:\n%s", n, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--respect-git-config") {
		t.Errorf("hint should mention --respect-git-config, got %q", stderr.String())
	}
}

// A pull inside an existing checkout honours the same rules: the origin is
// read unrewritten, and what git will really contact is what gets checked.
func TestPullRespectsGitConfigRewrite(t *testing.T) {
	t.Chdir(t.TempDir())
	mkRepo(t, "acme/repo")

	git := &configAwareGit{
		origin: "https://gitlab.example.com/acme/repo.git",
		rewrite: func(u string) string {
			return strings.Replace(u, "https://gitlab.example.com/", "https://git.internal/", 1)
		},
	}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true, RespectGitConfig: true, CloneHosts: []string{"git.internal"}},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.cred = credential{token: "t", username: "oauth2"}
	s.exePath = "/bin/gitty"
	s.syncRepos(context.Background(), "acme")

	if !strings.Contains(stdout.String(), "pull acme/repo") {
		t.Fatalf("expected a pull event, got %q", stdout.String())
	}
	call, _ := git.network(t)
	if hasPin(call) {
		t.Errorf("--respect-git-config must not pin the pull URL, got %v", call)
	}
}

// Over HTTP ssh is normally irrelevant, but a rewrite to an SSH URL puts it
// back in the path — where --accept-new-host-keys still has to reach it.
func TestRewriteToSSHStillSteersSSH(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_SSH_COMMAND", "")

	git := &configAwareGit{
		rewrite: func(string) string { return "git@gitlab.example.com:acme/repo.git" },
	}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true, RespectGitConfig: true},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.acceptNewHostKeys = true
	s.syncRepos(context.Background(), "acme")

	_, env := git.network(t)
	if !strings.Contains(strings.Join(env, "\n"), "StrictHostKeyChecking=accept-new") {
		t.Errorf("clone rewritten to SSH lost the ssh option, env: %v", env)
	}
}

// A workspace honouring git rewrites may end up on SSH, so the host-key
// warmup applies even though the configured transport is HTTP.
func TestRespectGitConfigWarmsUpHostKeys(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *Config
		jobs     int
		dryRun   bool
		wantWarm bool
	}{
		{name: "http alone needs no warmup", cfg: &Config{HTTP: true}, jobs: 4},
		{name: "http honouring rewrites warms up", cfg: &Config{HTTP: true, RespectGitConfig: true}, jobs: 4, wantWarm: true},
		{name: "ssh warms up", cfg: &Config{}, jobs: 4, wantWarm: true},
		{name: "serial runs never need it", cfg: &Config{HTTP: true, RespectGitConfig: true}, jobs: 1},
		{name: "dry runs never need it", cfg: &Config{HTTP: true, RespectGitConfig: true}, jobs: 4, dryRun: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &syncer{cfg: tc.cfg, jobs: tc.jobs, dryRun: tc.dryRun}
			if got := s.needsHostKeyWarmup(); got != tc.wantWarm {
				t.Errorf("needsHostKeyWarmup() = %v, want %v", got, tc.wantWarm)
			}
		})
	}
}

// Managed subgroup directories must inherit these settings, or syncing from
// inside one would fail where syncing from the root succeeds.
func TestSubgroupConfigInheritsGitSettings(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{}
	s, _, _ := newTestSyncer(
		&Config{
			URL:              "https://gitlab.example.com",
			HTTP:             true,
			RespectGitConfig: true,
			CloneHosts:       []string{"git.internal"},
		},
		fakeSource{subgroups: map[string][]*gitlab.Group{
			"acme": {{FullPath: "acme/team", Name: "team"}},
		}},
		git.run,
	)
	s.syncGroups(context.Background(), "acme")

	t.Chdir(filepath.Join("acme", "team"))
	sub, err := LoadLocalConfig()
	if err != nil {
		t.Fatalf("loading subgroup config: %v", err)
	}
	if !sub.RespectGitConfig {
		t.Error("subgroup config lost respect_git_config")
	}
	if len(sub.CloneHosts) != 1 || sub.CloneHosts[0] != "git.internal" {
		t.Errorf("subgroup config lost clone_hosts: %v", sub.CloneHosts)
	}
}

// Per-run flags widen the stored config rather than replacing it.
func TestApplyGitOverridesIsAdditive(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", CloneHosts: []string{"a.internal"}}
	cfg.ApplyGitOverrides(false, nil)
	if cfg.RespectGitConfig {
		t.Error("an unset flag must not turn respect_git_config on")
	}
	if len(cfg.CloneHosts) != 1 {
		t.Errorf("clone_hosts changed without a flag: %v", cfg.CloneHosts)
	}

	cfg = &Config{URL: "https://gitlab.example.com", RespectGitConfig: true, CloneHosts: []string{"a.internal"}}
	cfg.ApplyGitOverrides(false, []string{"b.internal"})
	if !cfg.RespectGitConfig {
		t.Error("an unset flag must not turn a stored respect_git_config off")
	}
	if len(cfg.CloneHosts) != 2 {
		t.Errorf("clone_hosts should be additive, got %v", cfg.CloneHosts)
	}
}

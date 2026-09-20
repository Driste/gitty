package main

import (
	"context"
	"fmt"
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
	cloned  string // URL origin was last pointed at, which "origin" then names
	rewrite func(string) string

	failFetch bool   // make the network step fail
	rules     string // what "config --get-regexp url.*insteadof" reports
}

func (g *configAwareGit) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string{dir}, args...))
	g.envs = append(g.envs, extraEnv)
	if sub := gitSubArgs(append([]string{dir}, args...)); len(sub) == 4 && sub[0] == "remote" && (sub[1] == "add" || sub[1] == "set-url") {
		g.cloned = sub[3]
	}
	origin, cloned, rewrite := g.origin, g.cloned, g.rewrite
	g.mu.Unlock()

	switch {
	case len(args) == 3 && args[0] == "config" && args[2] == "remote.origin.url":
		return []byte(origin + "\n"), nil
	case len(args) >= 3 && args[0] == "config" && args[1] == "--show-origin":
		g.mu.Lock()
		rules := g.rules
		g.mu.Unlock()
		return []byte(rules), nil
	case len(args) > 0 && args[0] == "fetch", len(args) > 2 && args[0] == "-c" && args[2] == "fetch":
		g.mu.Lock()
		fail := g.failFetch
		g.mu.Unlock()
		if fail {
			return []byte("fatal: unable to access 'https://gitlab.internal/acme/repo.git/': Could not resolve host\n"), fmt.Errorf("exit status 128")
		}
	case len(args) == 3 && args[0] == "ls-remote" && args[1] == "--get-url":
		// Like git: a remote name resolves to its configured URL first, and
		// the rewrite rules apply to the result.
		u := args[2]
		if u == "origin" {
			u = origin
			if u == "" {
				u = cloned
			}
		}
		if rewrite != nil {
			u = rewrite(u)
		}
		return []byte(u + "\n"), nil
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
		if isNetworkGit(gitSubArgs(c)) {
			return c, g.envs[i]
		}
	}
	t.Fatalf("no network git invocation recorded, calls: %v", g.calls)
	return nil, nil
}

func (g *configAwareGit) sawNetworkCall() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if isNetworkGit(gitSubArgs(c)) {
			return true
		}
	}
	return false
}

// hasPin reports whether a recorded invocation carries an identity insteadOf
// override, which is what gitty used to add to defeat the user's own rewrites.
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
		"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: httpURL, DefaultBranch: "main"}},
	}}
}

// originURL returns the URL origin was pointed at during a bring-up.
func (g *configAwareGit) originURL() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cloned
}

// gitty hands git the advertised URL and adds nothing that would stop the
// user's url.<base>.insteadOf rules from rewriting it.
func TestCloneIsNeverPinned(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{
		// The user's rule maps the instance's advertised host onto the one
		// they can actually reach.
		rewrite: func(u string) string {
			return strings.Replace(u, "https://gitlab.example.com/", "https://git.internal/", 1)
		},
	}
	s, stdout, stderr := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.syncRepos(context.Background(), "acme")

	call, _ := git.network(t)
	if hasPin(call) {
		t.Errorf("gitty must not pin the URL, got %v", call)
	}
	if sub := gitSubArgs(call); len(sub) == 0 || sub[0] != "fetch" || call[0] != "acme/repo" {
		t.Errorf("the network step should be a fetch inside the new repository, got %v", call)
	}
	if got := git.originURL(); got != "https://gitlab.example.com/acme/repo.git" {
		t.Errorf("origin should be the advertised URL for git to rewrite, got %q", got)
	}
	if !strings.Contains(stdout.String(), "clone acme/repo\n") {
		t.Errorf("expected a clone event, got %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "does not match") {
		t.Errorf("a rewritten host must not be rejected, stderr: %s", stderr.String())
	}
}

// Where a URL ends up is the local git config's decision, including a host
// the workspace never heard of.
func TestRewriteDestinationIsFollowed(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{
		rewrite: func(string) string { return "https://somewhere.else/acme/repo.git" },
	}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true},
		oneProject("https://gitlab.example.com/acme/repo.git"),
		git.run,
	)
	s.jobs = 1
	s.cred = credential{token: "t", username: "oauth2"}
	s.exePath = "/bin/gitty"
	s.syncRepos(context.Background(), "acme")

	if !git.sawNetworkCall() {
		t.Errorf("a git-config rewrite must be followed, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "clone acme/repo\n") {
		t.Errorf("expected a clone event, got %q", stdout.String())
	}
}

// An unexpected host is reported but never blocks git. gitty cannot resolve a
// `[includeIf "gitdir:..."]` rewrite from outside the repository, so a refusal
// based on its own guess would break exactly the setups those rewrites serve.
func TestUnexpectedCloneHostIsNotedNotBlocked(t *testing.T) {
	tests := []struct {
		name       string
		cloneHosts []string
		wantNote   bool
	}{
		{name: "unlisted host is noted", wantNote: true},
		{name: "listed host is silent", cloneHosts: []string{"git.internal"}, wantNote: false},
		{name: "a different listed host still notes", cloneHosts: []string{"other.internal"}, wantNote: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			git := &configAwareGit{}
			s, stdout, stderr := newTestSyncer(
				&Config{URL: "https://gitlab.example.com", HTTP: true, CloneHosts: tc.cloneHosts},
				oneProject("https://git.internal/acme/repo.git"),
				git.run,
			)
			s.jobs = 1
			s.syncRepos(context.Background(), "acme")

			assertEventLines(t, stdout)
			if !git.sawNetworkCall() {
				t.Errorf("the clone must always be attempted, stdout: %q", stdout.String())
			}
			if s.counts.errors != 0 {
				t.Errorf("errors = %d, want 0", s.counts.errors)
			}
			noted := strings.Contains(stderr.String(), "--allow-clone-host")
			if noted != tc.wantNote {
				t.Errorf("noted = %v, want %v (stderr: %q)", noted, tc.wantNote, stderr.String())
			}
		})
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
	if !strings.Contains(stderr.String(), "url.insteadOf") {
		t.Errorf("note should say git has the final word, got %q", stderr.String())
	}
}

// A pull inside an existing checkout follows the same rules: the origin is
// read unrewritten and handed to git, which rewrites it as the user configured.
func TestPullFollowsGitConfigRewrite(t *testing.T) {
	t.Chdir(t.TempDir())
	mkRepo(t, "acme/repo")

	git := &configAwareGit{
		origin: "https://gitlab.example.com/acme/repo.git",
		rewrite: func(u string) string {
			return strings.Replace(u, "https://gitlab.example.com/", "https://git.internal/", 1)
		},
	}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.example.com", HTTP: true},
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
		t.Errorf("gitty must not pin the pull URL, got %v", call)
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
		&Config{URL: "https://gitlab.example.com", HTTP: true},
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

// Managed subgroup directories must inherit clone_hosts, or syncing from
// inside one would fail where syncing from the root succeeds.
func TestSubgroupConfigInheritsCloneHosts(t *testing.T) {
	t.Chdir(t.TempDir())

	git := &configAwareGit{}
	s, _, _ := newTestSyncer(
		&Config{
			URL:        "https://gitlab.example.com",
			HTTP:       true,
			CloneHosts: []string{"git.internal"},
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
	if len(sub.CloneHosts) != 1 || sub.CloneHosts[0] != "git.internal" {
		t.Errorf("subgroup config lost clone_hosts: %v", sub.CloneHosts)
	}
}

// The per-run flag widens the stored config rather than replacing it.
func TestAllowCloneHostsIsAdditive(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", CloneHosts: []string{"a.internal"}}
	cfg.AllowCloneHosts(nil)
	if len(cfg.CloneHosts) != 1 {
		t.Errorf("clone_hosts changed without a flag: %v", cfg.CloneHosts)
	}
	cfg.AllowCloneHosts([]string{"b.internal"})
	if len(cfg.CloneHosts) != 2 {
		t.Errorf("clone_hosts should be additive, got %v", cfg.CloneHosts)
	}
}

// Under --verbose gitty reports the origin URL git itself resolved from inside
// the checkout — the one place every part of the user's configuration is in
// effect — so whether a rewrite took effect is visible per repository.
func TestVerboseReportsResolvedOrigin(t *testing.T) {
	t.Run("clone", func(t *testing.T) {
		t.Chdir(t.TempDir())

		git := &configAwareGit{
			rewrite: func(u string) string {
				return strings.Replace(u, "https://gitlab.example.com/", "https://git.internal/", 1)
			},
		}
		s, _, stderr := newTestSyncer(
			&Config{URL: "https://gitlab.example.com", HTTP: true},
			oneProject("https://gitlab.example.com/acme/repo.git"),
			git.run,
		)
		s.jobs = 1
		s.verbose = true
		s.syncRepos(context.Background(), "acme")

		want := "acme/repo: origin https://git.internal/acme/repo.git (rewritten by git config from https://gitlab.example.com/acme/repo.git)"
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing resolved-origin line %q in:\n%s", want, stderr.String())
		}
	})

	t.Run("pull", func(t *testing.T) {
		t.Chdir(t.TempDir())
		mkRepo(t, "acme/repo")

		git := &configAwareGit{origin: "https://gitlab.example.com/acme/repo.git"}
		s, _, stderr := newTestSyncer(
			&Config{URL: "https://gitlab.example.com", HTTP: true},
			oneProject("https://gitlab.example.com/acme/repo.git"),
			git.run,
		)
		s.jobs = 1
		s.verbose = true
		s.syncRepos(context.Background(), "acme")

		if !strings.Contains(stderr.String(), "acme/repo: origin https://gitlab.example.com/acme/repo.git\n") {
			t.Errorf("missing resolved-origin line in:\n%s", stderr.String())
		}
		if strings.Contains(stderr.String(), "rewritten by git config") {
			t.Errorf("no rewrite happened, but one was reported:\n%s", stderr.String())
		}
	})

	t.Run("silent without verbose", func(t *testing.T) {
		t.Chdir(t.TempDir())

		git := &configAwareGit{}
		s, _, stderr := newTestSyncer(
			&Config{URL: "https://gitlab.example.com", HTTP: true},
			oneProject("https://gitlab.example.com/acme/repo.git"),
			git.run,
		)
		s.jobs = 1
		s.syncRepos(context.Background(), "acme")

		if strings.Contains(stderr.String(), "origin https://") {
			t.Errorf("resolved origin should only be reported under --verbose:\n%s", stderr.String())
		}
	})
}

// A failed bring-up fetch is followed by the configuration facts that decide
// whether a rewrite applied, read from inside the repository.
func TestFetchFailureExplainsRewriteRules(t *testing.T) {
	t.Run("no rules visible", func(t *testing.T) {
		t.Chdir(t.TempDir())
		git := &configAwareGit{}
		git.failFetch = true
		s, _, stderr := newTestSyncer(
			&Config{URL: "https://gitlab.example.com", HTTP: true},
			oneProject("https://gitlab.internal/acme/repo.git"),
			git.run,
		)
		s.jobs = 1
		s.syncRepos(context.Background(), "acme")
		got := stderr.String()
		for _, want := range []string{
			"origin https://gitlab.internal/acme/repo.git, which no url.insteadOf rule rewrote",
			"git sees no url.<base>.insteadOf rules inside",
			"includeIf",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})

	t.Run("rules listed with their files", func(t *testing.T) {
		t.Chdir(t.TempDir())
		git := &configAwareGit{}
		git.failFetch = true
		git.rules = "file:/home/u/.gitconfig\turl.https://gitlab.example.com/.insteadof https://other.internal/\n"
		s, _, stderr := newTestSyncer(
			&Config{URL: "https://gitlab.example.com", HTTP: true},
			oneProject("https://gitlab.internal/acme/repo.git"),
			git.run,
		)
		s.jobs = 1
		s.syncRepos(context.Background(), "acme")
		got := stderr.String()
		if !strings.Contains(got, "url.insteadOf rules git sees inside") || !strings.Contains(got, "file:/home/u/.gitconfig") {
			t.Errorf("rules block missing in:\n%s", got)
		}
	})
}

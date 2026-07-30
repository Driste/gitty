package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.com/gitlab-org/api/client-go"
)

// gitlabSource is the subset of the GitLab API that gitty needs. Defining it as
// an interface lets the sync logic be exercised with a fake in tests instead of
// talking to a live GitLab instance. Implementations are responsible for
// aggregating paginated responses into a single slice.
type gitlabSource interface {
	// Subgroups returns the immediate subgroups of target, or every descendant
	// group when nested is true.
	Subgroups(target string, nested bool) ([]*gitlab.Group, error)
	// Group returns the group identified by target.
	Group(target string) (*gitlab.Group, error)
	// Projects returns the projects directly in target, or all projects
	// including those in subgroups when nested is true.
	Projects(target string, nested bool) ([]*gitlab.Project, error)
}

// gitRunner executes a git command in dir with extra environment entries and
// returns its combined output. Injecting it as a function makes the clone/pull
// decision logic testable without shelling out to real git.
type gitRunner func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error)

// syncCounts tallies per-repo outcomes for the run summary. Dry runs advance
// the same counters as real runs (a planned clone counts as cloned), keeping
// the summary identical between the two — the dry-run parity contract.
type syncCounts struct {
	cloned  int
	pulled  int
	skipped int
	errors  int
}

// syncer carries the resolved configuration and collaborators for a single
// sync run. Event lines (primary output) go to out; human diagnostics go to
// errOut. mu makes each emitted line atomic, which the concurrent repo
// workers rely on.
type syncer struct {
	cfg           *Config
	src           gitlabSource
	git           gitRunner
	dryRun        bool
	nested        bool
	verbose       bool
	recloneBroken bool
	jobs          int
	cred          credential
	exePath       string // this binary, for the askpass re-exec

	out    io.Writer
	errOut io.Writer

	mu     sync.Mutex
	counts syncCounts
}

// event emits one machine-readable line on the primary output stream:
//
//	<kind> <path> [detail...]
//
// Kinds: clone, pull, group, reclone, skip, error. Under --dry-run the action
// kinds (clone/pull/group/reclone) are prefixed with "plan " so a dry run's
// output is diffable against a real run's actions.
func (s *syncer) event(kind, path string, detail ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventLocked(kind, path, detail...)
}

// eventLocked is event without locking; callers must hold s.mu.
func (s *syncer) eventLocked(kind, path string, detail ...string) {
	line := kind
	if s.dryRun {
		switch kind {
		case "clone", "pull", "group", "reclone":
			line = "plan " + kind
		}
	}
	line += " " + path
	if len(detail) > 0 {
		line += " " + strings.Join(detail, " ")
	}
	fmt.Fprintln(s.out, line)

	switch kind {
	case "clone", "reclone":
		s.counts.cloned++
	case "pull":
		s.counts.pulled++
	case "skip":
		s.counts.skipped++
	case "error":
		s.counts.errors++
	}
}

// diagf prints a human diagnostic line to stderr.
func (s *syncer) diagf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.errOut, format+"\n", args...)
}

// runGit is the single choke point for git execution: it prints the redacted
// invocation under --verbose, runs git through the injected runner, and on
// failure emits the error event plus an attributed block of the captured git
// output on stderr (on success the block is shown only under --verbose).
func (s *syncer) runGit(ctx context.Context, path, dir string, extraEnv []string, args ...string) error {
	if s.verbose {
		s.diagf("exec git %s (in %s) for %s", strings.Join(redactArgs(args), " "), dir, path)
	}
	out, err := s.git(ctx, dir, extraEnv, args...)
	if err != nil {
		s.reportGitFailure(path, args, out, err)
		return err
	}
	if s.verbose && len(out) > 0 {
		s.mu.Lock()
		fmt.Fprintf(s.errOut, "--- git %s for %s ---\n", strings.Join(redactArgs(args), " "), path)
		s.errOut.Write(out)
		if out[len(out)-1] != '\n' {
			fmt.Fprintln(s.errOut)
		}
		fmt.Fprintf(s.errOut, "--- end %s ---\n", path)
		s.mu.Unlock()
	}
	return nil
}

// gitSubcommand returns the git subcommand from an argv, skipping any
// leading "-c key=value" option pairs, so event lines name the operation
// ("clone", "pull") rather than an option.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return "git"
}

// reportGitFailure emits the stdout error event and the stderr detail block
// under one lock hold so the pair stays adjacent within each stream even when
// repo workers run concurrently.
func (s *syncer) reportGitFailure(path string, args []string, out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventLocked("error", path, "git "+gitSubcommand(args)+" failed")
	fmt.Fprintf(s.errOut, "--- git %s for %s failed: %v ---\n", strings.Join(redactArgs(args), " "), path, err)
	if len(out) > 0 {
		s.errOut.Write(out)
		if out[len(out)-1] != '\n' {
			fmt.Fprintln(s.errOut)
		}
	}
	fmt.Fprintf(s.errOut, "--- end %s ---\n", path)
}

// syncOptions bundles the sync command's flags.
type syncOptions struct {
	Path          string
	Token         string
	DryRun        bool
	Groups        bool
	Repos         bool
	Nested        bool
	Anon          bool
	Verbose       bool
	RecloneBroken bool
	Jobs          int
}

// maxJobs bounds --jobs: beyond ~16 concurrent clones the bottleneck is the
// network or the GitLab server, not gitty.
const maxJobs = 16

// runSync wires up the real GitLab client and git runner and performs a sync.
// It returns an error when setup fails or when one or more groups/repositories
// could not be synced, so the caller can surface a non-zero exit code.
func runSync(ctx context.Context, opts syncOptions) error {
	if !opts.Groups && !opts.Repos {
		opts.Repos = true
	}
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		return usageErrf("no .gitty/config found in this directory; run 'gitty init' first")
	}

	fullTarget := cfg.RootPath
	if opts.Path != "" {
		if fullTarget != "" {
			fullTarget = fullTarget + "/" + opts.Path
		} else {
			fullTarget = opts.Path
		}
	}

	if fullTarget == "" {
		return usageErrf("target group path is empty; provide --path or sync from a managed subgroup directory")
	}

	cred := resolveCredential(opts.Token)
	token := cred.token
	if token == "" && !opts.Anon {
		return usageErrf("a token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required; use --anon to sync public resources without a token")
	}

	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	// Resolve our own binary path up front when HTTP-auth injection may
	// apply, so a broken /proc/self/exe fails the run with a clear error
	// instead of failing every clone mid-sync.
	exePath := ""
	if cfg.HTTP && token != "" {
		exePath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolving gitty's own path for git authentication: %w", err)
		}
	}

	s := &syncer{
		cfg:           cfg,
		src:           gitlabClientSource{client: client},
		git:           execGit,
		dryRun:        opts.DryRun,
		nested:        opts.Nested,
		verbose:       opts.Verbose,
		recloneBroken: opts.RecloneBroken,
		jobs:          opts.Jobs,
		cred:          cred,
		exePath:       exePath,
		out:           os.Stdout,
		errOut:        os.Stderr,
	}

	if s.verbose && s.credentialEnv() != nil {
		s.diagf("HTTP auth: injecting %s credential (username %s) via askpass", s.cred.source, s.cred.username)
	}

	if token == "" {
		s.diagf("Running anonymously (--anon): only public groups and repositories are accessible.")
	}
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	if opts.Groups {
		s.syncGroups(ctx, fullTarget)
	}
	if opts.Repos {
		s.syncRepos(ctx, fullTarget)
	}

	// The summary is always the last stdout line, on success and failure
	// alike, and is identical between dry and real runs (parity contract).
	fmt.Fprintf(s.out, "summary cloned=%d pulled=%d skipped=%d errors=%d\n",
		s.counts.cloned, s.counts.pulled, s.counts.skipped, s.counts.errors)

	// An interrupted run reports 130 even when items had already failed; the
	// workspace is left recoverable and a re-run picks up where this stopped.
	if ctx.Err() != nil {
		s.diagf("interrupted: cloned=%d pulled=%d errors=%d before shutdown; re-run to resume",
			s.counts.cloned, s.counts.pulled, s.counts.errors)
		return errInterrupted
	}
	if s.counts.errors > 0 {
		return &syncFailedError{failures: s.counts.errors}
	}
	return nil
}

// syncGroups fetches subgroups and creates directories with their own
// .gitty/configs. Failures are reported as error events and tallied on the
// syncer's counters.
func (s *syncer) syncGroups(ctx context.Context, target string) {
	s.diagf("--- Syncing Groups ---")
	s.diagf("Fetching subgroups for: '%s' (Nested: %t)...", target, s.nested)

	allGroups, err := s.src.Subgroups(target, s.nested)
	if err != nil {
		s.event("error", target, "listing subgroups failed")
		s.diagf("listing subgroups for %s: %v", target, err)
		return
	}

	if root, err := s.src.Group(target); err == nil && root != nil {
		allGroups = append([]*gitlab.Group{root}, allGroups...)
	}

	s.diagf("Found %d groups to sync.", len(allGroups))

	for _, g := range allGroups {
		if ctx.Err() != nil {
			return
		}
		relPath := getLocalRelPath(g.FullPath, s.cfg.RootPath)
		if !isWithinWorkspace(relPath) {
			s.event("error", g.FullPath, "resolved path escapes the workspace")
			continue
		}
		groupDest := filepath.Join(".", relPath)

		if s.dryRun {
			s.event("group", g.FullPath)
			continue
		}

		if err := os.MkdirAll(groupDest, 0755); err != nil {
			s.event("error", g.FullPath, "creating directory failed")
			s.diagf("creating %s: %v", groupDest, err)
			continue
		}

		subCfg := &Config{
			URL:      s.cfg.URL,
			HTTP:     s.cfg.HTTP,
			RootPath: g.FullPath,
		}
		if err := SaveConfigTo(groupDest, subCfg); err != nil {
			s.event("error", g.FullPath, "saving config failed")
			s.diagf("saving config to %s: %v", groupDest, err)
			continue
		}
		s.event("group", g.FullPath)
	}
}

// syncRepos fetches projects and clones or pulls each one.
func (s *syncer) syncRepos(ctx context.Context, target string) {
	s.diagf("--- Syncing Repositories ---")
	s.diagf("Fetching projects for: '%s' (Nested: %t)...", target, s.nested)

	allProjects, err := s.src.Projects(target, s.nested)
	if err != nil {
		s.event("error", target, "listing projects failed")
		s.diagf("listing projects for %s: %v", target, err)
		return
	}

	s.diagf("Found %d projects.", len(allProjects))

	// Dispatch to a bounded worker pool. jobs=1 preserves serial FIFO
	// behavior; workers rely on the syncer mutex for line-atomic output.
	jobs := s.jobs
	if jobs < 1 {
		jobs = 1
	}
	work := make(chan *gitlab.Project)
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				s.syncOneRepo(ctx, p)
			}
		}()
	}
	for _, p := range allProjects {
		if ctx.Err() != nil {
			break
		}
		select {
		case work <- p:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
}

// destState classifies a repo's local destination path.
type destState int

const (
	destMissing  destState = iota // nothing there: clone
	destRepo                      // usable checkout: pull
	destEmptyDir                  // empty dir (e.g. killed clone): clone into it
	destBroken                    // non-empty non-repo, or repo without HEAD
)

// classifyDest inspects a checkout destination. A usable repo has either a
// .git file (worktrees/submodules) or a .git directory containing HEAD — a
// SIGKILL'd clone can leave .git without HEAD, which pulls would fail on
// forever, so that counts as broken rather than a repo.
func classifyDest(dest string) destState {
	fi, err := os.Stat(dest)
	if os.IsNotExist(err) {
		return destMissing
	}
	if err != nil || !fi.IsDir() {
		return destBroken
	}
	if gfi, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		if !gfi.IsDir() {
			return destRepo
		}
		if _, err := os.Stat(filepath.Join(dest, ".git", "HEAD")); err == nil {
			return destRepo
		}
		return destBroken
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return destBroken
	}
	if len(entries) == 0 {
		return destEmptyDir
	}
	return destBroken
}

// renameAside moves a broken destination to dest+".gitty-broken-<n>" (never
// deleting anything) so a fresh clone can take its place.
func renameAside(dest string) (string, error) {
	for n := 1; n < 100; n++ {
		aside := fmt.Sprintf("%s.gitty-broken-%d", dest, n)
		if _, err := os.Stat(aside); os.IsNotExist(err) {
			if err := os.Rename(dest, aside); err != nil {
				return "", err
			}
			return aside, nil
		}
	}
	return "", fmt.Errorf("too many .gitty-broken directories beside %s", dest)
}

// syncOneRepo handles one project end-to-end and emits its own events.
func (s *syncer) syncOneRepo(ctx context.Context, p *gitlab.Project) {
	// An item already handed to a worker when the run is interrupted is
	// dropped here rather than started.
	if ctx.Err() != nil {
		return
	}

	cloneURL := p.SSHURLToRepo
	if s.cfg.HTTP {
		cloneURL = p.HTTPURLToRepo
	}

	// Calculate destination relative to where we ran the command.
	relPath := getLocalRelPath(p.PathWithNamespace, s.cfg.RootPath)
	if !isWithinWorkspace(relPath) {
		s.event("error", p.PathWithNamespace, "resolved path escapes the workspace")
		return
	}
	repoDest := filepath.Join(".", relPath)

	state := classifyDest(repoDest)

	if state == destRepo {
		// Existing checkout: fast-forward it. --ff-only refuses to create a
		// merge commit, so a diverged or dirty checkout fails loudly instead
		// of leaving the repo in a surprising state.
		if s.dryRun {
			s.event("pull", p.PathWithNamespace)
			return
		}
		env := s.credentialEnv()
		args := []string{"pull", "--ff-only"}
		if env != nil {
			// The URL git will actually use is the checkout's origin, which a
			// user may have re-pointed since the clone: never send our
			// credential toward a host other than the configured instance.
			originOut, err := s.git(ctx, repoDest, nil, "remote", "get-url", "origin")
			if err != nil {
				s.event("error", p.PathWithNamespace, "reading origin remote failed")
				s.diagf("%s: git remote get-url origin: %v", p.PathWithNamespace, err)
				return
			}
			origin := strings.TrimSpace(string(originOut))
			if ok, err := hostsMatch(s.cfg.URL, origin); err != nil || !ok {
				s.event("error", p.PathWithNamespace, "origin host does not match the configured instance")
				s.diagf("%s: origin %q does not match instance %q; not sending credentials", p.PathWithNamespace, redactURL(origin), s.cfg.URL)
				return
			}
			args = append([]string{"-c", "credential.helper="}, args...)
		}
		if err := s.runGit(ctx, p.PathWithNamespace, repoDest, env, args...); err == nil {
			s.event("pull", p.PathWithNamespace)
		}
		return
	}

	if state == destBroken && !s.recloneBroken {
		s.event("error", p.PathWithNamespace, "broken checkout (not a git repo; use --reclone-broken)")
		return
	}

	// Clone (or reclone): verify the clone URL points at the configured
	// instance before handing it to git, so a compromised or misconfigured
	// API response cannot redirect the clone to an attacker-controlled host.
	if ok, err := hostsMatch(s.cfg.URL, cloneURL); err != nil || !ok {
		s.event("error", p.PathWithNamespace, "clone URL host does not match the configured instance")
		s.diagf("%s: clone URL %q does not match instance %q", p.PathWithNamespace, redactURL(cloneURL), s.cfg.URL)
		return
	}

	kind := "clone"
	if state == destBroken {
		kind = "reclone"
	}

	if s.dryRun {
		s.event(kind, p.PathWithNamespace)
		return
	}

	if state == destBroken {
		aside, err := renameAside(repoDest)
		if err != nil {
			s.event("error", p.PathWithNamespace, "moving broken checkout aside failed")
			s.diagf("%s: %v", p.PathWithNamespace, err)
			return
		}
		s.diagf("%s: moved broken checkout aside to %s", p.PathWithNamespace, aside)
	}

	parentDir := filepath.Dir(repoDest)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		s.event("error", p.PathWithNamespace, "creating parent directories failed")
		s.diagf("creating %s: %v", parentDir, err)
		return
	}
	env := s.credentialEnv()
	args := []string{"clone", cloneURL, repoDest}
	if env != nil {
		args = append([]string{"-c", "credential.helper="}, args...)
	}
	if err := s.runGit(ctx, p.PathWithNamespace, ".", env, args...); err == nil {
		s.event(kind, p.PathWithNamespace)
	}
}

// execGit runs a git command with the user's environment (SSH_AUTH_SOCK,
// global ~/.gitconfig, etc.) plus any extra entries, capturing combined
// output so concurrent invocations never interleave on the terminal.
//
// On context cancellation git receives SIGINT rather than the default
// SIGKILL: git cleans up its partially-cloned destination on SIGINT, which is
// what keeps an interrupted workspace recoverable. WaitDelay bounds a git
// that ignores the signal.
func execGit(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// GIT_TERMINAL_PROMPT=0: a bulk sync can never sensibly answer an
	// interactive credential prompt, so a missing/wrong credential fails
	// fast instead of hanging the run (or a CI job) on /dev/tty. Injected
	// credentials arrive via GIT_ASKPASS in extraEnv, which git consults
	// regardless of this setting. extraEnv entries win over os.Environ.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), extraEnv...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

// credential is a resolved GitLab token plus the git username its kind
// requires over HTTP: personal/project access tokens authenticate as
// "oauth2", CI job tokens as "gitlab-ci-token".
type credential struct {
	token    string
	username string
	source   string // "flag" | "GITLAB_TOKEN" | "CI_JOB_TOKEN", for diagnostics
}

// resolveCredential picks the GitLab access token from the --token flag
// first, then the GITLAB_TOKEN environment variable, then CI_JOB_TOKEN — the
// same fixed order resolveToken always had.
func resolveCredential(flagToken string) credential {
	if flagToken != "" {
		return credential{token: flagToken, username: "oauth2", source: "flag"}
	}
	if t := os.Getenv("GITLAB_TOKEN"); t != "" {
		return credential{token: t, username: "oauth2", source: "GITLAB_TOKEN"}
	}
	if t := os.Getenv("CI_JOB_TOKEN"); t != "" {
		return credential{token: t, username: "gitlab-ci-token", source: "CI_JOB_TOKEN"}
	}
	return credential{}
}

// resolveToken returns just the token from the fixed resolution order.
func resolveToken(flagToken string) string {
	return resolveCredential(flagToken).token
}

// credentialEnv builds the extra environment for a git invocation that may
// need HTTP auth: gitty re-execs itself as the askpass helper with the token
// handed over via the child's environment (never argv, never any file). Empty
// when no injection applies (SSH mode, anonymous, or no resolved binary
// path). Callers pair it with the "-c credential.helper=" argv prefix, which
// resets git's helper list so ambient credential managers can neither supply
// stale credentials nor capture this one.
func (s *syncer) credentialEnv() []string {
	if !s.cfg.HTTP || s.cred.token == "" || s.exePath == "" {
		return nil
	}
	return []string{
		"GIT_ASKPASS=" + s.exePath,
		"GITTY_ASKPASS_MODE=1",
		"GITTY_ASKPASS_USERNAME=" + s.cred.username,
		"GITTY_ASKPASS_TOKEN=" + s.cred.token,
	}
}

// getLocalRelPath strips the local context's RootPath from the GitLab API path
// so that folders are built correctly relative to the current directory. The
// prefix is only stripped on a path-segment boundary, so a configRoot of
// "acme/team" does not accidentally match "acme/team-x/repo".
func getLocalRelPath(apiFullPath, configRoot string) string {
	if configRoot == "" {
		return apiFullPath
	}
	if apiFullPath == configRoot {
		return ""
	}
	if rel := strings.TrimPrefix(apiFullPath, configRoot+"/"); rel != apiFullPath {
		return rel
	}
	// configRoot is not a path-segment prefix of apiFullPath; leave it as-is
	// rather than mangling the path.
	return apiFullPath
}

// isWithinWorkspace reports whether a relative destination path stays inside the
// current workspace. It rejects absolute paths and any path that escapes the
// workspace root via "..". This guards against a malicious or misconfigured
// GitLab instance returning namespace paths that would write outside the tree.
func isWithinWorkspace(rel string) bool {
	if rel == "" {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// redactURL returns a URL safe for output: any userinfo password is masked.
// Non-URL strings (scp-like git@host:path, plain paths) carry no embedded
// password and pass through unchanged; a string that looks like a URL but
// cannot be parsed is replaced entirely rather than echoed.
func redactURL(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable-url>"
	}
	return u.Redacted()
}

// redactArgs applies redactURL to every URL-shaped argv element so a git
// invocation can be printed without leaking credentials.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(a, "://") {
			out[i] = redactURL(a)
		} else {
			out[i] = a
		}
	}
	return out
}

// extractHost returns the lower-cased host of a git remote, understanding both
// URL forms (https://host/path, ssh://git@host/path) and the scp-like SSH
// syntax (git@host:path). It returns "" when no host can be determined.
func extractHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		// scp-like syntax: [user@]host:path
		if at := strings.LastIndex(raw, "@"); at != -1 {
			raw = raw[at+1:]
		}
		if colon := strings.Index(raw, ":"); colon != -1 {
			raw = raw[:colon]
		}
		return strings.ToLower(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostsMatch reports whether a clone URL targets the same host as the
// configured GitLab instance. It returns an error when either host cannot be
// determined, which callers treat as a mismatch.
func hostsMatch(configURL, cloneURL string) (bool, error) {
	ch := extractHost(configURL)
	rh := extractHost(cloneURL)
	if ch == "" || rh == "" {
		return false, fmt.Errorf("could not determine host (config %q, clone %q)", configURL, cloneURL)
	}
	return ch == rh, nil
}

// gitlabClientSource adapts a *gitlab.Client to the gitlabSource interface,
// handling pagination for each listing.
type gitlabClientSource struct {
	client *gitlab.Client
}

func (s gitlabClientSource) Subgroups(target string, nested bool) ([]*gitlab.Group, error) {
	var all []*gitlab.Group
	if nested {
		opts := &gitlab.ListDescendantGroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
		}
		for {
			groups, resp, err := s.client.Groups.ListDescendantGroups(target, opts)
			if err != nil {
				return nil, err
			}
			all = append(all, groups...)
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
		return all, nil
	}
	opts := &gitlab.ListSubGroupsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
	}
	for {
		groups, resp, err := s.client.Groups.ListSubGroups(target, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, groups...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func (s gitlabClientSource) Group(target string) (*gitlab.Group, error) {
	g, _, err := s.client.Groups.GetGroup(target, nil)
	return g, err
}

func (s gitlabClientSource) Projects(target string, nested bool) ([]*gitlab.Project, error) {
	var all []*gitlab.Project
	opts := &gitlab.ListGroupProjectsOptions{
		IncludeSubGroups: &nested,
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
			Page:    1,
		},
	}
	for {
		projects, resp, err := s.client.Groups.ListGroupProjects(target, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, projects...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

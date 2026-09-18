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
	// TopLevelGroups returns the instance's top-level groups — the namespaces
	// visible to the caller, with no parent.
	TopLevelGroups() ([]*gitlab.Group, error)
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

	// acceptNewHostKeys maps to ssh's StrictHostKeyChecking=accept-new:
	// unknown hosts are recorded without prompting, a changed key is still
	// refused. Opt-in, because it trades a confirmation prompt for
	// trust-on-first-use.
	acceptNewHostKeys bool

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
	if hint := s.authFailureHint(out); hint != "" {
		fmt.Fprintln(s.errOut, hint)
	}
}

// authFailureHint returns guidance when git's output looks like an HTTP
// authentication or authorization failure. Since gitty clones over HTTP(S),
// the usual cause is a token that can reach the API but not the repositories:
// GitLab's `api` and `read_api` scopes do not grant git access, which
// `read_repository` does. That is easy to misread as "my token is wrong" when
// the same token works fine for listing groups.
func (s *syncer) authFailureHint(out []byte) string {
	if !s.cfg.HTTP || s.cred.token == "" {
		return ""
	}
	lower := strings.ToLower(string(out))
	for _, marker := range []string{
		"authentication failed",
		"http basic: access denied",
		"401 unauthorized",
		"403 forbidden",
		"could not read username",
	} {
		if strings.Contains(lower, marker) {
			return fmt.Sprintf(
				"hint: gitty clones over HTTP(S) and authenticated git with the %s token. "+
					"That token needs the %s scope (or %s) — %s alone lets it list groups "+
					"but not clone. Re-run 'gitty init' to check the token's scopes, or "+
					"'gitty init --force --ssh' to clone with SSH keys instead.",
				s.cred.source, scopeReadRepo, scopeAPI, scopeReadAPI)
		}
	}
	return ""
}

// syncOptions bundles the sync command's flags.
type syncOptions struct {
	Path              string
	Token             string
	DryRun            bool
	Groups            bool
	Repos             bool
	Nested            bool
	Anon              bool
	Verbose           bool
	RecloneBroken     bool
	AcceptNewHostKeys bool
	Jobs              int
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

	s, fullTarget, err := setupWorkspace(opts.Path, opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s.dryRun = opts.DryRun
	s.nested = opts.Nested
	s.verbose = opts.Verbose
	s.recloneBroken = opts.RecloneBroken
	s.acceptNewHostKeys = opts.AcceptNewHostKeys
	s.jobs = opts.Jobs

	if s.verbose && s.credentialEnv() != nil {
		s.diagf("HTTP auth: injecting %s credential (username %s) via askpass", s.cred.source, s.cred.username)
	}

	if s.cred.token == "" {
		s.diagf("Running anonymously (--anon): only public groups and repositories are accessible.")
	}
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}
	if opts.Repos && !opts.DryRun {
		s.warnIfURLRewritten(ctx)
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

	// Sync one repository on its own first so an SSH host-key prompt happens
	// once rather than once per worker (see needsHostKeyWarmup).
	if s.needsHostKeyWarmup() && len(allProjects) > 1 {
		s.syncOneRepo(ctx, allProjects[0])
		allProjects = allProjects[1:]
	}

	// Dispatch to a bounded worker pool. jobs=1 preserves serial FIFO
	// behavior; workers rely on the syncer mutex for line-atomic output.
	forEachConcurrent(ctx, s.jobs, allProjects, func(p *gitlab.Project) {
		s.syncOneRepo(ctx, p)
	})
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
		env, args, ok := s.authForCheckout(ctx, p.PathWithNamespace, repoDest, "pull", "--ff-only")
		if !ok {
			return
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
	env = append(env, s.sshEnv()...)
	// Pin the transport: the URL gitty selected and host-checked must be the
	// URL git actually contacts, whatever insteadOf rules are configured.
	args = append(insteadOfOverride(cloneURL), args...)
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

// resolveCredentialFor applies the same order, except that --anon means
// anonymous: an ambient GITLAB_TOKEN or CI_JOB_TOKEN in the environment is
// ignored rather than silently used. Without this, a stale, expired or
// wrongly-scoped token left in the shell turns an explicitly anonymous run
// into a 401. Combining --anon with an explicit --token is contradictory and
// is reported as a usage error.
func resolveCredentialFor(flagToken string, anon bool) (credential, error) {
	if !anon {
		return resolveCredential(flagToken), nil
	}
	if flagToken != "" {
		return credential{}, usageErrf("--anon and --token are mutually exclusive")
	}
	return credential{}, nil
}

// resolveToken returns just the token from the fixed resolution order.
func resolveToken(flagToken string) string {
	return resolveCredential(flagToken).token
}

// insteadOfOverride returns the "-c url.<u>.insteadOf=<u>" option pair that
// pins a clone or fetch to exactly the URL gitty selected.
//
// Before contacting a remote, git rewrites its URL through any matching
// url.<base>.insteadOf setting. A very common global rule rewrites
// "https://<host>/" to "git@<host>:", which silently turns gitty's --http mode
// into an SSH clone: the injected HTTP credentials never apply, ssh asks for
// host-key confirmation instead, and CI runners without SSH keys fail. It also
// defeats the clone-URL host check, since gitty would be validating a URL that
// git then replaces.
//
// git resolves insteadOf by longest matching prefix, so mapping the full URL
// to itself outranks any shorter host-level rule and leaves the transport the
// user asked for intact.
func insteadOfOverride(rawURL string) []string {
	if rawURL == "" {
		return nil
	}
	return []string{"-c", "url." + rawURL + ".insteadOf=" + rawURL}
}

// warnIfURLRewritten notes once, on stderr, that the local git configuration
// would redirect the configured instance to another transport, and that gitty
// is overriding it for this run. "git ls-remote --get-url" resolves insteadOf
// without contacting the network.
func (s *syncer) warnIfURLRewritten(ctx context.Context) {
	probe := strings.TrimSuffix(s.cfg.URL, "/") + "/"
	out, err := s.git(ctx, ".", nil, "ls-remote", "--get-url", probe)
	if err != nil {
		return
	}
	got := strings.TrimSpace(string(out))
	if got == "" || got == probe {
		return
	}
	s.diagf("note: local git config rewrites %s to %s (url.insteadOf); gitty is overriding that so the URL it selected is the URL git uses",
		probe, redactURL(got))
}

// authForCheckout prepares a network git command to run inside an existing
// checkout: it returns the environment and the final argv. When credentials
// would be injected it first verifies the checkout's own origin still points
// at the configured instance — a user may have re-pointed it since the clone,
// and the token must never travel to another host. ok=false means the caller
// must not run the command (an error event was emitted).
func (s *syncer) authForCheckout(ctx context.Context, path, dir string, args ...string) ([]string, []string, bool) {
	env := s.credentialEnv()

	// Read the checkout's configured origin: it is both what the credential
	// host check applies to and what git would rewrite via insteadOf.
	//
	// "git config --get remote.origin.url" is deliberate: "git remote get-url"
	// resolves insteadOf and would hand back the already-rewritten URL, so
	// pinning that would pin the very rewrite we mean to override.
	originOut, err := s.git(ctx, dir, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		s.event("error", path, "reading origin remote failed")
		s.diagf("%s: git config --get remote.origin.url: %v", path, err)
		return nil, nil, false
	}
	origin := strings.TrimSpace(string(originOut))

	if env != nil {
		if ok, err := hostsMatch(s.cfg.URL, origin); err != nil || !ok {
			s.event("error", path, "origin host does not match the configured instance")
			s.diagf("%s: origin %q does not match instance %q; not sending credentials", path, redactURL(origin), s.cfg.URL)
			return nil, nil, false
		}
		args = append([]string{"-c", "credential.helper="}, args...)
	} else {
		// Nothing to protect (SSH mode, or anonymous HTTP), but ssh may still
		// need steering.
		env = s.sshEnv()
	}

	return env, append(insteadOfOverride(origin), args...), true
}

// sshEnv returns the extra environment that steers ssh for SSH-mode clones,
// pulls, and fetches. It is empty unless gitty has something to say: over HTTP
// ssh is not involved at all, and without --accept-new-host-keys gitty leaves
// ssh's host-key policy exactly as the user configured it.
//
// A GIT_SSH_COMMAND the user already set is preserved and extended, so a
// custom ssh binary or existing options keep working.
func (s *syncer) sshEnv() []string {
	if s.cfg.HTTP || !s.acceptNewHostKeys {
		return nil
	}
	base := strings.TrimSpace(os.Getenv("GIT_SSH_COMMAND"))
	if base == "" {
		base = "ssh"
	}
	return []string{"GIT_SSH_COMMAND=" + base + " -o StrictHostKeyChecking=accept-new"}
}

// needsHostKeyWarmup reports whether the first repository should be synced on
// its own before the worker pool starts.
//
// Over SSH the first connection to a host whose key is not yet in known_hosts
// prompts for confirmation, and ssh reads that answer straight from the
// terminal. If every worker starts at once they all reach that prompt before
// any of them has recorded the accepted key, so the user is asked once per
// repository for the same fingerprint — and the concurrent appends to
// known_hosts can lose each other's writes. Syncing one repository first lets
// that happen exactly once.
func (s *syncer) needsHostKeyWarmup() bool {
	return !s.cfg.HTTP && !s.dryRun && s.jobs > 1
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
	// authenticated records whether a token was supplied, which decides
	// whether a top-level listing can be scoped to the caller's memberships.
	authenticated bool
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

// maxTopLevelPages bounds the top-level group listing. An unauthenticated
// listing on a large instance would otherwise walk every public group on it.
const maxTopLevelPages = 10

func (s gitlabClientSource) TopLevelGroups() ([]*gitlab.Group, error) {
	var all []*gitlab.Group
	topLevel := true
	opts := &gitlab.ListGroupsOptions{
		TopLevelOnly: &topLevel,
		ListOptions:  gitlab.ListOptions{PerPage: 100, Page: 1},
	}
	// With credentials, restrict the listing to namespaces the caller is
	// actually a member of — "my groups" is the useful answer, and an
	// unrestricted listing returns every group visible on the instance.
	if s.authenticated {
		level := gitlab.GuestPermissions
		opts.MinAccessLevel = &level
	}
	for page := 0; page < maxTopLevelPages; page++ {
		groups, resp, err := s.client.Groups.ListGroups(opts)
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

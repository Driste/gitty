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
	Subgroups(ctx context.Context, target string, nested bool) ([]*gitlab.Group, error)
	// Group returns the group identified by target.
	Group(ctx context.Context, target string) (*gitlab.Group, error)
	// Projects returns the projects directly in target, or all projects
	// including those in subgroups when nested is true.
	Projects(ctx context.Context, target string, nested bool) ([]*gitlab.Project, error)
	// TopLevelGroups returns the instance's top-level groups — the namespaces
	// visible to the caller, with no parent.
	TopLevelGroups(ctx context.Context) ([]*gitlab.Group, error)
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

	// hintOnce keeps the "how to allow this host" guidance to a single line
	// per run instead of repeating it for every rejected repository.
	hintOnce sync.Once

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

	// AllowCloneHosts adds trusted clone hosts for this run only; see
	// Config.AllowCloneHosts.
	AllowCloneHosts []string
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
	s.cfg.AllowCloneHosts(opts.AllowCloneHosts)

	if s.verbose {
		// Enough to tell which binary, which git and whose configuration a
		// run used — the three things a "gitty ignores my gitconfig" report
		// needs settled first.
		gitVersion := "git (version unknown)"
		if out, err := s.git(ctx, ".", nil, "--version"); err == nil {
			gitVersion = strings.TrimSpace(string(out))
		}
		s.diagf("gitty %s, %s, HOME=%s", versionString(), gitVersion, os.Getenv("HOME"))
	}
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

	allGroups, root, err := s.groupsOf(ctx, target)
	if err != nil {
		s.event("error", target, "listing subgroups failed")
		s.diagf("listing subgroups for %s: %v", target, err)
		return
	}
	if root != nil {
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

		// Every field but root_path is inherited, so syncing from inside a
		// managed subgroup directory behaves exactly like syncing from the
		// workspace root.
		subCfg := &Config{
			URL:        s.cfg.URL,
			HTTP:       s.cfg.HTTP,
			RootPath:   g.FullPath,
			CloneHosts: append([]string(nil), s.cfg.CloneHosts...),
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

	allProjects, err := s.src.Projects(ctx, target, s.nested)
	if err != nil {
		s.event("error", target, "listing projects failed")
		s.diagf("listing projects for %s: %v", target, err)
		return
	}

	s.diagf("Found %d projects.", len(allProjects))

	// Sync one repository on its own first so an SSH host-key prompt happens
	// once rather than once per worker (see needsHostKeyWarmup).
	if len(allProjects) > 1 && s.needsHostKeyWarmup(ctx, ".", s.cloneURL(allProjects[0])) {
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

	cloneURL := s.cloneURL(p)

	// Calculate destination relative to where we ran the command.
	relPath := getLocalRelPath(p.PathWithNamespace, s.cfg.RootPath)
	if !isWithinWorkspace(relPath) {
		s.event("error", p.PathWithNamespace, "resolved path escapes the workspace")
		return
	}
	repoDest := filepath.Join(".", relPath)

	state := classifyDest(repoDest)

	if state == destRepo {
		if s.dryRun {
			s.event("pull", p.PathWithNamespace)
			return
		}
		// A repository with no commits yet is one whose bring-up was cut
		// short (or whose remote is empty): finish it rather than pull it.
		if isUnborn(repoDest) {
			if s.bringUp(ctx, p, repoDest, cloneURL, false) == nil {
				s.event("pull", p.PathWithNamespace)
				s.reportResolvedOrigin(ctx, p.PathWithNamespace, repoDest, cloneURL)
			}
			return
		}
		// Existing checkout: fast-forward it. --ff-only refuses to create a
		// merge commit, so a diverged or dirty checkout fails loudly instead
		// of leaving the repo in a surprising state.
		env, args, ok := s.authForCheckout(ctx, p.PathWithNamespace, repoDest, "pull", "--ff-only")
		if !ok {
			return
		}
		if err := s.runGit(ctx, p.PathWithNamespace, repoDest, env, args...); err == nil {
			s.event("pull", p.PathWithNamespace)
			s.reportResolvedOrigin(ctx, p.PathWithNamespace, repoDest, cloneURL)
		}
		return
	}

	if state == destBroken && !s.recloneBroken {
		s.event("error", p.PathWithNamespace, "broken checkout (not a git repo; use --reclone-broken)")
		return
	}

	// Clone (or reclone).
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
	if err := s.bringUp(ctx, p, repoDest, cloneURL, true); err == nil {
		s.event(kind, p.PathWithNamespace)
		s.reportResolvedOrigin(ctx, p.PathWithNamespace, repoDest, cloneURL)
	}
}

// bringUp brings a repository into being the way `git clone` does — init,
// configure origin, fetch, check out the default branch — but in that order,
// so that the one step which touches the network runs inside a fully-formed
// repository with its remote already configured.
//
// That ordering is the point. git evaluates conditional includes —
// `[includeIf "gitdir:..."]`, `[includeIf "hasconfig:remote.*.url:..."]` —
// against the repository it is operating on, and `git clone` reads the user's
// configuration before the repository it is creating exists. Whether a
// url.<base>.insteadOf rule living in such an include reaches the clone's
// fetch is then a matter of git's version and internals. Fetching from inside
// the repository removes the question: the user's configuration applies to
// gitty's fetch exactly as it applies to a `git fetch` they run in that
// checkout themselves. It also means the URL gitty reports and steers ssh by
// is the one git resolved, not a guess made from the workspace root.
//
// fresh says whether dest is being created by this call. A fresh repository
// that fails part-way is removed again, as `git clone` removes its
// destination; one that was already there (an unborn repository from an
// earlier interrupted run, or an empty remote) is left for the next run to
// resume, which is what a killed `git clone` leaves behind too.
func (s *syncer) bringUp(ctx context.Context, p *gitlab.Project, dest, url string, fresh bool) (err error) {
	path := p.PathWithNamespace
	if fresh {
		if err := s.runGit(ctx, path, ".", nil, "init", "-q", dest); err != nil {
			return err
		}
		defer func() {
			if err != nil && ctx.Err() == nil {
				// Failed rather than interrupted: leave nothing half-made.
				// An interrupt keeps the partial repository so the next run
				// resumes it without repeating the fetch's work.
				os.RemoveAll(dest)
			}
		}()
	}

	// Point origin at the advertised URL. On a resume it may already be
	// there, possibly stale if the project moved.
	if current, cerr := s.originOf(ctx, dest); cerr != nil || current == "" {
		if err := s.runGit(ctx, path, dest, nil, "remote", "add", "origin", url); err != nil {
			return err
		}
	} else if current != url {
		if err := s.runGit(ctx, path, dest, nil, "remote", "set-url", "origin", url); err != nil {
			return err
		}
	}

	// From inside the repository git resolves the URL with the user's full
	// configuration in effect, so this is exact — the basis for the ssh
	// steering and the foreign-host note.
	effective := s.effectiveURL(ctx, dest, "origin")
	if effective == "origin" {
		effective = url
	}
	s.noteForeignHost("clone URL", url, effective)
	env := s.credentialEnv()
	fetch := []string{"fetch", "--tags", "origin"}
	if env != nil {
		fetch = append([]string{"-c", "credential.helper="}, fetch...)
	}
	env = append(env, s.sshEnvFor(effective)...)
	if err := s.runGit(ctx, path, dest, env, fetch...); err != nil {
		s.explainFetchFailure(ctx, path, dest, url, effective)
		return err
	}

	// Check out the default branch. GitLab reports it; when it does not (an
	// older instance, or a project with no commits) ask the remote, which
	// costs one more round trip and fails cleanly on an empty repository —
	// which then stays unborn, as `git clone` leaves it.
	branch := p.DefaultBranch
	if branch == "" {
		if err := s.runGit(ctx, path, dest, env, "remote", "set-head", "origin", "--auto"); err != nil {
			return nil
		}
		out, err := s.git(ctx, dest, nil, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return nil
		}
		branch = strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
		if branch == "" {
			return nil
		}
	} else {
		// Mirror what clone records, so `git remote show origin` and
		// origin/HEAD behave the same in a gitty checkout.
		_ = s.runGit(ctx, path, dest, nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
	}
	// checkout of a branch that only exists at origin creates the local
	// tracking branch, exactly as clone would have.
	return s.runGit(ctx, path, dest, nil, "checkout", "-q", branch)
}

// explainFetchFailure follows a failed bring-up fetch with the facts that
// decide whether the user's URL rewrites applied: the URL origin was given,
// the URL git resolved it to from inside the repository, and every
// url.<base>.insteadOf rule git can see from there, with the file each came
// from. Read from inside dest, this is the configuration that governed the
// fetch — conditional includes and all — so an absent or non-matching rule
// here is the whole explanation, and a present one that did not apply points
// at its prefix.
func (s *syncer) explainFetchFailure(ctx context.Context, path, dest, url, effective string) {
	if effective != url {
		s.diagf("%s: origin %s, which git resolved to %s", path, redactURL(url), redactURL(effective))
	} else {
		s.diagf("%s: origin %s, which no url.insteadOf rule rewrote", path, redactURL(url))
	}
	out, _ := s.git(ctx, dest, nil, "config", "--show-origin", "--get-regexp", `^url\..*\.insteadof$`)
	rules := strings.TrimSpace(string(out))
	if rules == "" {
		s.diagf("%s: git sees no url.<base>.insteadOf rules inside %s (HOME=%s); a rule in an includeIf section whose condition does not match this repository is not loaded",
			path, dest, os.Getenv("HOME"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.errOut, "--- url.insteadOf rules git sees inside %s ---\n", dest)
	for _, line := range strings.Split(rules, "\n") {
		// "file:<path>\t<key> <value>": redact token by token, since the
		// value may carry credentials but the line as a whole is not a URL.
		fmt.Fprintln(s.errOut, strings.Join(redactArgs(strings.Fields(line)), " "))
	}
	fmt.Fprintf(s.errOut, "--- end %s ---\n", path)
}

// isUnborn reports whether a repository has no commits on any branch: the
// state `git init` leaves, and the state a bring-up interrupted before its
// checkout leaves. Refs are kept either as loose files under refs/heads or
// packed into packed-refs; a repository with neither has never had a commit
// checked in.
func isUnborn(dest string) bool {
	gitDir := filepath.Join(dest, ".git")
	if _, err := os.Stat(filepath.Join(gitDir, "packed-refs")); err == nil {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(gitDir, "refs", "heads"))
	if err != nil {
		return false // not a shape we understand; treat as a normal checkout
	}
	return len(entries) == 0
}

// reportResolvedOrigin says, under --verbose, which URL git actually used for
// a checkout's origin — resolved by git itself, from inside the repository,
// where every part of the user's configuration is in effect. That is the
// ground truth no pre-clone guess can be, and the quickest way to confirm
// whether a url.<base>.insteadOf rule took effect on a given repository.
func (s *syncer) reportResolvedOrigin(ctx context.Context, path, dir, advertised string) {
	if !s.verbose {
		return
	}
	resolved := s.effectiveURL(ctx, dir, "origin")
	if resolved == "origin" {
		return // the probe failed; nothing trustworthy to report
	}
	if advertised != "" && resolved != advertised {
		s.diagf("%s: origin %s (rewritten by git config from %s)", path, redactURL(resolved), redactURL(advertised))
		return
	}
	s.diagf("%s: origin %s", path, redactURL(resolved))
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

// effectiveURL is a best-effort answer to "where will git actually send this
// URL", after the local git configuration's url.<base>.insteadOf rules.
// "git ls-remote --get-url" performs that resolution without touching the
// network.
//
// It is advisory only, and deliberately never gates a git invocation. git
// resolves a `[includeIf "gitdir:..."]` section against the repository it is
// operating on, so a rule living in such an include is invisible from anywhere
// that is not that repository — including the workspace root, where a clone
// starts. A clone still picks the rule up (git creates the gitdir, then
// fetches), which is exactly the case where predicting the URL up front gets
// it wrong. So gitty uses this to steer ssh and to say something useful on
// stderr, and lets git decide where to go.
//
// dir should be the repository the command will run in, when there is one.
// If the probe fails, rawURL is the best available answer.
func (s *syncer) effectiveURL(ctx context.Context, dir, rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	out, err := s.git(ctx, dir, nil, "ls-remote", "--get-url", rawURL)
	if err != nil {
		return rawURL
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		return got
	}
	return rawURL
}

// noteForeignHost reports, once per run, that repositories are being cloned or
// fetched from a host other than the configured instance — and that the token
// goes there with them.
//
// It is a note rather than a refusal. gitty cannot know where git will end up
// before git runs (see effectiveURL), and the local git config is the
// authority on that; blocking on a guess breaks the legitimate setups where an
// instance advertises URLs on an external host that the user's own config
// rewrites to an internal one. Listing the host with --allow-clone-host says
// "yes, I know" and silences this.
func (s *syncer) noteForeignHost(what, rawURL, effective string) {
	if ok, err := s.cfg.AllowsHost(effective); ok && err == nil {
		return
	}
	s.hintOnce.Do(func() {
		s.diagf("note: %s %s is not on the configured instance %s; git decides where that really goes (your url.insteadOf rules apply, including ones in conditional includes that are only visible from inside a repository) and any token travels with it",
			what, redactURL(effective), s.cfg.URL)
		s.diagf("hint: --verbose shows the URL git actually resolved for each repository; if this host is expected, list it with --allow-clone-host=<host> to silence this note")
	})
}

// warnIfURLRewritten notes once, on stderr, that the local git configuration
// redirects the configured instance somewhere else, so a surprising transport
// is visible rather than silent.
func (s *syncer) warnIfURLRewritten(ctx context.Context) {
	probe := strings.TrimSuffix(s.cfg.URL, "/") + "/"
	got := s.effectiveURL(ctx, ".", probe)
	if got == probe {
		return
	}
	s.diagf("note: local git config rewrites %s to %s (url.insteadOf); gitty is following that",
		probe, redactURL(got))
}

// authForCheckout prepares a network git command to run inside an existing
// checkout: it returns the environment and the final argv. Running inside the
// repository is what lets git apply that repository's own configuration,
// including `[includeIf "gitdir:..."]` sections. ok=false means the caller must
// not run the command (an error event was emitted).
func (s *syncer) authForCheckout(ctx context.Context, path, dir string, args ...string) ([]string, []string, bool) {
	env := s.credentialEnv()

	// Read the checkout's configured origin, unrewritten: it is what git will
	// rewrite via insteadOf when it contacts the remote.
	origin, err := s.originOf(ctx, dir)
	if err != nil {
		s.event("error", path, "reading origin remote failed")
		s.diagf("%s: git config --get remote.origin.url: %v", path, err)
		return nil, nil, false
	}

	// Resolve what git will really contact. Inside the checkout this sees the
	// repository's full configuration, gitdir-conditional includes and all.
	effective := s.effectiveURL(ctx, dir, origin)

	if env != nil {
		s.noteForeignHost("origin", origin, effective)
		args = append([]string{"-c", "credential.helper="}, args...)
	}
	// ssh may still need steering: always in SSH mode, and over HTTP when the
	// user's git config rewrites this remote to an SSH URL.
	env = append(env, s.sshEnvFor(effective)...)

	return env, args, true
}

// sshEnv returns the extra environment that steers ssh for SSH-mode clones,
// pulls, and fetches. It is empty unless gitty has something to say: over HTTP
// ssh is not involved at all, and without --accept-new-host-keys gitty leaves
// ssh's host-key policy exactly as the user configured it.
func (s *syncer) sshEnv() []string { return s.sshEnvFor("") }

// sshEnvFor is sshEnv for a remote whose effective URL is known. Over HTTP,
// ssh is normally not involved — but a workspace honouring the local git
// config may have its HTTP URL rewritten to an SSH one, in which case ssh is
// in the path after all and still needs steering.
//
// A GIT_SSH_COMMAND the user already set is preserved and extended, so a
// custom ssh binary or existing options keep working.
func (s *syncer) sshEnvFor(effectiveURL string) []string {
	if !s.acceptNewHostKeys {
		return nil
	}
	if s.cfg.HTTP && !isSSHURL(effectiveURL) {
		return nil
	}
	base := strings.TrimSpace(os.Getenv("GIT_SSH_COMMAND"))
	if base == "" {
		base = "ssh"
	}
	return []string{"GIT_SSH_COMMAND=" + base + " -o StrictHostKeyChecking=accept-new"}
}

// needsHostKeyWarmup reports whether the first repository should be handled on
// its own before the worker pool starts, given a representative remote URL.
//
// Over SSH the first connection to a host whose key is not yet in known_hosts
// prompts for confirmation, and ssh reads that answer straight from the
// terminal. If every worker starts at once they all reach that prompt before
// any of them has recorded the accepted key, so the user is asked once per
// repository for the same fingerprint — and the concurrent appends to
// known_hosts can lose each other's writes. Handling one repository first lets
// that happen exactly once.
//
// An HTTP workspace can land on ssh too, when the local git config rewrites
// its URLs, so the decision follows the effective URL rather than the
// configured transport. sampleURL is resolved through those rewrites; an empty
// one means "unknown", which falls back to the configured transport.
func (s *syncer) needsHostKeyWarmup(ctx context.Context, dir, sampleURL string) bool {
	if s.dryRun || s.jobs <= 1 {
		return false
	}
	if !s.cfg.HTTP {
		return true
	}
	return isSSHURL(s.effectiveURL(ctx, dir, sampleURL))
}

// originOf returns a checkout's configured origin URL, unrewritten.
//
// "git config --get" is deliberate: "git remote get-url" applies insteadOf
// itself and would hand back the already-rewritten URL, while callers here
// want the raw one so they can resolve it themselves and compare the two.
func (s *syncer) originOf(ctx context.Context, dir string) (string, error) {
	out, err := s.git(ctx, dir, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// cloneURL returns the remote URL gitty would hand git for a project, per the
// workspace's configured transport. The local git config may still rewrite it.
func (s *syncer) cloneURL(p *gitlab.Project) string {
	if s.cfg.HTTP {
		return p.HTTPURLToRepo
	}
	return p.SSHURLToRepo
}

// isSSHURL reports whether a git remote URL uses an SSH transport, in either
// the ssh:// form or the scp-like [user@]host:path form.
func isSSHURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if strings.Contains(raw, "://") {
		return strings.HasPrefix(raw, "ssh://") || strings.HasPrefix(raw, "git+ssh://")
	}
	return strings.Contains(raw, ":")
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

// groupsOf lists a target's subgroups and fetches the target group itself,
// concurrently: the two are independent round trips. A missing root is not
// an error — the caller may lack permission to read the group object while
// still being able to list beneath it.
func (s *syncer) groupsOf(ctx context.Context, target string) (groups []*gitlab.Group, root *gitlab.Group, err error) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		groups, err = s.src.Subgroups(ctx, target, s.nested)
	}()
	go func() {
		defer wg.Done()
		root, _ = s.src.Group(ctx, target)
	}()
	wg.Wait()
	return groups, root, err
}

// listConcurrency bounds how many pages of one listing are fetched at once.
// GitLab's per-IP limits leave ample room for this, and it turns a listing of
// N pages into roughly N/8 sequential round trips.
const listConcurrency = 8

// listPages collects every page of a paginated GitLab listing. It requests the
// first page, learns the page count from it, and fetches the remaining pages
// concurrently — the work of a listing is almost entirely round-trip latency,
// so pages are the unit worth parallelising. Results keep the API's order.
// When the server does not report a total (GitLab omits it beyond 10,000
// rows) it follows next-page links one at a time instead. The first error
// cancels the rest.
func listPages[T any](ctx context.Context, fetch func(ctx context.Context, page int) ([]T, *gitlab.Response, error)) ([]T, error) {
	first, resp, err := fetch(ctx, 1)
	if err != nil {
		return nil, err
	}
	total := int(resp.TotalPages)
	if total <= 1 {
		all := first
		for page := int(resp.NextPage); page != 0; {
			items, r, err := fetch(ctx, page)
			if err != nil {
				return nil, err
			}
			all = append(all, items...)
			page = int(r.NextPage)
		}
		return all, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pages := make([][]T, total+1)
	pages[1] = first
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, listConcurrency)
	for page := 2; page <= total; page++ {
		wg.Add(1)
		go func(page int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			items, _, err := fetch(ctx, page)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			pages[page] = items
		}(page)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var all []T
	for _, p := range pages {
		all = append(all, p...)
	}
	return all, nil
}

// gitlabClientSource adapts a *gitlab.Client to the gitlabSource interface,
// handling pagination for each listing.
type gitlabClientSource struct {
	client *gitlab.Client
	// authenticated records whether a token was supplied, which decides
	// whether a top-level listing can be scoped to the caller's memberships.
	authenticated bool
}

func (s gitlabClientSource) Subgroups(ctx context.Context, target string, nested bool) ([]*gitlab.Group, error) {
	if nested {
		return listPages(ctx, func(ctx context.Context, page int) ([]*gitlab.Group, *gitlab.Response, error) {
			return s.client.Groups.ListDescendantGroups(target, &gitlab.ListDescendantGroupsOptions{
				ListOptions: gitlab.ListOptions{PerPage: 100, Page: int64(page)},
			}, gitlab.WithContext(ctx))
		})
	}
	return listPages(ctx, func(ctx context.Context, page int) ([]*gitlab.Group, *gitlab.Response, error) {
		return s.client.Groups.ListSubGroups(target, &gitlab.ListSubGroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: int64(page)},
		}, gitlab.WithContext(ctx))
	})
}

func (s gitlabClientSource) Group(ctx context.Context, target string) (*gitlab.Group, error) {
	// The group object alone: by default GitLab embeds the group's projects
	// (and shared projects) in this response, which for a large group is
	// hundreds of kilobytes and seconds of server time, none of it used here.
	withProjects := false
	g, _, err := s.client.Groups.GetGroup(target, &gitlab.GetGroupOptions{WithProjects: &withProjects}, gitlab.WithContext(ctx))
	return g, err
}

// maxTopLevelPages bounds the top-level group listing. An unauthenticated
// listing on a large instance would otherwise walk every public group on it.
const maxTopLevelPages = 10

func (s gitlabClientSource) TopLevelGroups(ctx context.Context) ([]*gitlab.Group, error) {
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
		groups, resp, err := s.client.Groups.ListGroups(opts, gitlab.WithContext(ctx))
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

func (s gitlabClientSource) Projects(ctx context.Context, target string, nested bool) ([]*gitlab.Project, error) {
	// simple=true trims each project to its identifying fields — path, URLs,
	// default branch — which is all gitty reads, and is a fifth of the bytes
	// (and a fraction of the server time) of the full representation.
	simple := true
	return listPages(ctx, func(ctx context.Context, page int) ([]*gitlab.Project, *gitlab.Response, error) {
		return s.client.Groups.ListGroupProjects(target, &gitlab.ListGroupProjectsOptions{
			IncludeSubGroups: &nested,
			Simple:           &simple,
			ListOptions:      gitlab.ListOptions{PerPage: 100, Page: int64(page)},
		}, gitlab.WithContext(ctx))
	})
}

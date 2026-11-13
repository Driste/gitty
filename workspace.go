package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"gitlab.com/gitlab-org/api/client-go"
)

// newWorkspaceSyncer loads the workspace config and resolves credentials,
// returning a syncer wired to the real GitLab client and git runner. It does
// not resolve a target group: commands that take one (sync) add it via
// setupWorkspace, while ls resolves its own argument shell-style.
func newWorkspaceSyncer(tokenFlag string, anon bool) (*syncer, error) {
	cfg, err := DiscoverWorkspace()
	if err != nil {
		return nil, err
	}
	return newSyncerFor(cfg, tokenFlag, anon, true)
}

// errTokenRequired is what a command that must reach the API says when it
// has no credential and was not told to go without one.
func errTokenRequired() error {
	return usageErrf("a token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required; use --anon to access public resources without a token")
}

// newSyncerFor builds a syncer for a config that has already been found.
// needToken says whether having neither a token nor --anon is an error now;
// commands that may never touch the network decide that later, when they do
// (see workspace.requireAPI).
func newSyncerFor(cfg *Config, tokenFlag string, anon, needToken bool) (*syncer, error) {
	cred, err := resolveCredentialFor(tokenFlag, anon)
	if err != nil {
		return nil, err
	}
	if cred.token == "" && !anon && needToken {
		return nil, errTokenRequired()
	}

	client, err := gitlab.NewClient(cred.token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		return nil, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	exePath, err := askpassPath(cfg, cred)
	if err != nil {
		return nil, err
	}

	return &syncer{
		cfg:     cfg,
		src:     gitlabClientSource{client: client, authenticated: cred.token != ""},
		git:     execGit,
		jobs:    1,
		cred:    cred,
		exePath: exePath,
		out:     os.Stdout,
		errOut:  os.Stderr,
	}, nil
}

// workspace is what the commands that act on a whole workspace — index,
// related, context, fetch — run in.
//
// The group commands (sync, ls, status) run in the directory of the group
// they were started in, with that group as their root. These commands keep
// one answer per workspace, so they run at its top instead: the process
// moves to the workspace root, the syncer's root_path is the root's
// namespace, and every path gitty computes is relative to that. Where the
// user actually was is remembered, because it decides which project a bare
// `gitty sync --related` is about.
type workspace struct {
	s *syncer

	Root     string // absolute directory of the workspace root
	StartDir string // absolute directory the command was started in
	Group    string // namespace path of the group StartDir is in
	Project  string // namespace path of the checkout StartDir is in, or ""

	anon bool
}

// openWorkspace locates the workspace around the current directory and
// moves to its root. It does not insist on a credential: see requireAPI.
func openWorkspace(tokenFlag string, anon bool) (*workspace, error) {
	start, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining working directory: %w", err)
	}
	loc, err := locateWorkspace(start)
	if err != nil {
		return nil, err
	}
	root, rootCfg := workspaceRoot(loc.ConfigDir, loc.Config)

	// The root's config anchors the workspace; a group directory's own
	// config may add hosts and exclusions on top of it.
	cfg := *rootCfg
	cfg.CloneHosts = mergeStrings(rootCfg.CloneHosts, loc.Config.CloneHosts)
	cfg.Exclude = mergeStrings(rootCfg.Exclude, loc.Config.Exclude)

	s, err := newSyncerFor(&cfg, tokenFlag, anon, false)
	if err != nil {
		return nil, err
	}

	w := &workspace{s: s, Root: root, StartDir: start, anon: anon}
	groupDir, below := contextBelow(root, start)
	w.Group = cfg.RootPath
	if below != "" {
		w.Group = path.Join(cfg.RootPath, below)
	}
	// contextBelow stops at the first checkout on the way down: if there is
	// one, the command was started inside that project.
	if rel, err := filepath.Rel(groupDir, start); err == nil && rel != "." {
		first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if classifyDest(filepath.Join(groupDir, first)) == destRepo {
			w.Project = path.Join(w.Group, first)
		}
	}

	if err := os.Chdir(root); err != nil {
		return nil, fmt.Errorf("entering %s: %w", root, err)
	}
	return w, nil
}

// requireAPI reports whether this run may talk to the instance: it has a
// token, or was told to go without one. Commands that read the index call it
// only at the point they find they need the network, so a warm index is
// usable offline and without credentials.
func (w *workspace) requireAPI() error {
	if w.s.cred.token == "" && !w.anon {
		return errTokenRequired()
	}
	return nil
}

// dirOf returns a project's directory relative to the workspace root (where
// the process is), and whether that stays inside the workspace.
func (w *workspace) dirOf(project string) (string, bool) {
	rel := getLocalRelPath(project, w.s.cfg.RootPath)
	if rel == "" || !isWithinWorkspace(rel) {
		return "", false
	}
	// A project outside the root's namespace has nowhere to go: its path
	// would land under a directory that stands for a different group.
	if w.s.cfg.RootPath != "" && rel == project {
		return "", false
	}
	return filepath.FromSlash(rel), true
}

// absDir is dirOf as an absolute path.
func (w *workspace) absDir(rel string) string {
	return filepath.Join(w.Root, rel)
}

// relToStart expresses an absolute path relative to where the command was
// started, for output a person reads from that directory.
func (w *workspace) relToStart(abs string) string {
	rel, err := filepath.Rel(w.StartDir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// mergeStrings returns a followed by the entries of b it does not have.
func mergeStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// setupWorkspace is newWorkspaceSyncer plus the target group a sync needs:
// the workspace's root_path, joined with an optional --path.
func setupWorkspace(pathFlag, tokenFlag string, anon bool) (*syncer, string, error) {
	s, err := newWorkspaceSyncer(tokenFlag, anon)
	if err != nil {
		return nil, "", err
	}

	target := s.cfg.RootPath
	if pathFlag != "" {
		if target != "" {
			target = target + "/" + pathFlag
		} else {
			target = pathFlag
		}
	}
	if target == "" {
		return nil, "", usageErrf("target group path is empty; provide --path or run from a managed subgroup directory")
	}
	return s, target, nil
}

// askpassPath resolves this binary's path for the git askpass re-exec, but
// only when HTTP credential injection can actually apply. Resolving up front
// means a broken /proc/self/exe fails the run with a clear error instead of
// failing every git invocation mid-run.
func askpassPath(cfg *Config, cred credential) (string, error) {
	if !cfg.HTTP || cred.token == "" {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving gitty's own path for git authentication: %w", err)
	}
	return exe, nil
}

// forEachConcurrent runs fn over items with at most jobs workers. The feeder
// stops handing out work as soon as ctx is cancelled, so an interrupted run
// drains promptly instead of queueing the remainder.
func forEachConcurrent[T any](ctx context.Context, jobs int, items []T, fn func(T)) {
	if jobs < 1 {
		jobs = 1
	}
	work := make(chan T)
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				fn(item)
			}
		}()
	}
	for _, item := range items {
		if ctx.Err() != nil {
			break
		}
		select {
		case work <- item:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
}

// brokenAsideMarker names the directories renameAside creates; they are
// archived copies, not live checkouts, so workspace walks skip them.
const brokenAsideMarker = ".gitty-broken-"

// findRepos walks root and returns every git checkout beneath it, as paths
// relative to root using forward slashes. It does not descend into a repo once
// found (submodules and vendored checkouts are part of their parent), and
// skips gitty's own config dirs and moved-aside (broken or orphaned) checkouts.
func findRepos(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			// Unreadable entries are skipped rather than failing the walk:
			// one bad directory must not hide the rest of the workspace.
			return nil //nolint:nilerr
		}
		base := filepath.Base(path)
		if path != root && (base == ConfigDir || strings.Contains(base, brokenAsideMarker) || strings.Contains(base, orphanAsideMarker)) {
			return filepath.SkipDir
		}
		if classifyDest(path) == destRepo {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil {
				repos = append(repos, filepath.ToSlash(rel))
			}
			return filepath.SkipDir
		}
		return nil
	})
	return repos, err
}

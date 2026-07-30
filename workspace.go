package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gitlab.com/gitlab-org/api/client-go"
)

// setupWorkspace loads the workspace config, resolves the effective target
// group (root_path joined with an optional --path), resolves credentials, and
// builds a syncer wired to the real GitLab client and git runner. It is shared
// by every command that talks to the GitLab API.
func setupWorkspace(pathFlag, tokenFlag string, anon bool) (*syncer, string, error) {
	cfg, err := LoadLocalConfig()
	if err != nil {
		return nil, "", usageErrf("no .gitty/config found in this directory; run 'gitty init' first")
	}

	target := cfg.RootPath
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

	cred := resolveCredential(tokenFlag)
	if cred.token == "" && !anon {
		return nil, "", usageErrf("a token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required; use --anon to access public resources without a token")
	}

	client, err := gitlab.NewClient(cred.token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create GitLab client: %w", err)
	}

	exePath, err := askpassPath(cfg, cred)
	if err != nil {
		return nil, "", err
	}

	return &syncer{
		cfg:     cfg,
		src:     gitlabClientSource{client: client},
		git:     execGit,
		jobs:    1,
		cred:    cred,
		exePath: exePath,
		out:     os.Stdout,
		errOut:  os.Stderr,
	}, target, nil
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
// skips gitty's own config dirs and moved-aside broken checkouts.
func findRepos(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			// Unreadable entries are skipped rather than failing the walk:
			// one bad directory must not hide the rest of the workspace.
			return nil //nolint:nilerr
		}
		base := filepath.Base(path)
		if path != root && (base == ConfigDir || strings.Contains(base, brokenAsideMarker)) {
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

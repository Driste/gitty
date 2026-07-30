package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// statusOptions bundles the status command's flags.
type statusOptions struct {
	Token   string
	Anon    bool
	Fetch   bool
	Verbose bool
	Jobs    int
}

// repoStatus is one checkout's branch and freshness, as reported by
// `git status --porcelain=v2 --branch`.
type repoStatus struct {
	Path     string
	Branch   string
	Upstream bool
	Ahead    int
	Behind   int
	Dirty    bool
}

// detachedBranch is what git reports for a checkout with no current branch.
const detachedBranch = "(detached)"

// parseStatusPorcelainV2 extracts the branch header fields and working-tree
// state from `git status --porcelain=v2 --branch` output. Any line that is not
// a header describes a changed, unmerged, or untracked path, so its presence
// means the tree is dirty.
func parseStatusPorcelainV2(out []byte) repoStatus {
	st := repoStatus{Branch: detachedBranch}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			st.Dirty = true
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "branch.head":
			st.Branch = fields[2]
		case "branch.upstream":
			st.Upstream = true
		case "branch.ab":
			// "# branch.ab +<ahead> -<behind>"
			if len(fields) >= 4 {
				st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[2], "+"))
				st.Behind, _ = strconv.Atoi(strings.TrimPrefix(fields[3], "-"))
			}
		}
	}
	return st
}

// statusDetail renders the key=value fields of a status event line.
func statusDetail(st repoStatus) string {
	detail := fmt.Sprintf("branch=%s ahead=%d behind=%d dirty=%t",
		st.Branch, st.Ahead, st.Behind, st.Dirty)
	if !st.Upstream {
		// Without an upstream, ahead/behind are unknowable rather than zero;
		// say so explicitly so consumers do not read 0/0 as "in sync".
		detail += " upstream=none"
	}
	return detail
}

// runStatus reports the branch and freshness of every checkout in the
// workspace. It needs no GitLab API access; --fetch refreshes remote-tracking
// refs first, which does require credentials for HTTP remotes.
func runStatus(ctx context.Context, opts statusOptions) error {
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		return usageErrf("no .gitty/config found in this directory; run 'gitty init' first")
	}

	cred := resolveCredential(opts.Token)
	if opts.Fetch && cred.token == "" && !opts.Anon {
		return usageErrf("--fetch needs a token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var); use --anon to fetch public repositories without one")
	}
	exePath, err := askpassPath(cfg, cred)
	if err != nil {
		return err
	}

	s := &syncer{
		cfg:     cfg,
		git:     execGit,
		verbose: opts.Verbose,
		jobs:    opts.Jobs,
		cred:    cred,
		exePath: exePath,
		out:     os.Stdout,
		errOut:  os.Stderr,
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determining working directory: %w", err)
	}
	repos, err := findRepos(wd)
	if err != nil {
		return fmt.Errorf("scanning workspace for repositories: %w", err)
	}
	sort.Strings(repos)
	s.diagf("Found %d repositories.", len(repos))

	var (
		results []repoStatus
		resMu   sync.Mutex
	)
	forEachConcurrent(ctx, s.jobs, repos, func(rel string) {
		if ctx.Err() != nil {
			return
		}
		if opts.Fetch {
			// A failed fetch is reported but does not skip the repo: the
			// local-only status is still worth showing.
			env, args, ok := s.authForCheckout(ctx, rel, rel, "fetch", "--quiet")
			if ok {
				_ = s.runGit(ctx, rel, rel, env, args...)
			}
		}
		out, err := s.git(ctx, rel, nil, "status", "--porcelain=v2", "--branch")
		if err != nil {
			s.reportGitFailure(rel, []string{"status"}, out, err)
			return
		}
		st := parseStatusPorcelainV2(out)
		st.Path = rel

		resMu.Lock()
		results = append(results, st)
		resMu.Unlock()

		s.event("status", rel, statusDetail(st))
	})

	dirty, ahead, behind := 0, 0, 0
	for _, st := range results {
		if st.Dirty {
			dirty++
		}
		if st.Ahead > 0 {
			ahead++
		}
		if st.Behind > 0 {
			behind++
		}
	}
	fmt.Fprintf(s.out, "summary repos=%d dirty=%d ahead=%d behind=%d errors=%d\n",
		len(results), dirty, ahead, behind, s.counts.errors)

	if ctx.Err() != nil {
		s.diagf("interrupted: reported %d of %d repositories", len(results), len(repos))
		return errInterrupted
	}
	if s.counts.errors > 0 {
		return &syncFailedError{failures: s.counts.errors}
	}
	return nil
}

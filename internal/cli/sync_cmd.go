package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"gitty/internal/config"
	"gitty/internal/gitexec"
	"gitty/internal/gitlabapi"
	syncpkg "gitty/internal/sync"
)

type clientCtor func(baseURL, token string) (gitlabapi.Client, error)
type runnerCtor func() gitexec.Runner

func defaultRunnerCtor() gitexec.Runner {
	return &gitexec.Real{}
}

// maxJobs is the upper bound on the effective concurrency. Values larger
// than this are clamped at the flag boundary (FR-003, FR-010).
const maxJobs = 64

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string, newClient clientCtor, newRunner runnerCtor) (int, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)

	syncPath := fs.String("path", "", "GitLab Group Path (e.g., tenant/images) (required)")
	syncToken := fs.String("token", "", "GitLab Access Token (falls back to env vars)")
	syncDryRun := fs.Bool("dry-run", false, "Print what would happen without actually making changes")
	syncGroups := fs.Bool("groups", false, "Fetch and create group/subgroup directory structures")
	syncRepos := fs.Bool("repos", false, "Fetch and clone/pull repositories")
	syncNested := fs.Bool("nested", false, "Include nested subgroups/projects recursively")
	syncJobs := fs.Int("jobs", 0, "Max concurrent clone/pull operations (range [1, 64]); 0 ⇒ use jobs from .gitty/config")

	if err := fs.Parse(args); err != nil {
		return 2, nil
	}

	doGroups := *syncGroups
	doRepos := *syncRepos
	if !doGroups && !doRepos {
		doRepos = true
	}

	wd, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("getwd: %w", err)
	}

	cfg, err := config.Load(wd)
	if err != nil {
		return 1, err
	}

	jobs, jobsErr := resolveJobs(*syncJobs, cfg.Jobs, stderr)
	if jobsErr != nil {
		return 2, jobsErr
	}

	token, err := syncpkg.ResolveToken(*syncToken, env)
	if err != nil {
		return 1, err
	}

	client, err := newClient(cfg.URL, token)
	if err != nil {
		return 1, fmt.Errorf("Failed to create GitLab client: %w", err)
	}

	deps := syncpkg.Deps{
		Client: client,
		Runner: newRunner(),
		Stdout: stdout,
		Stderr: stderr,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return !os.IsNotExist(err)
		},
	}

	req := syncpkg.Request{
		GroupFlag: *syncPath,
		Token:     token,
		DryRun:    *syncDryRun,
		DoGroups:  doGroups,
		DoRepos:   doRepos,
		Nested:    *syncNested,
		Jobs:      jobs,
	}

	if err := syncpkg.Sync(ctx, req, cfg, deps); err != nil {
		// SIGINT/SIGTERM: exit 130 (POSIX convention) — distinct from a
		// clean run (0) and from per-repo failure that the spec maps to 0.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 130, err
		}
		return 1, err
	}
	return 0, nil
}

// resolveJobs applies the --jobs / cfg.Jobs resolution and clamping rules
// from contracts/cli.md. Returns the effective jobs value in [1, 64].
// A non-nil error means the user passed an invalid value and runSync
// should return exit code 2 with that error.
func resolveJobs(flagValue, cfgValue int, stderr io.Writer) (int, error) {
	switch {
	case flagValue < 0:
		return 0, fmt.Errorf("--jobs must be >= 1 (got %d)", flagValue)
	case flagValue == 0:
		// Not passed; fall back to config. cfg.Jobs is already coalesced
		// to >=1 by config.Load.
		if cfgValue < 1 {
			return 1, nil
		}
		if cfgValue > maxJobs {
			return maxJobs, nil
		}
		return cfgValue, nil
	case flagValue > maxJobs:
		fmt.Fprintf(stderr, "--jobs %d clamped to %d\n", flagValue, maxJobs)
		return maxJobs, nil
	default:
		return flagValue, nil
	}
}

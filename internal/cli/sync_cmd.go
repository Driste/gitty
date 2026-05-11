package cli

import (
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
type runnerCtor func(stdout, stderr io.Writer) gitexec.Runner

func defaultRunnerCtor(stdout, stderr io.Writer) gitexec.Runner {
	return &gitexec.Real{Stdout: stdout, Stderr: stderr}
}

func runSync(args []string, stdout, stderr io.Writer, env func(string) string, newClient clientCtor, newRunner runnerCtor) (int, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)

	syncPath := fs.String("path", "", "GitLab Group Path (e.g., tenant/images) (required)")
	syncToken := fs.String("token", "", "GitLab Access Token (falls back to env vars)")
	syncDryRun := fs.Bool("dry-run", false, "Print what would happen without actually making changes")
	syncGroups := fs.Bool("groups", false, "Fetch and create group/subgroup directory structures")
	syncRepos := fs.Bool("repos", false, "Fetch and clone/pull repositories")
	syncNested := fs.Bool("nested", false, "Include nested subgroups/projects recursively")

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
		Runner: newRunner(stdout, stderr),
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
	}

	if err := syncpkg.Sync(req, cfg, deps); err != nil {
		return 1, err
	}
	return 0, nil
}

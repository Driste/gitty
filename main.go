package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Hidden re-exec mode: git invokes gitty as its askpass helper during
	// authenticated HTTP clones/pulls. Must run before any flag parsing,
	// config loading, or output.
	if os.Getenv("GITTY_ASKPASS_MODE") == "1" {
		runAskpass(os.Args)
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	initCmd := flag.NewFlagSet("init", flag.ExitOnError)
	initURL := initCmd.String("url", "https://gitlab.com", "GitLab Base URL")
	initHTTP := initCmd.Bool("http", false, "Use HTTP(S) for cloning instead of SSH")
	initForce := initCmd.Bool("force", false, "Overwrite an existing .gitty/config")

	syncCmd := flag.NewFlagSet("sync", flag.ExitOnError)
	syncPath := syncCmd.String("path", "", "GitLab Group Path (e.g., tenant/images) (required)")
	syncToken := syncCmd.String("token", "", "GitLab Access Token (falls back to env vars)")
	syncDryRun := syncCmd.Bool("dry-run", false, "Print what would happen without actually making changes")
	syncGroups := syncCmd.Bool("groups", false, "Fetch and create group/subgroup directory structures")
	syncRepos := syncCmd.Bool("repos", false, "Fetch and clone/pull repositories")
	syncNested := syncCmd.Bool("nested", false, "Include nested subgroups/projects recursively")
	syncAnon := syncCmd.Bool("anon", false, "Access public resources anonymously (no token required)")
	syncVerbose := syncCmd.Bool("verbose", false, "Print each git invocation and its output (URLs redacted) to stderr")
	syncRecloneBroken := syncCmd.Bool("reclone-broken", false, "Move aside non-repo directories that block a clone (renamed, never deleted) and re-clone")
	syncJobs := syncCmd.Int("jobs", 4, "Number of concurrent repo clone/pull operations (1-16)")

	statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
	statusToken := statusCmd.String("token", "", "GitLab Access Token (only needed with --fetch)")
	statusAnon := statusCmd.Bool("anon", false, "With --fetch, contact public repositories anonymously")
	statusFetch := statusCmd.Bool("fetch", false, "Refresh remote-tracking refs first so ahead/behind reflect the remote now")
	statusVerbose := statusCmd.Bool("verbose", false, "Print each git invocation and its output (URLs redacted) to stderr")
	statusJobs := statusCmd.Int("jobs", 4, "Number of concurrent repositories to inspect (1-16)")

	lsCmd := flag.NewFlagSet("ls", flag.ExitOnError)
	lsPath := lsCmd.String("path", "", "GitLab Group Path (e.g., tenant/images)")
	lsToken := lsCmd.String("token", "", "GitLab Access Token (falls back to env vars)")
	lsAnon := lsCmd.Bool("anon", false, "List public resources anonymously (no token required)")
	lsNested := lsCmd.Bool("nested", false, "Include nested subgroups/projects recursively")
	lsFormat := lsCmd.String("format", "text", "Output format: text, tree, or json")

	switch os.Args[1] {
	case "init":
		initCmd.Parse(os.Args[2:])
		// Inside a GitLab CI job, default the instance URL to the job's own
		// server unless --url was passed explicitly. Only init applies this
		// default: once written, the workspace config is the source of truth.
		resolvedURL := *initURL
		urlSet := false
		initCmd.Visit(func(fl *flag.Flag) {
			if fl.Name == "url" {
				urlSet = true
			}
		})
		if !urlSet && os.Getenv("GITLAB_CI") == "true" {
			if ciURL := os.Getenv("CI_SERVER_URL"); ciURL != "" {
				fmt.Fprintf(os.Stderr, "using CI_SERVER_URL=%s as instance URL\n", ciURL)
				resolvedURL = ciURL
			}
		}
		exitOnError(runInit(resolvedURL, *initHTTP, *initForce))
	case "sync":
		syncCmd.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runSync(ctx, syncOptions{
			Path:          *syncPath,
			Token:         *syncToken,
			DryRun:        *syncDryRun,
			Groups:        *syncGroups,
			Repos:         *syncRepos,
			Nested:        *syncNested,
			Anon:          *syncAnon,
			Verbose:       *syncVerbose,
			RecloneBroken: *syncRecloneBroken,
			Jobs:          *syncJobs,
		})
		stop()
		exitOnError(err)
	case "status":
		statusCmd.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runStatus(ctx, statusOptions{
			Token:   *statusToken,
			Anon:    *statusAnon,
			Fetch:   *statusFetch,
			Verbose: *statusVerbose,
			Jobs:    *statusJobs,
		})
		stop()
		exitOnError(err)
	case "ls":
		lsCmd.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runLs(ctx, lsOptions{
			Path:   *lsPath,
			Token:  *lsToken,
			Anon:   *lsAnon,
			Nested: *lsNested,
			Format: *lsFormat,
		})
		stop()
		exitOnError(err)
	case "agent":
		runAgent(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

// exitOnError reports a command error on stderr and exits with the taxonomy
// code; a nil error returns normally (exit 0 at end of main).
func exitOnError(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(exitCode(err))
}

// printUsage writes usage to stderr: it is only ever printed on error paths
// (no arguments or an unknown command).
func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: gitty <command> [flags]")
	fmt.Fprintln(os.Stderr, "\nCommands:")
	fmt.Fprintln(os.Stderr, "  init    Initialize a .gitty/config file in the current directory")
	fmt.Fprintln(os.Stderr, "  sync    Sync (clone/pull) a GitLab group based on the .gitty/config")
	fmt.Fprintln(os.Stderr, "  status  Report the branch and freshness of every checkout in the workspace")
	fmt.Fprintln(os.Stderr, "  ls      List the remote groups/projects for a target and what a sync would clone")
	fmt.Fprintln(os.Stderr, "  agent   Print an MCP-style schema describing how an LLM/agent should use gitty")
	fmt.Fprintln(os.Stderr, "\nRun 'gitty <command> -h' for specific flags.")
}

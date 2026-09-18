package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
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
	initSSH := initCmd.Bool("ssh", false, "Clone over SSH (git@...) instead of the default HTTP(S)")
	initHTTP := initCmd.Bool("http", false, "Clone over HTTP(S) (this is the default; accepted for compatibility)")
	initForce := initCmd.Bool("force", false, "Overwrite an existing .gitty/config")
	initToken := initCmd.String("token", "", "GitLab Access Token to verify (falls back to env vars)")
	initVerify := initCmd.Bool("verify", true, "Check the token against the instance and report its scopes")
	var initCloneHosts stringList
	initCmd.Var(&initCloneHosts, "allow-clone-host", "Additional host whose repositories this workspace may clone and send its token to (repeatable, or comma-separated)")
	initRespectGit := initCmd.Bool("respect-git-config", false, "Honour url.<base>.insteadOf rewrites from your git config instead of pinning the URL gitty selected")

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
	syncAcceptHostKeys := syncCmd.Bool("accept-new-host-keys", false, "For SSH clones, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new); a changed key is still refused")
	var syncCloneHosts stringList
	syncCmd.Var(&syncCloneHosts, "allow-clone-host", "Additional host whose repositories may be cloned and sent this workspace's token (repeatable, or comma-separated)")
	syncRespectGit := syncCmd.Bool("respect-git-config", false, "Honour url.<base>.insteadOf rewrites from your git config instead of pinning the URL gitty selected")

	statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
	statusToken := statusCmd.String("token", "", "GitLab Access Token (only needed with --fetch)")
	statusAnon := statusCmd.Bool("anon", false, "With --fetch, contact public repositories anonymously")
	statusFetch := statusCmd.Bool("fetch", false, "Refresh remote-tracking refs first so ahead/behind reflect the remote now")
	statusVerbose := statusCmd.Bool("verbose", false, "Print each git invocation and its output (URLs redacted) to stderr")
	statusJobs := statusCmd.Int("jobs", 4, "Number of concurrent repositories to inspect (1-16)")
	statusAcceptHostKeys := statusCmd.Bool("accept-new-host-keys", false, "With --fetch over SSH, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new)")
	var statusCloneHosts stringList
	statusCmd.Var(&statusCloneHosts, "allow-clone-host", "With --fetch, an additional host that may be contacted with this workspace's token (repeatable, or comma-separated)")
	statusRespectGit := statusCmd.Bool("respect-git-config", false, "Honour url.<base>.insteadOf rewrites from your git config instead of pinning the URL gitty selected")

	lsCmd := flag.NewFlagSet("ls", flag.ExitOnError)
	lsPath := lsCmd.String("path", "", "GitLab group path (deprecated: pass it as a positional argument)")
	lsToken := lsCmd.String("token", "", "GitLab Access Token (falls back to env vars)")
	lsAnon := lsCmd.Bool("anon", false, "List public resources anonymously (no token required)")
	lsNested := lsCmd.Bool("nested", false, "Include nested subgroups/projects recursively")
	lsFormat := lsCmd.String("format", "auto", "Output format: auto, tree, text, or json")
	lsColor := lsCmd.String("color", "auto", "Colorize output: auto, always, or never")

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
		// HTTP(S) is the default transport: the token gitty already needs for
		// the API authenticates the clones too, which is what makes an
		// unattended run work without SSH keys. --ssh opts out; --http is
		// still accepted so existing scripts keep working.
		if *initSSH && *initHTTP {
			exitOnError(usageErrf("--ssh and --http are mutually exclusive"))
		}
		exitOnError(runInit(initOptions{
			URL:              resolvedURL,
			HTTP:             !*initSSH,
			Force:            *initForce,
			Verify:           *initVerify,
			Token:            *initToken,
			RespectGitConfig: *initRespectGit,
			CloneHosts:       initCloneHosts,
		}))
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

			AcceptNewHostKeys: *syncAcceptHostKeys,
			RespectGitConfig:  *syncRespectGit,
			AllowCloneHosts:   syncCloneHosts,
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

			AcceptNewHostKeys: *statusAcceptHostKeys,
			RespectGitConfig:  *statusRespectGit,
			AllowCloneHosts:   statusCloneHosts,
		})
		stop()
		exitOnError(err)
	case "ls":
		// Flags may sit on either side of the positional group argument.
		lsFlagArgs, lsPositional := splitFlagArgs(lsCmd, os.Args[2:])
		lsCmd.Parse(lsFlagArgs)
		lsTarget := *lsPath
		if len(lsPositional) > 0 {
			if lsTarget != "" {
				exitOnError(usageErrf("pass the group either as an argument or with --path, not both"))
			}
			lsTarget = lsPositional[0]
		}
		if len(lsPositional) > 1 {
			exitOnError(usageErrf("ls takes at most one group argument, got %d", len(lsPositional)))
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runLs(ctx, lsOptions{
			Target: lsTarget,
			Token:  *lsToken,
			Anon:   *lsAnon,
			Nested: *lsNested,
			Format: *lsFormat,
			Color:  *lsColor,
		})
		stop()
		exitOnError(err)
	case "version":
		runVersion()
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

// stringList is a repeatable string flag. Values also accept a comma-separated
// list, so --allow-clone-host=a,b and --allow-clone-host=a --allow-clone-host=b
// are equivalent.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// boolFlag matches the flag package's interface for flags that take no
// separate value argument.
type boolFlag interface{ IsBoolFlag() bool }

// splitFlagArgs separates flags from positional arguments so that flags may
// appear on either side of the positional one — `gitty ls acme --nested` as
// well as `gitty ls --nested acme`. The flag package stops parsing at the
// first non-flag argument, unlike ls(1) and most modern CLIs. Everything after
// a bare "--" is positional.
func splitFlagArgs(fs *flag.FlagSet, args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			return flagArgs, positional
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue // value is attached: --flag=value
		}
		known := fs.Lookup(name)
		if known == nil {
			continue // unknown flag: let Parse produce the error
		}
		if bf, ok := known.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue // booleans never consume the next argument
		}
		// --flag value: the next argument belongs to this flag.
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
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
	fmt.Fprintln(os.Stderr, "  version Print the gitty version")
	fmt.Fprintln(os.Stderr, "\nRun 'gitty <command> -h' for specific flags.")
}

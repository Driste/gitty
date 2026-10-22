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

	switch os.Args[1] {
	case "init":
		fs := newFlagSet("init")
		f := declareInitFlags(fs)
		fs.Parse(os.Args[2:])
		// Inside a GitLab CI job, default the instance URL to the job's own
		// server unless --url was passed explicitly. Only init applies this
		// default: once written, the workspace config is the source of truth.
		resolvedURL := *f.url
		urlSet := false
		fs.Visit(func(fl *flag.Flag) {
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
		if *f.ssh && *f.http {
			exitOnError(usageErrf("--ssh and --http are mutually exclusive"))
		}
		exitOnError(runInit(initOptions{
			URL:        resolvedURL,
			HTTP:       !*f.ssh,
			Force:      *f.force,
			Verify:     *f.verify,
			Token:      *f.token,
			CloneHosts: f.cloneHosts,
		}))
	case "sync":
		// The group is a positional argument, relative to the current
		// directory's group; flags may sit on either side of it. --path is
		// the older spelling and still works.
		fs := newFlagSet("sync")
		f := declareSyncFlags(fs)
		flagArgs, positional := splitFlagArgs(fs, os.Args[2:])
		fs.Parse(flagArgs)
		target, err := positionalTarget("sync", *f.path, positional)
		exitOnError(err)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = runSync(ctx, syncOptions{
			Path:          target,
			Token:         *f.token,
			DryRun:        *f.dryRun,
			Groups:        *f.groups,
			Repos:         *f.repos,
			Nested:        *f.nested,
			Anon:          *f.anon,
			Verbose:       *f.verbose,
			RecloneBroken: *f.recloneBroken,
			Jobs:          *f.jobs,

			AcceptNewHostKeys: *f.acceptHostKeys,
			IncludeArchived:   *f.archived,
			AllowCloneHosts:   f.cloneHosts,
			Exclude:           f.exclude,
			Prune:             *f.prune,
		})
		stop()
		exitOnError(err)
	case "status":
		fs := newFlagSet("status")
		f := declareStatusFlags(fs)
		fs.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runStatus(ctx, statusOptions{
			Token:   *f.token,
			Anon:    *f.anon,
			Fetch:   *f.fetch,
			Verbose: *f.verbose,
			Jobs:    *f.jobs,

			AcceptNewHostKeys: *f.acceptHostKeys,
			AllowCloneHosts:   f.cloneHosts,
		})
		stop()
		exitOnError(err)
	case "ls":
		// Flags may sit on either side of the positional group argument.
		fs := newFlagSet("ls")
		f := declareLsFlags(fs)
		flagArgs, positional := splitFlagArgs(fs, os.Args[2:])
		fs.Parse(flagArgs)
		target, err := positionalTarget("ls", *f.path, positional)
		exitOnError(err)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = runLs(ctx, lsOptions{
			Target:          target,
			Token:           *f.token,
			Anon:            *f.anon,
			Nested:          *f.nested,
			IncludeArchived: *f.archived,
			Exclude:         f.exclude,
			Format:          *f.format,
			Color:           *f.color,
		})
		stop()
		exitOnError(err)
	case "auth":
		fs := newFlagSet("auth")
		f := declareAuthFlags(fs)
		fs.Parse(os.Args[2:])
		exitOnError(runAuth(*f.token))
	case "completion":
		exitOnError(runCompletion(os.Stdout, os.Args[2:]))
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
// (no arguments or an unknown command). The command list comes from the
// same table that drives dispatch and completion.
func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: gitty <command> [flags]")
	fmt.Fprintln(os.Stderr, "\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-11s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(os.Stderr, "\nRun 'gitty <command> -h' for specific flags.")
}

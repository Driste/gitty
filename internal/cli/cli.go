// Package cli is gitty's CLI surface: argv parsing, subcommand dispatch,
// and stream wiring. main.go is a 5-line shim that calls Main with the
// process's real argv and standard streams.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"gitty/internal/gitlabapi"
)

// Main is the testable entrypoint. main() calls
// os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)).
//
// The context used for sync invocations listens for SIGINT and SIGTERM via
// signal.NotifyContext, so Ctrl-C cancels in-flight git children and exits
// the process cleanly (FR-009). Tests use the mainWithDeps seam to inject
// their own context.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return mainWithDeps(ctx, args, stdin, stdout, stderr, os.Getenv, gitlabapi.NewReal, defaultRunnerCtor)
}

// mainWithDeps is the test seam. It accepts an explicit context plus
// injectable constructors for the GitLab client and the git runner so tests
// can drive the full dispatch flow with fakes and cancellable contexts.
func mainWithDeps(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	env func(string) string,
	newClient clientCtor,
	newRunner runnerCtor,
) int {
	_ = stdin
	if len(args) < 1 {
		usage(stdout)
		return 1
	}
	switch args[0] {
	case "init":
		code, err := runInit(args[1:], stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return code
	case "sync":
		code, err := runSync(ctx, args[1:], stdout, stderr, env, newClient, newRunner)
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return code
	default:
		fmt.Fprintf(stdout, "Unknown command: %s\n", args[0])
		usage(stdout)
		return 1
	}
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: gitty <command> [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  init    Initialize a .gitty/config in the current directory")
	fmt.Fprintln(out, "  sync    Sync (clone/pull) a GitLab group based on the .gitty/config")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run 'gitty <command> -h' for specific flags.")
}

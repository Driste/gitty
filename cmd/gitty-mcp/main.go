// gitty-mcp is gitty's Model Context Protocol server: a plugin that lets a
// coding agent ask which other projects of a GitLab instance matter to the
// one it is working in, where they are on disk, and why.
//
// It is started as `gitty mcp` — gitty finds this binary beside itself or on
// PATH and runs it in its own place — by whatever hosts the agent, which then
// talks to it over stdin and stdout.
//
// It is thin by rule. It owns the protocol and the description of the tools;
// what the tools answer comes from running gitty (`gitty context
// --format=json`, `gitty fetch --format=json`, `gitty index`) and from the
// index file gitty keeps. Only searching and reading files inside checkouts,
// which need nothing but the checkouts, happen here. So the gitty CLI stays
// the one implementation and the one public interface: there is nothing this
// server can do that a script could not.
//
// It never handles a credential. gitty resolves one, when it needs one, from
// its own flags and environment, exactly as it does on a command line.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

// version is set at link time for releases (-X main.version=<tag>), the same
// way gitty's is; the two are released together.
var version = "dev"

// Environment gitty starts a plugin with; see gitty's plugin.go.
const (
	envBin       = "GITTY_BIN"
	envWorkspace = "GITTY_WORKSPACE"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gitty mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noFetch := fs.Bool("no-fetch", false, "Read-only: never clone. Projects that are not checked out are reported as absent")
	anon := fs.Bool("anon", false, "Reach the instance anonymously (public projects only); otherwise gitty uses GITLAB_TOKEN or CI_JOB_TOKEN")
	jobs := fs.Int("jobs", 0, "Concurrent clones and checkout reads gitty may use (1-16; default: gitty's own)")
	showVersion := fs.Bool("version", false, "Print the version and exit")
	describe := fs.Bool("describe", false, "Print the tools this server offers, as JSON, and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gitty mcp [--no-fetch] [--anon] [--jobs=N]")
		fmt.Fprintln(stderr, "\nServes cross-repository context to a coding agent over the Model Context")
		fmt.Fprintln(stderr, "Protocol, on stdin and stdout. Start it from inside a gitty workspace, or")
		fmt.Fprintln(stderr, "with `gitty -C <directory> mcp`; the project in that directory is the one")
		fmt.Fprintln(stderr, "the tools are about when they are not told otherwise.")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Error: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *jobs < 0 || *jobs > 16 {
		fmt.Fprintf(stderr, "Error: --jobs must be between 1 and 16, got %d\n", *jobs)
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if *describe {
		return printDescription(stdout, stderr, *noFetch)
	}

	s, err := newServer(os.Getenv(envBin), os.Getenv(envWorkspace), *noFetch, *jobs, stderr)
	if s != nil {
		s.anon = *anon
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stderr, "%s %s: serving workspace %s (project context from %s)\n", serverName, version, s.workspace, s.dir)
	if err := s.serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// newServer checks what gitty handed over. This plugin does not look for a
// workspace itself — that is gitty's job, done once, in one place — so it
// insists on having been told.
func newServer(bin, workspace string, noFetch bool, jobs int, errOut io.Writer) (*server, error) {
	if bin == "" {
		found, err := exec.LookPath("gitty")
		if err != nil {
			return nil, fmt.Errorf("gitty-mcp is a gitty plugin and needs gitty: start it as `gitty mcp`")
		}
		bin = found
	}
	if workspace == "" {
		return nil, fmt.Errorf("no gitty workspace here: start the server from inside one, or as `gitty -C <directory> mcp`")
	}
	if fi, err := os.Stat(filepath.Join(workspace, ".gitty", "config")); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("%s is not a gitty workspace (no .gitty/config in it)", workspace)
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining the working directory: %v", err)
	}
	return &server{gittyBin: bin, workspace: workspace, dir: dir, noFetch: noFetch, jobs: jobs, errOut: errOut}, nil
}

// printDescription writes what this server offers, for anyone deciding
// whether to install it.
func printDescription(stdout, stderr io.Writer, noFetch bool) int {
	s := &server{noFetch: noFetch}
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	doc := struct {
		Name      string     `json:"name"`
		Version   string     `json:"version"`
		Protocol  []string   `json:"protocol"`
		Transport string     `json:"transport"`
		Command   []string   `json:"command"`
		Tools     []toolInfo `json:"tools"`
	}{Name: serverName, Version: version, Protocol: protocolVersions, Transport: "stdio", Command: []string{"gitty", "mcp"}}
	for _, t := range s.tools() {
		doc.Tools = append(doc.Tools, toolInfo{Name: t.name, Description: t.description})
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

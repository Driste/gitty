// gitty-up is the shared memory of a GitLab instance: a small HTTP service
// that knows the instance's groups and projects, what each project's
// repository says of it, and what the agents working there have learned —
// and serves all of that over the Model Context Protocol as a tree an agent
// walks by reasoning.
//
// It is started as `gitty up` — gitty finds this binary beside itself or on
// PATH and runs it in its own place — once for an instance, and left
// running. Agents reach it by URL; `gitty`, told where it is, sends it what
// it reads from the checkouts it manages.
//
// It reads no repository itself. What it knows of a repository was sent by
// a gitty that had it checked out, and what it knows beyond that was either
// in GitLab's listing or written down by an agent, signed. Each is kept
// apart and said to be what it is.
//
// It holds no credential but its own. A caller's token is used to ask the
// instance who they are, and is not kept.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is set at link time for releases (-X main.version=<tag>), the same
// way gitty's is; the two are released together.
var version = "dev"

// envInstance is set by gitty when it starts a plugin inside a workspace.
const envInstance = "GITTY_INSTANCE"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// repeated collects a flag given several times, or once with commas.
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*r = append(*r, part)
		}
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gitty up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	instance := fs.String("url", os.Getenv(envInstance), "The GitLab instance (default: the workspace's, when started as `gitty up` inside one)")
	listen := fs.String("listen", "127.0.0.1:7878", "Address to serve on")
	data := fs.String("data", os.Getenv("GITTY_UP_DATA"), "Directory the hub keeps what it knows in (default: per instance, under your data directory)")
	token := fs.String("token", "", "Token the hub lists the instance with (falls back to GITLAB_TOKEN)")
	anon := fs.Bool("anon", false, "List the instance anonymously: public groups and projects only")
	noAuth := fs.Bool("no-auth", false, "Do not ask callers for a token. Only allowed on a loopback address: for one person's own machine")
	refresh := fs.Duration("refresh", 30*time.Minute, "How often the instance is listed again")
	quietDays := fs.Int("quiet-after", 365, "Days without activity after which a project is set aside as unmaintained (0: never)")
	showVersion := fs.Bool("version", false, "Print the version and exit")
	var groups, origins repeated
	fs.Var(&groups, "group", "Cover only this top-level group or subgroup (repeatable, or comma-separated). Default: every group the token sees")
	fs.Var(&origins, "allow-origin", "A web origin allowed to call the hub from a browser (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gitty up [--url=<instance>] [--listen=<addr>] [--group=<path>]... [flags]")
		fmt.Fprintln(stderr, "\nRuns the instance's shared context: what its projects are, what they provide,")
		fmt.Fprintln(stderr, "what uses what, and what agents working there have noted. Agents reach it")
		fmt.Fprintln(stderr, "over MCP at http://<addr>/mcp, with a token of the instance as a bearer token.")
		fmt.Fprintln(stderr, "gitty sends it what it reads from checkouts when a workspace's config names it:")
		fmt.Fprintln(stderr, "\n    hub = 'http://<addr>'")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "Error: "+format+"\n", a...)
		return 2
	}
	if fs.NArg() > 0 {
		return usage("unexpected argument %q", fs.Arg(0))
	}
	if *instance == "" {
		return usage("which instance? Pass --url=https://gitlab.example.com, or run `gitty up` inside a gitty workspace")
	}
	if *anon && *token != "" {
		return usage("--anon and --token cannot be combined")
	}
	if *token == "" && !*anon {
		*token = os.Getenv("GITLAB_TOKEN")
	}
	if *token == "" && !*anon {
		return usage("a token to list the instance with is required (--token or GITLAB_TOKEN); --anon lists what is public without one")
	}
	if *quietDays < 0 || *refresh < time.Minute {
		return usage("--quiet-after must not be negative, and --refresh must be at least 1m")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return usage("--listen must be host:port, got %q", *listen)
	}
	if *noAuth && !isLoopback(host) {
		return usage("--no-auth lets anyone who can reach the hub read it and write to it, so it is only allowed on a loopback address (127.0.0.1), not %q", host)
	}

	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(stderr, time.Now().Format("15:04:05")+" "+format+"\n", a...)
	}
	gl, err := newGitLab(*instance, *token, logf)
	if err != nil {
		return usage("%v", err)
	}
	dir := *data
	if dir == "" {
		if dir, err = defaultDataDir(gl.host); err != nil {
			return usage("no place to keep data: %v (pass --data)", err)
		}
	}
	st, err := openStore(dir, strings.TrimRight(*instance, "/"))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	defer st.close()

	h := &hub{
		st: st, gl: gl, auth: newAuthenticator(gl, *noAuth),
		quietAfter: time.Duration(*quietDays) * 24 * time.Hour,
		now:        time.Now, logf: logf,
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	srv := &http.Server{
		Handler:           h.routes(origins),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logf("gitty-up %s for %s, serving MCP at http://%s/mcp", version, gl.host, ln.Addr())
	logf("data in %s: %d projects, %d with their repository read", dir, len(st.projects), len(st.facts))
	if *noAuth {
		logf("--no-auth: callers are not asked who they are")
	}

	// The listing: now, and then every so often. The server answers from
	// what it already holds while a listing runs, and from the last good
	// one if a listing fails.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			started := time.Now()
			if err := st.list(ctx, gl, groups, time.Now()); err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("listing %s failed: %v (keeping what is held; trying again in %s)", gl.host, err, *refresh)
			} else {
				st.mu.RLock()
				logf("listed %s in %s: %d projects in %d groups", gl.host, time.Since(started).Round(time.Millisecond), len(st.projects), len(st.groups))
				st.mu.RUnlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(*refresh):
			}
		}
	}()

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	code := 0
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			code = 1
		}
	case <-ctx.Done():
		logf("stopping")
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		srv.Shutdown(shut)
		cancel()
	}
	wg.Wait()
	return code
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultDataDir is where a hub keeps its data when not told: one directory
// per instance, where the platform keeps a user's application data.
func defaultDataDir(host string) (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			dir, err := os.UserConfigDir()
			if err != nil {
				return "", err
			}
			base = dir
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "share")
		}
	}
	safe := strings.NewReplacer(":", "_", "/", "_").Replace(host)
	return filepath.Join(base, "gitty-up", safe), nil
}

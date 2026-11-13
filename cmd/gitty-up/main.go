// gitty-up is the shared memory of a GitLab instance: a small HTTP service
// that knows the instance's groups and projects, what each project's
// repository says of it, and what the agents working there have learned —
// and serves all of that over the Model Context Protocol as a tree an agent
// walks by reasoning.
//
// It is started as `gitty up` — gitty finds this binary beside itself or on
// PATH and runs it in its own place. Started once for an instance and left
// running, agents reach it by URL, and `gitty`, told where it is, sends it
// what it reads from the checkouts it manages. Started with --stdio by an
// agent's host, it serves that one agent over stdin and stdout: from the
// team's hub when the workspace names one, and as a hub of its own when not
// (see stdio.go).
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
	"encoding/json"
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

// What gitty says about where it started this plugin, and one thing the user
// may say themselves.
const (
	envInstance = "GITTY_INSTANCE" // the workspace's instance
	envGroup    = "GITTY_GROUP"    // the group the working directory is in
	envHub      = "GITTY_HUB"      // the hub the workspace names, or the user does
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
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

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	stdio := fs.Bool("stdio", false, "Serve one agent over stdin and stdout instead of listening on an address: for a host that starts the server itself")
	logPath := fs.String("log", os.Getenv("GITTY_UP_LOG"), "Also append the log to this file. A host that starts the server keeps its stderr; this is how to watch it")
	teamHub := fs.String("hub", "", "With --stdio: the address of the team's hub to serve from (default: the one the workspace's config names; with none, --stdio is a hub of its own)")
	own := fs.Bool("own", false, "With --stdio: be a hub of its own even where the workspace names a team's")
	showVersion := fs.Bool("version", false, "Print the version and exit")
	var groups, origins repeated
	fs.Var(&groups, "group", "Cover only this top-level group or subgroup (repeatable, or comma-separated). Default: every group the token sees")
	fs.Var(&origins, "allow-origin", "A web origin allowed to call the hub from a browser (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gitty up --stdio [flags]                                   for an agent's host to start")
		fmt.Fprintln(stderr, "       gitty up [--url=<instance>] [--listen=<addr>] [flags]      as a service for a team")
		fmt.Fprintln(stderr, "\nServes the instance's shared context over MCP: what its projects are, what")
		fmt.Fprintln(stderr, "they provide, what uses what, and what agents working there have noted.")
		fmt.Fprintln(stderr, "\nWith --stdio it serves one agent over stdin and stdout, started by the agent's")
		fmt.Fprintln(stderr, "host inside a gitty workspace. It serves from the team's hub if the workspace's")
		fmt.Fprintln(stderr, "config names one (hub = 'https://...'), and is a hub of its own if not.")
		fmt.Fprintln(stderr, "\nWithout it, it is that team hub: agents reach it at http://<addr>/mcp with a")
		fmt.Fprintln(stderr, "token of the instance as a bearer token, and gitty sends it what checkouts say.")
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
	if *anon && *token != "" {
		return usage("--anon and --token cannot be combined")
	}
	if *token == "" && !*anon {
		*token = os.Getenv("GITLAB_TOKEN")
	}
	if !*stdio && (*teamHub != "" || *own) {
		return usage("--hub and --own are for --stdio; without --stdio this is a hub itself, and listens on --listen")
	}
	if *own && *teamHub != "" {
		return usage("--own and --hub cannot be combined")
	}
	// Which hub a workspace uses is in its config, and gitty passes it on.
	// Someone running the team's hub from inside such a workspace is not
	// asking to be served by it.
	if *stdio && !*own && *teamHub == "" {
		*teamHub = os.Getenv(envHub)
	}

	// The log: a line for everything the server does on its own account and
	// for every exchange with a client. It goes to stderr, and to a file as
	// well when one is named — with the date, since a file outlives the day,
	// and the process, since several sessions may be writing to it.
	var logFile *os.File
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return usage("--log: %v", err)
		}
		defer f.Close()
		logFile = f
	}
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		line, now := fmt.Sprintf(format, a...), time.Now()
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(stderr, "%s %s\n", now.Format("15:04:05"), line)
		if logFile != nil {
			fmt.Fprintf(logFile, "%s [%d] %s\n", now.Format("2006-01-02 15:04:05"), os.Getpid(), line)
		}
	}

	// For one agent, from the team's hub: nothing is kept or listed here.
	if *stdio && *teamHub != "" {
		if *token == "" && !*anon {
			return usage("a GitLab token is required to speak to the hub at %s (GITLAB_TOKEN, in the environment the host starts this server in); --anon sends none", *teamHub)
		}
		b, err := newBridge(*teamHub, *token, logf)
		if err != nil {
			return usage("%v", err)
		}
		go b.check(ctx)
		if err := serveStdio(ctx, stdin, stdout, b.answer); err != nil {
			logf("%v", err)
			return 1
		}
		return 0
	}

	if *instance == "" {
		if *stdio {
			return usage("which instance? Start this server inside a gitty workspace (the host's working directory for it), or pass --url=https://gitlab.example.com")
		}
		return usage("which instance? Pass --url=https://gitlab.example.com, or run `gitty up` inside a gitty workspace")
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

	gl, err := newGitLab(*instance, *token, logf)
	if err != nil {
		return usage("%v", err)
	}
	// An organization's own instance is covered whole. gitlab.com is not
	// one organization's, and has more groups than can be listed: there the
	// groups are named, or are the one the workspace is in.
	if len(groups) == 0 && strings.EqualFold(gl.host, "gitlab.com") {
		top, _, _ := strings.Cut(os.Getenv(envGroup), "/")
		if !*stdio || top == "" {
			return usage("gitlab.com is not one organization's instance and cannot be listed whole: name the groups to cover with --group")
		}
		groups = repeated{top}
		logf("gitlab.com is listed a group at a time: covering %s, where this was started (--group names others)", top)
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
		st: st, gl: gl, auth: newAuthenticator(gl, *noAuth || *stdio),
		quietAfter: time.Duration(*quietDays) * 24 * time.Hour,
		now:        time.Now, logf: logf,
	}
	if *stdio {
		return runOwnHub(ctx, h, groups, *refresh, *token, *anon, dir, stdin, stdout)
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

// identifyWait is how long a hub of one's own waits to learn whose token it
// was started with. The instance is asked while the client is greeted, and
// only a tool call — the first thing done in someone's name — waits for the
// answer, and not for long: an instance that is down is no reason to keep
// an agent from what is already known.
var identifyWait = 5 * time.Second

// runOwnHub serves one agent over stdio from a hub of its own: this
// person's, kept in their data directory and shared by every session they
// start. It lists the instance and has gitty tell it what the workspace's
// checkouts say, now and each time the listing is taken again; the agent is
// answered from the start, with whatever is known so far.
func runOwnHub(ctx context.Context, h *hub, groups []string, refresh time.Duration, token string, anon bool,
	dir string, stdin io.Reader, stdout io.Writer) int {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	h.st.several = true

	env := readStdioEnv()
	h.logf("gitty-up %s for %s, serving over stdio from a hub of your own", version, h.gl.host)
	h.logf("data in %s: %d projects, %d with their repository read", dir, len(h.st.projects), len(h.st.facts))
	if env.gittyBin == "" || env.workspace == "" {
		h.logf("not started by gitty inside a workspace: the listing and notes are served, and no checkout is read")
	}

	// Notes are signed. Here the caller is whoever started the server: the
	// instance is asked for their name, once.
	me, refused, known := &caller{Name: "anonymous"}, false, make(chan struct{})
	go func() {
		defer close(known)
		if anon {
			return
		}
		asked, cancel := context.WithTimeout(ctx, identifyWait)
		defer cancel()
		u, err := h.gl.whoIs(asked, token)
		switch status := statusOf(err); {
		case err == nil && u.Username != "":
			me = &caller{Name: u.Username, ID: u.ID, External: u.External, token: token}
			h.logf("notes are signed @%s", me.Name)
		case err == nil, status == http.StatusUnauthorized, status == http.StatusForbidden:
			h.logf("%s does not accept the token this server was started with (GITLAB_TOKEN): stopping", h.gl.host)
			refused = true
			stop()
		case ctx.Err() == nil:
			h.logf("could not ask %s whose token this is (%v): notes are signed \"me\"", h.gl.host, err)
			me = &caller{Name: "me", token: token}
		}
	}()
	answerAs := h.answerLine
	answer := func(ctx context.Context, line []byte) []byte {
		var msg struct {
			Method string `json:"method"`
		}
		json.Unmarshal(line, &msg)
		if msg.Method != "tools/call" {
			return answerAs(&caller{})(ctx, line) // nothing but a tool call is anyone's
		}
		select {
		case <-known:
			return answerAs(me)(ctx, line)
		case <-ctx.Done():
			return nil
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			started := time.Now()
			if err := h.st.list(ctx, h.gl, groups, time.Now()); err != nil {
				if ctx.Err() != nil {
					return
				}
				h.logf("listing %s failed: %v (keeping what is held; trying again in %s)", h.gl.host, err, refresh)
			} else {
				h.st.mu.RLock()
				h.logf("listed %s in %s: %d projects in %d groups", h.gl.host, time.Since(started).Round(time.Millisecond), len(h.st.projects), len(h.st.groups))
				h.st.mu.RUnlock()
				if env.gittyBin != "" && env.workspace != "" {
					h.shareWorkspace(ctx, env.gittyBin, env.workspace, token, anon)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(refresh):
			}
		}
	}()

	code := 0
	if err := serveStdio(ctx, stdin, stdout, answer); err != nil {
		h.logf("%v", err)
		code = 1
	}
	stop()
	wg.Wait()
	if <-known; refused {
		code = 2
	}
	return code
}

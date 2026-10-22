package main

import "flag"

// command describes one gitty subcommand: what it is called, what it does in
// a sentence, whether it takes a group path as its argument, and how its
// flags are declared. main dispatches on this table, printUsage lists it, and
// shell completion is generated from it — one source, so the three never
// disagree about which commands and flags exist.
type command struct {
	name       string
	summary    string
	takesGroup bool // a positional group path, which is a directory locally
	declare    func(*flag.FlagSet)
}

var commands = []command{
	{"init", "Initialize a .gitty/config file in the current directory", false, func(fs *flag.FlagSet) { declareInitFlags(fs) }},
	{"sync", "Sync (clone/pull) a GitLab group: gitty sync [group] [flags]", true, func(fs *flag.FlagSet) { declareSyncFlags(fs) }},
	{"status", "Report the branch and freshness of every checkout in the workspace", false, func(fs *flag.FlagSet) { declareStatusFlags(fs) }},
	{"ls", "List the remote groups/projects for a target and what a sync would clone", true, func(fs *flag.FlagSet) { declareLsFlags(fs) }},
	{"auth", "Check the token against the workspace's instance and report its scopes", false, func(fs *flag.FlagSet) { declareAuthFlags(fs) }},
	{"completion", "Print a shell completion script: gitty completion bash|zsh|fish", false, nil},
	{"agent", "Print an MCP-style schema describing how an LLM/agent should use gitty", false, nil},
	{"version", "Print the gitty version", false, nil},
}

// newFlagSet is the one place flag sets are created, so every command parses
// the same way.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

type initFlags struct {
	url        *string
	ssh        *bool
	http       *bool
	force      *bool
	token      *string
	verify     *bool
	cloneHosts stringList
}

func declareInitFlags(fs *flag.FlagSet) *initFlags {
	f := &initFlags{}
	f.url = fs.String("url", "https://gitlab.com", "GitLab Base URL")
	f.ssh = fs.Bool("ssh", false, "Clone over SSH (git@...) instead of the default HTTP(S)")
	f.http = fs.Bool("http", false, "Clone over HTTP(S) (this is the default; accepted for compatibility)")
	f.force = fs.Bool("force", false, "Overwrite an existing .gitty/config")
	f.token = fs.String("token", "", "GitLab Access Token to verify (falls back to env vars)")
	f.verify = fs.Bool("verify", true, "Check the token against the instance and report its scopes")
	fs.Var(&f.cloneHosts, "allow-clone-host", "Additional host whose repositories this workspace may clone and send its token to (repeatable, or comma-separated)")
	return f
}

type syncFlags struct {
	path           *string
	token          *string
	dryRun         *bool
	groups         *bool
	repos          *bool
	nested         *bool
	anon           *bool
	verbose        *bool
	recloneBroken  *bool
	jobs           *int
	acceptHostKeys *bool
	archived       *bool
	prune          *bool
	exclude        stringList
	cloneHosts     stringList
}

func declareSyncFlags(fs *flag.FlagSet) *syncFlags {
	f := &syncFlags{}
	f.path = fs.String("path", "", "GitLab group path (deprecated: pass it as a positional argument)")
	f.token = fs.String("token", "", "GitLab Access Token (falls back to env vars)")
	f.dryRun = fs.Bool("dry-run", false, "Print what would happen without actually making changes")
	f.groups = fs.Bool("groups", false, "Fetch and create group/subgroup directory structures")
	f.repos = fs.Bool("repos", false, "Fetch and clone/pull repositories")
	f.nested = fs.Bool("nested", false, "Include nested subgroups/projects recursively")
	f.anon = fs.Bool("anon", false, "Access public resources anonymously (no token required)")
	f.verbose = fs.Bool("verbose", false, "Print each git invocation and its output (URLs redacted) to stderr")
	f.recloneBroken = fs.Bool("reclone-broken", false, "Move aside non-repo directories that block a clone (renamed, never deleted) and re-clone")
	f.jobs = fs.Int("jobs", 4, "Number of concurrent repo clone/pull operations (1-16)")
	f.acceptHostKeys = fs.Bool("accept-new-host-keys", false, "For SSH clones, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new); a changed key is still refused")
	f.archived = fs.Bool("archived", false, "Include archived projects and groups (left out by default)")
	f.prune = fs.Bool("prune", false, "Move checkouts that no longer exist upstream aside as <dir>.gitty-orphan-<n> (never deleted); without it they are only reported")
	fs.Var(&f.exclude, "exclude", "Namespace path or glob to leave out, e.g. acme/vendor or 'acme/*-mirror' (repeatable, or comma-separated; adds to the workspace's exclude list)")
	fs.Var(&f.cloneHosts, "allow-clone-host", "Additional host whose repositories may be cloned and sent this workspace's token (repeatable, or comma-separated)")
	return f
}

type statusFlags struct {
	token          *string
	anon           *bool
	fetch          *bool
	verbose        *bool
	jobs           *int
	acceptHostKeys *bool
	cloneHosts     stringList
}

func declareStatusFlags(fs *flag.FlagSet) *statusFlags {
	f := &statusFlags{}
	f.token = fs.String("token", "", "GitLab Access Token (only needed with --fetch)")
	f.anon = fs.Bool("anon", false, "With --fetch, contact public repositories anonymously")
	f.fetch = fs.Bool("fetch", false, "Refresh remote-tracking refs first so ahead/behind reflect the remote now")
	f.verbose = fs.Bool("verbose", false, "Print each git invocation and its output (URLs redacted) to stderr")
	f.jobs = fs.Int("jobs", 4, "Number of concurrent repositories to inspect (1-16)")
	f.acceptHostKeys = fs.Bool("accept-new-host-keys", false, "With --fetch over SSH, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new)")
	fs.Var(&f.cloneHosts, "allow-clone-host", "With --fetch, an additional host that may be contacted with this workspace's token (repeatable, or comma-separated)")
	return f
}

type authFlags struct {
	token *string
}

func declareAuthFlags(fs *flag.FlagSet) *authFlags {
	return &authFlags{token: fs.String("token", "", "GitLab Access Token to verify (falls back to env vars)")}
}

type lsFlags struct {
	path     *string
	token    *string
	anon     *bool
	nested   *bool
	archived *bool
	exclude  stringList
	format   *string
	color    *string
}

func declareLsFlags(fs *flag.FlagSet) *lsFlags {
	f := &lsFlags{}
	f.path = fs.String("path", "", "GitLab group path (deprecated: pass it as a positional argument)")
	f.token = fs.String("token", "", "GitLab Access Token (falls back to env vars)")
	f.anon = fs.Bool("anon", false, "List public resources anonymously (no token required)")
	f.nested = fs.Bool("nested", false, "Include nested subgroups/projects recursively")
	f.archived = fs.Bool("archived", false, "Include archived projects and groups (left out by default)")
	fs.Var(&f.exclude, "exclude", "Namespace path or glob to leave out (repeatable, or comma-separated; adds to the workspace's exclude list)")
	f.format = fs.String("format", "auto", "Output format: auto, tree, text, or json")
	f.color = fs.String("color", "auto", "Colorize output: auto, always, or never")
	return f
}

// positionalTarget resolves a command's group argument from the positional
// form and the deprecated --path flag, refusing both at once.
func positionalTarget(cmd, flagValue string, positional []string) (string, error) {
	target := flagValue
	if len(positional) > 0 {
		if target != "" {
			return "", usageErrf("pass the group either as an argument or with --path, not both")
		}
		target = positional[0]
	}
	if len(positional) > 1 {
		return "", usageErrf("%s takes at most one group argument, got %d", cmd, len(positional))
	}
	return target, nil
}

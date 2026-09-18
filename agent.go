package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// This file implements the `gitty agent schema` command. It emits a machine
// readable description of gitty's commands modelled after the Model Context
// Protocol (MCP) tool definition. An LLM/agent can read this schema to learn
// which commands exist, what flags they take, and how to invoke them.

// AgentSchema is the top-level document emitted by `gitty agent schema`.
type AgentSchema struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	ExitCodes   map[string]string `json:"exitCodes,omitempty"`
	Tools       []AgentTool       `json:"tools"`
}

// AgentTool describes a single invokable gitty command in MCP "tool" form.
type AgentTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
	// Invocation shows the agent how the inputSchema maps onto an argv array.
	Invocation Invocation `json:"invocation"`
}

// InputSchema is a (subset of) JSON Schema describing a tool's arguments.
type InputSchema struct {
	Type       string                `json:"type"`
	Properties map[string]SchemaProp `json:"properties"`
	Required   []string              `json:"required,omitempty"`
}

// SchemaProp describes a single argument/flag.
type SchemaProp struct {
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Default     interface{} `json:"default,omitempty"`
}

// Invocation tells the agent how to turn arguments into a command line.
type Invocation struct {
	Command  string   `json:"command"`
	BaseArgs []string `json:"baseArgs"`
	// Each property in the input schema maps to a CLI flag of the form
	// "--<name>=<value>" for strings and "--<name>" for booleans.
	FlagStyle string `json:"flagStyle"`
}

// AgentSchemaVersion is reported in the schema so consumers can detect changes.
const AgentSchemaVersion = "3.0.0"

// buildAgentSchema constructs the schema describing every gitty command.
// It is the single source of truth used to render the agent-facing schema.
func buildAgentSchema() AgentSchema {
	return AgentSchema{
		Name:        "gitty",
		Version:     AgentSchemaVersion,
		Description: "A configurable CLI to synchronize (clone/pull) GitLab groups, subgroups, and repositories to the local machine while preserving the GitLab namespace directory structure.",
		ExitCodes: map[string]string{
			"0":   "success",
			"1":   "sync completed but one or more items failed (retryable)",
			"2":   "usage or configuration error (do not retry without changing the invocation)",
			"130": "interrupted (SIGINT/SIGTERM); a re-run recovers cleanly",
		},
		Tools: []AgentTool{
			{
				Name:        "init",
				Description: "Initialize a gitty workspace in the current directory by writing a .gitty/config file. Run this once in the root folder before syncing.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]SchemaProp{
						"url": {
							Type:        "string",
							Description: "Base URL of the GitLab instance. Change this for self-hosted GitLab.",
							Default:     "https://gitlab.com",
						},
						"ssh": {
							Type:        "boolean",
							Description: "Clone over SSH (git@...) using local SSH keys, instead of the default HTTP(S). Prefer the default for unattended runs: the token gitty already needs for the API authenticates the clones too, so no SSH key is required.",
							Default:     false,
						},
						"http": {
							Type:        "boolean",
							Description: "Clone over HTTP(S). This is the default; the flag is accepted for compatibility and is mutually exclusive with ssh.",
							Default:     true,
						},
						"force": {
							Type:        "boolean",
							Description: "Overwrite an existing .gitty/config. Without this, init refuses to clobber an initialized workspace.",
							Default:     false,
						},
						"token": {
							Type:        "string",
							Description: "GitLab access token to verify. Falls back to GITLAB_TOKEN or CI_JOB_TOKEN. Used only for the verification check; it is never stored in the workspace.",
						},
						"verify": {
							Type:        "boolean",
							Description: "Check the token against the instance and report the authenticated user, the token's scopes, and any scope this workspace needs but the token lacks (notably read_repository for HTTP cloning). Advisory only: it never fails init. Set false for offline setup.",
							Default:     true,
						},
						"allow-clone-host": {
							Type:        "string",
							Description: "Records an additional host this workspace expects to clone from, beyond the instance's own. Advisory: gitty never refuses a clone over the host, because the local git config (including url.<base>.insteadOf rules in conditional includes) has the final say on the URL. Listing a host only silences the note gitty prints when repositories come from somewhere other than the instance. Repeatable, and also accepts a comma-separated list.",
						},
					},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"init"},
					FlagStyle: "--<name>=<value> for strings, --<name> for booleans",
				},
			},
			{
				Name:        "sync",
				Description: "Sync a GitLab group based on the workspace's .gitty/config. Clones repositories that do not exist locally and runs 'git pull' on those that do. Requires a workspace created by 'init'.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]SchemaProp{
						"path": {
							Type:        "string",
							Description: "GitLab group or subgroup path to sync (e.g., 'tenant/images'). Required unless syncing from a managed subgroup directory that already has its own config.",
						},
						"token": {
							Type:        "string",
							Description: "GitLab access token. Falls back to the GITLAB_TOKEN or CI_JOB_TOKEN environment variables when omitted. Required unless --anon is set.",
						},
						"anon": {
							Type:        "boolean",
							Description: "Access public groups and repositories anonymously, without a token. Only public resources are visible in this mode.",
							Default:     false,
						},
						"verbose": {
							Type:        "boolean",
							Description: "Print each git invocation and its output to stderr, with URLs redacted. Event lines on stdout are unaffected.",
							Default:     false,
						},
						"reclone-broken": {
							Type:        "boolean",
							Description: "When a destination exists but is not a usable git repo, move it aside (renamed to <dir>.gitty-broken-<n>, never deleted) and clone fresh. Without this flag such destinations are reported as errors.",
							Default:     false,
						},
						"jobs": {
							Type:        "integer",
							Description: "Number of concurrent repo clone/pull operations (1-16).",
							Default:     4,
						},
						"accept-new-host-keys": {
							Type:        "boolean",
							Description: "For SSH clones, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new); a changed host key is still refused. Set this for unattended runs, where an interactive host-key prompt would otherwise hang the job.",
							Default:     false,
						},
						"groups": {
							Type:        "boolean",
							Description: "Fetch groups/subgroups and create their directory structure locally (with per-directory configs).",
							Default:     false,
						},
						"repos": {
							Type:        "boolean",
							Description: "Fetch and clone/pull repositories. Defaults to true when neither --groups nor --repos is passed.",
							Default:     false,
						},
						"nested": {
							Type:        "boolean",
							Description: "Recurse into nested subgroups and projects instead of only the immediate group.",
							Default:     false,
						},
						"dry-run": {
							Type:        "boolean",
							Description: "Print what would happen without creating directories or executing git commands.",
							Default:     false,
						},
						"allow-clone-host": {
							Type:        "string",
							Description: "Records an additional host this workspace expects to clone from, beyond the instance's own. Advisory: gitty never refuses a clone over the host, because the local git config (including url.<base>.insteadOf rules in conditional includes) has the final say on the URL. Listing a host only silences the note gitty prints when repositories come from somewhere other than the instance. Repeatable, and also accepts a comma-separated list.",
						},
					},
					Required: []string{"path"},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"sync"},
					FlagStyle: "--<name>=<value> for strings, --<name> for booleans",
				},
			},
			{
				Name:        "status",
				Description: "Report the branch and freshness of every git checkout in the workspace, one 'status <path> branch=... ahead=N behind=N dirty=BOOL' line per repository. Read-only: it never clones, pulls, or modifies the workspace.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]SchemaProp{
						"fetch": {
							Type:        "boolean",
							Description: "Refresh remote-tracking refs before reporting, so ahead/behind reflect the remote right now instead of the last sync. Requires network access and, for HTTP remotes, a token.",
							Default:     false,
						},
						"token": {
							Type:        "string",
							Description: "GitLab access token, only needed with fetch. Falls back to the GITLAB_TOKEN or CI_JOB_TOKEN environment variables.",
						},
						"anon": {
							Type:        "boolean",
							Description: "With fetch, contact public repositories anonymously instead of requiring a token.",
							Default:     false,
						},
						"jobs": {
							Type:        "integer",
							Description: "Number of concurrent repositories to inspect (1-16).",
							Default:     4,
						},
						"accept-new-host-keys": {
							Type:        "boolean",
							Description: "With fetch over SSH, record unknown host keys without prompting (ssh StrictHostKeyChecking=accept-new).",
							Default:     false,
						},
						"verbose": {
							Type:        "boolean",
							Description: "Print each git invocation and its output to stderr, with URLs redacted.",
							Default:     false,
						},
						"allow-clone-host": {
							Type:        "string",
							Description: "Records an additional host this workspace expects to clone from, beyond the instance's own. Advisory: gitty never refuses a clone over the host, because the local git config (including url.<base>.insteadOf rules in conditional includes) has the final say on the URL. Listing a host only silences the note gitty prints when repositories come from somewhere other than the instance. Repeatable, and also accepts a comma-separated list.",
						},
					},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"status"},
					FlagStyle: "--<name>=<value> for strings, --<name> for booleans",
				},
			},
			{
				Name:        "ls",
				Description: "List the remote groups and projects under a target, marking each project 'new' (a sync would clone it) or 'present' (already checked out). Read-only: it contacts the GitLab API but never invokes git or writes to the workspace.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]SchemaProp{
						"path": {
							Type:        "string",
							Description: "Group to list, resolved like a shell path against the workspace directory the command runs in: omitted or '.' means the current context (the instance's top-level groups at the workspace root), '/' always means the instance's top-level groups, '..' the parent group, and a leading '/' makes it absolute. Passed as a positional argument (--path is also accepted, but not both).",
						},
						"token": {
							Type:        "string",
							Description: "GitLab access token. Falls back to the GITLAB_TOKEN or CI_JOB_TOKEN environment variables. Required unless anon is set.",
						},
						"anon": {
							Type:        "boolean",
							Description: "List public groups and projects anonymously, without a token.",
							Default:     false,
						},
						"nested": {
							Type:        "boolean",
							Description: "Recurse into nested subgroups instead of listing only the immediate group. Per-group project counts are only complete in this mode.",
							Default:     false,
						},
						"format": {
							Type:        "string",
							Description: "Output format: 'auto' (an indented tree on a terminal, greppable event lines when piped), 'tree', 'text', or 'json'. Always pass 'json' explicitly when consuming this programmatically rather than relying on auto-detection.",
							Default:     "auto",
						},
						"color": {
							Type:        "string",
							Description: "Colorize the tree: 'auto' (only on a terminal), 'always', or 'never'. NO_COLOR is honoured. Irrelevant for the text and json formats.",
							Default:     "auto",
						},
					},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"ls"},
					FlagStyle: "the 'path' argument is positional (gitty ls <path>); other arguments are --<name>=<value> for strings, --<name> for booleans, and may appear on either side of it",
				},
			},
			{
				Name:        "version",
				Description: "Print the gitty binary's version as a single bare line. Release builds report their tag (e.g. 'v1.2.3'); builds from source report 'dev' plus the commit they were built from.",
				InputSchema: InputSchema{
					Type:       "object",
					Properties: map[string]SchemaProp{},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"version"},
					FlagStyle: "takes no flags",
				},
			},
		},
	}
}

// runAgentSchema prints the gitty agent schema as indented JSON to stdout.
func runAgentSchema() {
	schema := buildAgentSchema()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	// Keep '<' and '>' literal so the flag-style hints stay readable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(schema); err != nil {
		log.Fatalf("Failed to render agent schema: %v", err)
	}
}

// runAgent dispatches the `gitty agent <subcommand>` family.
func runAgent(args []string) {
	if len(args) < 1 {
		printAgentUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "schema":
		runAgentSchema()
	default:
		fmt.Printf("Unknown agent subcommand: %s\n", args[0])
		printAgentUsage()
		os.Exit(1)
	}
}

func printAgentUsage() {
	fmt.Println("Usage: gitty agent <subcommand>")
	fmt.Println("\nSubcommands:")
	fmt.Println("  schema    Print an MCP-style JSON schema describing how an LLM/agent should use gitty")
}

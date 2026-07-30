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
const AgentSchemaVersion = "1.1.0"

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
						"http": {
							Type:        "boolean",
							Description: "Use HTTP(S) cloning (https://...) instead of the default SSH (git@...). Recommended for CI runners.",
							Default:     false,
						},
						"force": {
							Type:        "boolean",
							Description: "Overwrite an existing .gitty/config. Without this, init refuses to clobber an initialized workspace.",
							Default:     false,
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
					},
					Required: []string{"path"},
				},
				Invocation: Invocation{
					Command:   "gitty",
					BaseArgs:  []string{"sync"},
					FlagStyle: "--<name>=<value> for strings, --<name> for booleans",
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

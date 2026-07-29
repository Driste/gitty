# gitty

> A minimal, configurable Go CLI that synchronizes (clones/pulls) GitLab groups,
> subgroups, and repositories to the local machine while preserving the exact
> GitLab namespace directory structure. This document is written for AI agents
> and LLMs so they can understand what `gitty` is for and how to drive it.

## What it is for

Use `gitty` when you need to mirror a GitLab group's repositories onto local
disk and keep them updated. It:

- Preserves the GitLab namespace as a directory tree (e.g. `tenant/images/app`
  becomes `./tenant/images/app`), preventing name collisions.
- Clones repositories that do not exist locally and runs `git pull` on the ones
  that do — a single command idempotently brings a workspace up to date.
- Anchors a workspace with a `.gitty/config` file so the GitLab URL and
  SSH/HTTP preference do not have to be repeated on every invocation.
- Works in CI: it automatically reads `GITLAB_TOKEN` or `CI_JOB_TOKEN`.

Use it for: bulk-cloning an org/group, keeping a local mirror in sync,
bootstrapping a workspace in CI. Do NOT use it for: pushing changes, managing
merge requests, or per-repository operations — it only clones/pulls.

## Installation

```bash
# Build from source
go build -o gitty .

# Or download a prebuilt binary + checksum from the GitHub Releases page,
# then verify:
sha256sum -c SHA256SUMS --ignore-missing
```

## Core concepts

- **Workspace**: a directory containing a `.gitty/config` file. Created by
  `gitty init`. All sync output is placed relative to this directory.
- **`.gitty/config`**: a TOML file with `url`, `http`, and `root_path`. When
  `gitty sync --groups` creates subgroup directories, each gets its own config
  with `root_path` set to that subgroup, so you can `cd` into it and run
  `gitty sync` without repeating `--path`.
- **Token resolution order** for `sync`: `--token` flag → `GITLAB_TOKEN` env →
  `CI_JOB_TOKEN` env. A token is required.

## Commands

### `gitty init` — create a workspace

Writes `.gitty/config` in the current directory. Run once in the root folder.

| Flag     | Type    | Default                 | Description |
| :------- | :------ | :---------------------- | :---------- |
| `--url`  | string  | `https://gitlab.com`    | Base URL of the GitLab instance (change for self-hosted). |
| `--http` | boolean | `false`                 | Clone over HTTPS instead of SSH. Recommended for CI runners. |

```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com" --http
```

### `gitty sync` — clone/pull a group

Requires a workspace created by `init`. Clones missing repos and pulls existing
ones.

| Flag        | Type    | Default | Description |
| :---------- | :------ | :------ | :---------- |
| `--path`    | string  | `""`    | GitLab group/subgroup path, e.g. `tenant/images`. Required unless run from a managed subgroup directory that already has its own config. |
| `--token`   | string  | `""`    | GitLab access token. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. |
| `--groups`  | boolean | `false` | Create the subgroup directory structure locally (each with its own config). |
| `--repos`   | boolean | `false` | Clone/pull repositories. Defaults to `true` when neither `--groups` nor `--repos` is passed. |
| `--nested`  | boolean | `false` | Recurse into nested subgroups/projects instead of only the immediate group. |
| `--dry-run` | boolean | `false` | Print planned actions without creating directories or running git. |

```bash
export GITLAB_TOKEN="glpat-XXXXXXXX"

# Clone/pull repos directly inside a group (flat)
gitty sync --path="tenant/images"

# Clone/pull EVERYTHING recursively
gitty sync --path="tenant/images" --nested

# Only recreate the group folder hierarchy, no repos
gitty sync --path="engineering" --groups --nested

# Preview without making changes
gitty sync --path="tenant/images" --nested --dry-run
```

How `--groups` and `--repos` interact:
- `--path=tenant` alone → syncs only the immediate repositories in `tenant`.
- `--path=tenant --groups` → creates only the empty subgroup directory tree.
- `--path=tenant --groups --repos` → both.

### `gitty agent schema` — emit a machine-readable tool schema

Prints an MCP-style JSON description of every gitty command (name, description,
JSON-Schema `inputSchema`, and an `invocation` hint mapping arguments onto the
CLI). Consume this to drive gitty as a tool without hand-writing a definition.

```bash
gitty agent schema
```

Output shape:

```json
{
  "name": "gitty",
  "version": "1.0.0",
  "description": "A configurable CLI to synchronize ... GitLab groups ...",
  "tools": [
    {
      "name": "sync",
      "description": "Sync a GitLab group based on the workspace's .gitty/config ...",
      "inputSchema": {
        "type": "object",
        "properties": {
          "path":   { "type": "string",  "description": "GitLab group or subgroup path ..." },
          "nested": { "type": "boolean", "description": "Recurse into nested subgroups ...", "default": false }
        },
        "required": ["path"]
      },
      "invocation": {
        "command": "gitty",
        "baseArgs": ["sync"],
        "flagStyle": "--<name>=<value> for strings, --<name> for booleans"
      }
    }
  ]
}
```

Mapping arguments to a command line: a tool's arguments become flags per
`invocation.flagStyle` — strings render as `--<name>=<value>` and booleans as a
bare `--<name>` when true (omitted when false). For example, the `sync` tool
with `{"path": "tenant/images", "nested": true}` becomes:

```bash
gitty sync --path=tenant/images --nested
```

## Typical agent workflow

1. Run `gitty agent schema` to discover the available commands and arguments.
2. Ensure a workspace exists: if there is no `.gitty/config`, run `gitty init`
   (choose `--http` for CI/token-based cloning).
3. Ensure a token is available via `--token` or the `GITLAB_TOKEN` /
   `CI_JOB_TOKEN` environment variable.
4. Run `gitty sync --path=<group>` (add `--nested` for the full tree). Prefer
   `--dry-run` first to preview the actions.

## CI/CD example (GitLab)

`gitty` auto-detects `CI_JOB_TOKEN`. Use `--http` because CI runners usually
cannot use SSH.

```yaml
clone_all_repos:
  image: golang:latest
  script:
    - go build -o gitty .
    - ./gitty init --http
    - ./gitty sync --path="tenant/images" --nested
```

## Notes and gotchas

- `sync` must be run from a workspace directory; if there is no `.gitty/config`
  it exits with an error telling you to run `gitty init` first.
- Cloning uses the local git environment (`SSH_AUTH_SOCK`, `~/.gitconfig`), so
  SSH cloning requires working SSH keys; otherwise use `--http`.
- The tool never deletes local repositories; it only clones new ones and pulls
  existing ones.

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
- Clones repositories that do not exist locally and runs `git pull --ff-only`
  on the ones that do — a single command idempotently brings a workspace up to
  date, and diverged/dirty checkouts fail loudly instead of being merged.
- Anchors a workspace with a `.gitty/config` file so the GitLab URL and
  SSH/HTTP preference do not have to be repeated on every invocation.
- Works in CI: it automatically reads `GITLAB_TOKEN` or `CI_JOB_TOKEN`, uses
  the token for HTTP git authentication (as `oauth2` / `gitlab-ci-token`
  respectively), and exits non-zero when any item fails.
- Emits one machine-readable event per line on stdout (`clone <path>`,
  `pull <path>`, `group <path>`, `reclone <path>`, `error <path> <reason>`,
  `plan <action> <path>` under --dry-run, and a final
  `summary cloned=N pulled=N skipped=N errors=N`); human diagnostics go to
  stderr. Parse stdout only.

Use it for: bulk-cloning an org/group, keeping a local mirror in sync,
bootstrapping a workspace in CI, checking which local checkouts are stale or
dirty (`status`), and previewing what a sync would bring down (`ls`). Do NOT
use it for: pushing changes, managing merge requests, or per-repository
operations — it only clones/pulls.

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
  `CI_JOB_TOKEN` env. A token is required unless `--anon` is passed (public
  resources only). In `--http` mode the token also authenticates git itself,
  handed over via an internal askpass re-exec — never on the command line and
  never written to disk.
- **Exit codes**: `0` success, `1` completed with per-item failures, `2` usage
  or configuration error (do not retry unchanged), `130` interrupted.

## Commands

### `gitty init` — create a workspace

Writes `.gitty/config` in the current directory. Run once in the root folder.

| Flag      | Type    | Default                 | Description |
| :-------- | :------ | :---------------------- | :---------- |
| `--url`   | string  | `https://gitlab.com`    | Base URL of the GitLab instance (change for self-hosted). Must be http(s). Inside a GitLab CI job, defaults to `CI_SERVER_URL` when not passed. |
| `--http`  | boolean | `false`                 | Clone over HTTPS instead of SSH. Recommended for CI runners. |
| `--force` | boolean | `false`                 | Overwrite an existing `.gitty/config`; without it, re-init refuses (exit 2). |

```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com" --http
```

### `gitty sync` — clone/pull a group

Requires a workspace created by `init`. Clones missing repos and pulls existing
ones.

| Flag               | Type    | Default | Description |
| :----------------- | :------ | :------ | :---------- |
| `--path`           | string  | `""`    | GitLab group/subgroup path, e.g. `tenant/images`. Required unless run from a managed subgroup directory that already has its own config. |
| `--token`          | string  | `""`    | GitLab access token. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. Required unless `--anon`. |
| `--anon`           | boolean | `false` | Sync public groups/repositories anonymously, without a token. |
| `--groups`         | boolean | `false` | Create the subgroup directory structure locally (each with its own config). |
| `--repos`          | boolean | `false` | Clone/pull repositories. Defaults to `true` when neither `--groups` nor `--repos` is passed. |
| `--nested`         | boolean | `false` | Recurse into nested subgroups/projects instead of only the immediate group. |
| `--dry-run`        | boolean | `false` | Print planned actions (`plan <action> <path>` events) without creating directories or running git. Summary line matches the real run. |
| `--jobs`           | integer | `4`     | Concurrent repo clone/pull operations (1-16). |
| `--verbose`        | boolean | `false` | Print each git invocation and its output to stderr (URLs redacted). |
| `--reclone-broken` | boolean | `false` | Move aside non-repo destinations (renamed `<dir>.gitty-broken-<n>`, never deleted) and clone fresh; without it they are `error` events. |

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

### `gitty status` — report local checkout state

Walks the workspace and prints one line per git checkout. Read-only; no token
or network needed unless `--fetch` is passed.

| Flag        | Type    | Default | Description |
| :---------- | :------ | :------ | :---------- |
| `--fetch`   | boolean | `false` | Refresh remote-tracking refs first so `behind` reflects the remote now. Needs network, and a token for HTTP remotes. |
| `--token`   | string  | `""`    | Only used with `--fetch`. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. |
| `--anon`    | boolean | `false` | With `--fetch`, contact public repositories without a token. |
| `--jobs`    | integer | `4`     | Repositories inspected concurrently (1-16). |
| `--verbose` | boolean | `false` | Print git invocations to stderr (URLs redacted). |

```
status tenant/images/app branch=main ahead=0 behind=3 dirty=false
status tenant/images/spike branch=exp ahead=0 behind=0 dirty=false upstream=none
summary repos=2 dirty=0 ahead=0 behind=1 errors=0
```

`dirty=true` includes untracked files. `upstream=none` means the branch has no
tracking ref, so ahead/behind are unknowable rather than zero — do not read
`0/0` as "in sync" when that marker is present.

### `gitty ls` — preview a group's contents

Lists remote groups/projects under a target and whether each project is
already checked out. Contacts the API; never runs git or writes to disk.

| Flag       | Type    | Default | Description |
| :--------- | :------ | :------ | :---------- |
| `--path`   | string  | `""`    | Group path to list. Required unless run from a managed subgroup directory. |
| `--token`  | string  | `""`    | Access token; falls back to env vars. Required unless `--anon`. |
| `--anon`   | boolean | `false` | List public resources without a token. |
| `--nested` | boolean | `false` | Recurse into subgroups. Per-group counts are only complete in this mode. |
| `--format` | string  | `text`  | `text` (event lines), `tree` (indented), or `json`. Prefer `json` when parsing. |

```
group tenant/images projects=2
project tenant/images/app present
project tenant/images/lib new
summary groups=1 projects=2 new=1 present=1
```

Use `ls` to answer "what would a sync clone, and how much?" without touching
the filesystem; use `sync --dry-run` when you want the plan in sync's own
event vocabulary.

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
   `CI_JOB_TOKEN` environment variable (or pass `--anon` for public groups).
4. Optionally run `gitty ls --path=<group> --nested --format=json` to see how
   many projects the group holds and which are not yet cloned.
5. Run `gitty sync --path=<group>` (add `--nested` for the full tree). Prefer
   `--dry-run` first to preview the actions.
6. Run `gitty status` afterwards (or later) to see which checkouts are dirty or
   behind; add `--fetch` for remote-accurate `behind` counts.

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
  it exits 2 with an error telling you to run `gitty init` first.
- SSH cloning uses the local git environment (`SSH_AUTH_SOCK`, `~/.gitconfig`)
  and requires working SSH keys; in `--http` mode gitty authenticates git
  itself with the resolved token, and interactive credential prompts are
  disabled so bad credentials fail fast instead of hanging.
- The tool never deletes local repositories; it only clones new ones and
  fast-forwards existing ones. Even `--reclone-broken` renames aside rather
  than deleting.
- Diverged or dirty checkouts make `git pull --ff-only` fail: the repo is
  reported as an `error` event and left untouched for manual resolution.
- Ctrl-C (SIGINT) is safe: in-flight git processes are signalled to clean up,
  the run exits 130, and a re-run picks up where it stopped.

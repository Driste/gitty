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

Check what you are running with `gitty version`: a release binary prints its
tag (e.g. `v1.0.0`), a source build prints `dev` plus the commit it came from
(e.g. `dev+5f76104a77d5`). It takes no flags and prints one bare line.

## Core concepts

- **Workspace**: a directory containing a `.gitty/config` file. Created by
  `gitty init`. All sync output is placed relative to this directory.
- **`.gitty/config`**: a TOML file with `url`, `http`, and `root_path`. When
  `gitty sync --groups` creates subgroup directories, each gets its own config
  with `root_path` set to that subgroup, so you can `cd` into it and run
  `gitty sync` without repeating `--path`.
- **Transport**: HTTP(S) by default (the API token authenticates clones too, so
  no SSH keys are needed); `gitty init --ssh` selects SSH. The choice is stored
  explicitly in `.gitty/config`, so an existing workspace keeps its transport.
- **Token resolution order** for `sync`: `--token` flag → `GITLAB_TOKEN` env →
  `CI_JOB_TOKEN` env. A token is required unless `--anon` is passed (public
  resources only). `--anon` ignores ambient env tokens rather than silently
  using them, and conflicts with an explicit `--token`.
- **Token scopes**: in HTTP(S) mode the token also authenticates git itself,
  handed to git via an internal askpass re-exec — never on the command line,
  never written to disk. It therefore needs `read_repository` in addition to
  `api`/`read_api`: an API-only token lists groups fine but cannot clone.
  gitty detects that failure and prints a hint naming the scope.
- **Exit codes**: `0` success, `1` completed with per-item failures, `2` usage
  or configuration error (do not retry unchanged), `130` interrupted.

## Commands

### `gitty init` — create a workspace

Writes `.gitty/config` in the current directory. Run once in the root folder.

| Flag      | Type    | Default                 | Description |
| :-------- | :------ | :---------------------- | :---------- |
| `--url`   | string  | `https://gitlab.com`    | Base URL of the GitLab instance (change for self-hosted). Must be http(s). Inside a GitLab CI job, defaults to `CI_SERVER_URL` when not passed. |
| `--ssh`   | boolean | `false`                 | Clone over SSH (`git@...`) with local SSH keys. |
| `--http`  | boolean | `true`                  | Clone over HTTP(S). The default; accepted for compatibility, conflicts with `--ssh`. |
| `--force` | boolean | `false`                 | Overwrite an existing `.gitty/config`; without it, re-init refuses (exit 2). |
| `--token` | string  | `""`                    | Token to verify; falls back to env vars. Never stored. |
| `--verify`| boolean | `true`                  | Check the token against the instance and report its scopes. Advisory: never fails init. |

```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com"
```

`init` verifies the resolved token: it reports the authenticated user, the
token's scopes where the instance supports introspection, and warns when a
scope needed by this workspace is missing (notably `read_repository` for an
HTTP workspace). Missing, rejected and unreachable are reported distinctly.
The check never fails init; `--verify=false` skips it.

### `gitty sync` — clone/pull a group

Requires a workspace created by `init`. Clones missing repos and pulls existing
ones.

| Flag               | Type    | Default | Description |
| :----------------- | :------ | :------ | :---------- |
| `--path`           | string  | `""`    | GitLab group/subgroup path, e.g. `tenant/images`. Required unless run from a managed subgroup directory that already has its own config. |
| `--token`          | string  | `""`    | GitLab access token. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. Required unless `--anon`. |
| `--anon`           | boolean | `false` | Sync public groups/repositories anonymously. Ambient `GITLAB_TOKEN` / `CI_JOB_TOKEN` are ignored; conflicts with `--token`. |
| `--groups`         | boolean | `false` | Create the subgroup directory structure locally (each with its own config). |
| `--repos`          | boolean | `false` | Clone/pull repositories. Defaults to `true` when neither `--groups` nor `--repos` is passed. |
| `--nested`         | boolean | `false` | Recurse into nested subgroups/projects instead of only the immediate group. |
| `--dry-run`        | boolean | `false` | Print planned actions (`plan <action> <path>` events) without creating directories or running git. Summary line matches the real run. |
| `--jobs`           | integer | `4`     | Concurrent repo clone/pull operations (1-16). |
| `--verbose`        | boolean | `false` | Print each git invocation and its output to stderr (URLs redacted). |
| `--reclone-broken` | boolean | `false` | Move aside non-repo destinations (renamed `<dir>.gitty-broken-<n>`, never deleted) and clone fresh; without it they are `error` events. |
| `--accept-new-host-keys` | boolean | `false` | SSH only: record unknown host keys without prompting (`StrictHostKeyChecking=accept-new`); a changed key is still refused. Set this for unattended runs. |
| `--related`        | string  | `""`    | Sync the projects a project is linked to instead of a group: `--related` or `--related=uses` (what it uses), `--related=used-by`, `--related=both`. The value needs the equals sign. See "Cross-repository context" below. |

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

### `gitty ls` — browse groups and projects

Lists remote groups/projects under a target and whether each project is already
checked out. Contacts the API; never runs git or writes to disk.

The target is a POSITIONAL argument resolved like a shell path against the
workspace directory you are in (`--path` still works but cannot be combined
with it):

| Argument      | Meaning |
| :------------ | :------ |
| *(none)* or `.` | The current context: the workspace's `root_path`, or the instance's top-level groups at the workspace root. |
| `/`           | Always the instance's top-level groups. |
| `a/b`         | Relative to the current context. |
| `/a/b`        | Absolute, from the instance root. |
| `..`          | The parent group; clamped at the instance root. |

| Flag       | Type    | Default | Description |
| :--------- | :------ | :------ | :---------- |
| `--token`  | string  | `""`    | Access token; falls back to env vars. Required unless `--anon`. |
| `--anon`   | boolean | `false` | List public resources without a token. |
| `--nested` | boolean | `false` | Recurse into subgroups. Per-group counts are only complete in this mode. |
| `--format` | string  | `auto`  | `auto`, `tree`, `text`, or `json`. |
| `--color`  | string  | `auto`  | `auto`, `always`, or `never`. `NO_COLOR` is honoured. |

Output adapts to the destination, like `ls(1)`: a colored tree on a terminal,
and the greppable one-event-per-line `text` format when piped or redirected.
**When parsing, pass `--format=json` explicitly** rather than relying on
detection. The text form is:

```
group tenant/images projects=2
project tenant/images/app present
project tenant/images/lib new
summary groups=1 projects=2 new=1 present=1
```

Flags may appear before or after the positional argument.

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

### Cross-repository context — what else matters to a project

gitty knows which projects of the instance refer to which: it reads the
checkouts it manages for references (CI `include:`/`trigger:`/`component:`,
dependencies named in package manifests, clone URLs in CI scripts, container
images from the instance's registry, submodules, forks, links in
documentation) and keeps them in an index at `<workspace>/.gitty/index.json`. Use this before
changing a project, to find the code it depends on and the code that depends
on it.

```bash
gitty context                      # brief the project this directory is a checkout of
gitty context acme/app --fetch     # ...first cloning what it uses and is not here yet
gitty context --format=json        # the same briefing as one JSON document
gitty related acme/lib             # just the links, as greppable lines
gitty fetch acme/lib acme/tools    # make sure these projects are checked out
gitty sync --related               # clone what this project uses, and pull what is already here
gitty index                        # refresh project metadata from the instance
```

`gitty context --format=json` prints (abridged):

```json
{
  "schema": 1,
  "workspace": "/ws",
  "subject": { "kind": "project", "project": "acme/app", "present": true, "path": "/ws/acme/app", "relative_path": "." },
  "uses": [
    {
      "project": "acme/lib",
      "description": "Shared domain types",
      "present": true, "fetched": false,
      "path": "/ws/acme/lib", "relative_path": "../lib",
      "links": [ { "from": "acme/app", "to": "acme/lib", "kind": "depends", "evidence": "go.mod:12" } ],
      "docs": ["AGENTS.md", "README.md"]
    },
    {
      "project": "acme/ci-templates",
      "present": true, "fetched": true,
      "path": "/ws/acme/ci-templates", "relative_path": "../ci-templates",
      "links": [ { "from": "acme/app", "to": "acme/ci-templates", "kind": "ci-include", "evidence": ".gitlab-ci.yml:3", "target": "templates/build.yml" } ],
      "files": ["templates/build.yml"],
      "docs": ["README.md"]
    },
    {
      "project": "acme/base-images",
      "present": false,
      "fetch_command": "gitty fetch acme/base-images",
      "links": [ { "from": "acme/app", "to": "acme/base-images", "kind": "image", "evidence": "Dockerfile:1" } ]
    }
  ],
  "used_by": [ { "project": "acme/web", "present": true, "path": "/ws/acme/web", "links": [ { "from": "acme/web", "to": "acme/app", "kind": "depends", "evidence": "package.json:21" } ] } ],
  "uses_total": 3, "used_by_total": 1,
  "unresolved": [ { "from": "acme/app", "ref": "acme/old-lib", "kind": "depends", "evidence": "go.mod:14" } ],
  "coverage": { "projects": 42, "checked_out": 4 }
}
```

How to read it:

- `path` is absolute and exists whenever `present` is true. `evidence` is
  `file:line` inside the project named by the link's `from` — the subject for
  `uses`, the other project for `used_by` — or `api:<field>`.
- Link kinds: `depends`, `ci-include`, `ci-trigger`, `ci-component`, `image`,
  `submodule`, `fork`, `mentions`. `mentions` is a link in documentation: the
  weakest kind, listed last and never fetched on its own account.
- `target` is what the reference names inside the other project, as written:
  the files a pipeline includes, a component's name, a module's directory.
  `files` are the ones among them that exist in the checkout, relative to its
  `path` — the place to start reading.
- **`used_by` is only as complete as the workspace.** Links are found by
  reading checkouts; `coverage` says how many of the known projects are
  checked out. Do not conclude "nothing else uses this" from an empty
  `used_by` when `checked_out` is far below `projects`.
- `unresolved` are references that point at the instance but match no
  project: deleted, private, or simply not asked about yet. gitty asks the
  instance about each one when it is online (`gitty index`, `gitty context
  --fetch`), which is how a project in a group the workspace has never
  cloned, or one that has been renamed, still becomes a link. They are never
  guessed at.
- A checkout is of the project's default branch, which is not necessarily the
  version another project pins. The evidence line shows what is pinned.
- `--fetch` only clones. A project that is present is never pulled or
  modified. Exit code 1 with a briefing on stdout means a fetch failed; the
  entry has `fetch_error`.
- `gitty sync --related` is the one that also updates: it clones the related
  projects that are absent and fast-forwards the ones that are present, with
  `sync`'s events (`clone`, `pull`, `skip`, `error`, `summary`) and
  `--dry-run`. Do not run it on checkouts you have uncommitted work in
  progress on unless a pull is what you want.
- With an index in place `context` and `related` need no network and no
  token. Building the index, and fetching, need `--token`/`GITLAB_TOKEN` or
  `--anon`.

If you are connected to gitty's MCP server (`gitty mcp`, a separate plugin
binary `gitty-mcp`), the same things are tools: `gitty_context` (fetches by
default), `gitty_related`, `gitty_projects`, `gitty_fetch`, `gitty_search`
(git grep across the checkouts) and `gitty_read` (a tracked file from a
checkout). Pass `project` as a namespace path or as the absolute path of your
working directory.

## Typical agent workflow

1. Run `gitty agent schema` to discover the available commands and arguments.
2. Ensure a workspace exists: if there is no `.gitty/config`, run `gitty init`
   (HTTP(S) is the default and needs no SSH keys; pass `--ssh` to use keys).
3. Ensure a token is available via `--token` or the `GITLAB_TOKEN` /
   `CI_JOB_TOKEN` environment variable (or pass `--anon` for public groups).
4. Optionally run `gitty ls --path=<group> --nested --format=json` to see how
   many projects the group holds and which are not yet cloned.
5. Run `gitty sync --path=<group>` (add `--nested` for the full tree). Prefer
   `--dry-run` first to preview the actions.
6. Run `gitty status` afterwards (or later) to see which checkouts are dirty or
   behind; add `--fetch` for remote-accurate `behind` counts.
7. Before changing a project, run `gitty context --format=json` in its
   checkout (add `--fetch` to clone the projects it uses) and read the related
   projects at the paths it returns.

## CI/CD example (GitLab)

`gitty` auto-detects `CI_JOB_TOKEN`. HTTP(S) is the default transport, so a CI
runner needs no SSH keys.

```yaml
clone_all_repos:
  image: golang:latest
  script:
    - go build -o gitty .
    - ./gitty init
    - ./gitty sync --path="tenant/images" --nested
```

## Notes and gotchas

- `sync` must be run from a workspace directory; if there is no `.gitty/config`
  it exits 2 with an error telling you to run `gitty init` first.
- SSH cloning uses the local git environment (`SSH_AUTH_SOCK`, `~/.gitconfig`)
  and requires working SSH keys; in the default HTTP(S) mode gitty authenticates git
  itself with the resolved token, and interactive credential prompts are
  disabled so bad credentials fail fast instead of hanging.
- SSH host keys: ssh prompts for confirmation of an unknown host key and reads
  the answer from the terminal, not from gitty. gitty syncs the first
  repository alone so that prompt happens once before the worker pool fans
  out. For unattended runs pass `--accept-new-host-keys`, which records
  unknown keys automatically (a changed key is still refused).
- Transport is pinned: whichever URL gitty selects (HTTPS by default, SSH with
  `--ssh`) is the URL git contacts. A `url.<base>.insteadOf` rule in the
  local git config cannot silently switch the transport, which would otherwise
  turn an HTTP workspace into SSH clones and strand the injected credentials.
  gitty notes on stderr when it overrides such a rule. To clone over SSH, run
  `gitty init --ssh` rather than relying on a rewrite.
- The tool never deletes local repositories; it only clones new ones and
  fast-forwards existing ones. Even `--reclone-broken` renames aside rather
  than deleting.
- Diverged or dirty checkouts make `git pull --ff-only` fail: the repo is
  reported as an `error` event and left untouched for manual resolution.
- Ctrl-C (SIGINT) is safe: in-flight git processes are signalled to clean up,
  the run exits 130, and a re-run picks up where it stopped.

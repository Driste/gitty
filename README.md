# gitty

A minimal, configurable Go CLI tool to synchronize (clone/pull) GitLab groups, subgroups, and repositories directly to your local machine.

`gitty` uses a local `gitty.toml` configuration file to anchor your workspace, preserving the exact namespace directory structure of your GitLab environment to prevent naming collisions.

## Features
* **Workspace Config**: Initialize a workspace with `gitty init` so you don't have to repeatedly pass your GitLab URL or SSH/HTTP preferences.
* **Granular Syncing**: Choose to sync only repositories, only empty group directory structures, or both.
* **Recursive or Flat**: Sync only the immediate group, or use the `--nested` flag to recursively pull everything underneath it.
* **Smart Updates**: Automatically runs `git pull --ff-only` if the local directory exists, or `git clone` if it doesn't. Fast-forward-only pulls avoid surprise merge commits — a diverged or dirty checkout fails loudly and is reported instead of silently merged.
* **Dry Runs**: Test your sync commands safely with `--dry-run` to see exactly what folders will be created and which repos will be cloned.
* **CI/CD Ready**: Automatically detects `GITLAB_TOKEN` or `CI_JOB_TOKEN` environment variables, and exits non-zero when any group or repository fails to sync so a broken pipeline stage is never reported green.
* **Safe Destinations**: Refuses to write outside the workspace (namespace paths containing `..` or absolute paths are skipped) and verifies each clone URL points at the configured GitLab host before running `git clone`.

---

## Installation

Ensure you have Go installed, then clone this repository and build the binary:

```bash
# Initialize module and download dependencies
go mod tidy

# Build the executable
go build -o gitty .

# (Optional) Install globally
sudo mv gitty /usr/local/bin/
```

---

## Configuration (`gitty init`)

Before syncing, you need to initialize your workspace. Navigate to the root folder where you want your GitLab directory structure to live and run:

```bash
gitty init [flags]
```

### Init Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--url` | `https://gitlab.com` | The base URL of your GitLab instance (change this if using self-hosted GitLab). Must be an `http(s)://` URL. |
| `--http` | `false` | Use HTTP(S) for cloning (`https://...`) instead of the default SSH (`git@...`). |
| `--force` | `false` | Overwrite an existing `.gitty/config`. Without it, `init` refuses to clobber an initialized workspace (which would reset its `root_path`). |

**Example:**
```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com" --http
```
This generates a `gitty.toml` file in the current directory. `gitty` will use this directory as the root destination for all future sync commands.

---

## Usage (`gitty sync`)

Once your workspace is initialized, you can pull down your groups and repositories. 

```bash
gitty sync --path="your/gitlab/group/path" [flags]
```

### Sync Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--path` | `""` | **(Required)** The GitLab group or subgroup path (e.g., `tenant/images`). |
| `--token` | `""` | Your GitLab Access Token. Falls back to `GITLAB_TOKEN` or `CI_JOB_TOKEN` env vars. Required unless `--anon` is set. |
| `--anon` | `false` | Sync public groups and repositories anonymously, without a token. Only public resources are visible in this mode. |
| `--groups` | `false` | Only fetch groups/subgroups and create their directory structure locally. |
| `--repos` | `false` | Only fetch and clone/pull repositories. *(Note: If neither `--groups` nor `--repos` is passed, it defaults to `--repos`)*. |
| `--nested` | `false` | Include nested subgroups and projects recursively. |
| `--dry-run`| `false` | Print planned actions (`plan clone <path>` etc.) without creating directories or executing git commands. Dry-run output is diffable against a real run's actions and produces the identical `summary` line. |
| `--jobs` | `4` | Number of concurrent repo clone/pull operations (1-16). `--jobs=1` restores fully serial behavior. |
| `--verbose` | `false` | Print each git invocation and its output to stderr, with URL credentials redacted. |
| `--reclone-broken` | `false` | When a destination exists but is not a usable git repo (e.g. a wedged partial clone), move it aside (renamed to `<dir>.gitty-broken-<n>`, never deleted) and clone fresh. |

### Output and exit codes

`gitty sync` writes one machine-readable event per line to **stdout** — stable
prefixes, grep-friendly — while all human diagnostics (banners, progress, git
output) go to **stderr**:

```
clone tenant/images/app        # repo cloned
pull tenant/images/app         # repo fast-forwarded (git pull --ff-only)
group tenant/images            # group dir + nested config ensured
reclone tenant/images/app      # broken checkout moved aside and re-cloned
error tenant/images/app git pull failed
plan clone tenant/images/app   # --dry-run: "plan " + the exact action line
summary cloned=3 pulled=12 skipped=0 errors=1   # always the last line
```

Exit codes: `0` success · `1` completed with per-item failures · `2` usage or
configuration error · `130` interrupted (Ctrl-C; git is signalled cleanly and
a re-run recovers the workspace).

### Authentication for HTTP clones

In `--http` mode, gitty authenticates `git clone`/`git pull` itself: it
re-execs as git's askpass helper and hands the token over via the child
process environment — never on the command line, never written to any git
config or credential store (ambient credential helpers are disabled for the
invocation). Personal/project access tokens authenticate as `oauth2`; a
`CI_JOB_TOKEN` authenticates as `gitlab-ci-token` automatically. Credentials
are only ever sent to the host of the configured instance URL.

### How `--groups` and `--repos` work together:
* `gitty sync --path="tenant"`: Syncs **only** the immediate repositories inside `tenant`.
* `gitty sync --path="tenant" --groups`: Creates **only** the empty directory structure for the `tenant` group and its immediate subgroups.
* `gitty sync --path="tenant" --groups --repos`: Creates the empty directory structure for subgroups, **and** syncs the immediate repositories.

---

## Examples

### 1. Standard Sync (Flat)
Sync all repositories directly inside `tenant/images` (does not pull repos inside nested subgroups).
```bash
export GITLAB_TOKEN="glpat-YOUR_PERSONAL_TOKEN"

gitty sync --path="tenant/images"
```

### 2. Full Recursive Sync (Nested)
Sync **everything** (all repositories in the group and all repositories in every subgroup beneath it).
```bash
gitty sync --path="tenant/images" --nested
```

### 3. Recreate Group Hierarchy
Only create the folder structure for all subgroups beneath `engineering`, leaving them empty.
```bash
gitty sync --path="engineering" --groups --nested
```

### 4. Dry Run
Safely check what repositories would be downloaded recursively before actually doing it.
```bash
gitty sync --path="tenant/images" --nested --dry-run
```

### 5. Anonymous Public Sync
Sync a public group without any token (only public groups and repositories are visible).
```bash
gitty sync --path="gitlab-examples/wayne-enterprises" --nested --anon
```

### 6. GitLab CI/CD Pipeline
`gitty` automatically picks up the ephemeral `CI_JOB_TOKEN` for both the API
and the git transport (authenticating as `gitlab-ci-token`), and `gitty init`
defaults the instance URL to `CI_SERVER_URL` inside a CI job. Use HTTP during
the `init` step, as CI runners typically can't use SSH. A failed sync exits
non-zero, failing the job.
```yaml
stages:
  - sync

clone_all_repos:
  stage: sync
  image: golang:latest
  script:
    - go build -o gitty .
    - ./gitty init --http
    - ./gitty sync --path="tenant/images" --nested
```

---

## Inspecting a workspace (`gitty status`)

Reports the branch and freshness of every checkout in the workspace, one line
per repository. It is read-only and needs no token or network access — results
reflect the last sync unless you pass `--fetch`.

```bash
gitty status
gitty status --fetch     # refresh remote-tracking refs first (needs a token for HTTP remotes)
```

```
status tenant/images/app branch=main ahead=0 behind=3 dirty=false
status tenant/images/lib branch=main ahead=1 behind=0 dirty=true
status tenant/images/spike branch=experiment ahead=0 behind=0 dirty=false upstream=none
summary repos=3 dirty=1 ahead=1 behind=1 errors=0
```

`dirty=true` means the working tree has changes (including untracked files).
`upstream=none` marks a branch with no tracking ref, where ahead/behind are
unknowable rather than zero.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--fetch` | `false` | Refresh remote-tracking refs before reporting, so `behind` reflects the remote right now. |
| `--token` | `""` | Only needed with `--fetch`. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. |
| `--anon` | `false` | With `--fetch`, contact public repositories without a token. |
| `--jobs` | `4` | Repositories inspected concurrently (1-16). |
| `--verbose` | `false` | Print each git invocation to stderr (URLs redacted). |

---

## Previewing a group (`gitty ls`)

Lists the remote groups and projects under a target, with a project count per
group, marking each project `new` (a sync would clone it) or `present`
(already checked out). It never invokes git and never writes to the workspace —
use it to see what a sync *would* bring down, and how much.

```bash
gitty ls --path="tenant/images" --nested
gitty ls --path="tenant/images" --nested --format=tree
gitty ls --path="tenant/images" --nested --format=json
```

```
group tenant/images projects=2
project tenant/images/app present
project tenant/images/lib new
summary groups=1 projects=2 new=1 present=1
```

`--format=tree` renders the same data as an indented namespace tree with `+`
(would clone) and `=` (present) markers; `--format=json` emits a structured
document for programmatic use.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--path` | `""` | **(Required)** GitLab group or subgroup path, unless run from a managed subgroup directory. |
| `--token` | `""` | GitLab access token. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. Required unless `--anon`. |
| `--anon` | `false` | List public groups and projects anonymously. |
| `--nested` | `false` | Recurse into nested subgroups. Per-group project counts are only complete in this mode. |
| `--format` | `text` | `text` (greppable event lines), `tree` (indented tree), or `json`. |

---

## Agent Schema (`gitty agent schema`)

`gitty agent schema` prints a machine-readable, MCP-style JSON description of
every gitty command — its purpose, arguments, defaults, and how to turn those
arguments into a command line. Feed this to an LLM or agent so it knows how to
drive `gitty` as a tool without you having to hand-write a tool definition.

```bash
gitty agent schema
```

The output is a single JSON document shaped like an MCP tool list:

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

Each tool's `inputSchema` is JSON Schema, and `invocation` tells the agent how
to map the arguments onto an argv array (e.g. the `sync` tool with
`{"path": "tenant/images", "nested": true}` becomes
`gitty sync --path=tenant/images --nested`).

---

## Development

Run the full test suite (unit + end-to-end):

```bash
go test ./...
```

The end-to-end tests build the real `gitty` binary and drive it as a subprocess
against a local fake GitLab API server that also serves actual git repositories
over HTTP, so clone/pull behavior, exit codes, pagination, and the safety
guards are all exercised without a network connection or token. Skip them for a
fast unit-only run with:

```bash
go test -short ./...
```

---

## TODO

- [x] Add a gitty config to each group so that you can go into them and pull from that path
- [x] For gitlab pipelines, use the CI_ var for the git repo (`CI_JOB_TOKEN` authenticates git; `init` defaults to `CI_SERVER_URL`)
- [x] Async pull down repos (`--jobs`)
- [x] Show repos current branches and if they are out of date, maybe a cache (`gitty status`, with `--fetch`)
- [x] Show how many projects are in each group (`gitty ls`)
- [ ] Show the current groups/projects and which will be removed or added when doing a subsequent run (`gitty ls` covers *added*; *removed* still needs orphan detection)
# gitty

A minimal, configurable Go CLI tool to synchronize (clone/pull) GitLab groups, subgroups, and repositories directly to your local machine.

`gitty` uses a local `.gitty/config` file to anchor your workspace, preserving the exact namespace directory structure of your GitLab environment to prevent naming collisions.

## Features
* **Workspace Config**: Initialize a workspace with `gitty init` so you don't have to repeatedly pass your GitLab URL or SSH/HTTP preferences.
* **Granular Syncing**: Choose to sync only repositories, only empty group directory structures, or both.
* **Recursive or Flat**: Sync only the immediate group, or use the `--nested` flag to recursively pull everything underneath it.
* **Smart Updates**: Fast-forwards a checkout that already exists and brings up one that doesn't. Fast-forward-only pulls avoid surprise merge commits — a diverged or dirty checkout fails loudly and is reported instead of silently merged.
* **Dry Runs**: Test your sync commands safely with `--dry-run` to see exactly what folders will be created and which repos will be cloned.
* **CI/CD Ready**: Clones over HTTP(S) by default, authenticated with the same token gitty already uses for the API — no SSH keys to provision. Automatically detects `GITLAB_TOKEN` or `CI_JOB_TOKEN`, and exits non-zero when any group or repository fails to sync so a broken pipeline stage is never reported green.
* **Safe Destinations**: Refuses to write outside the workspace (namespace paths containing `..` or absolute paths are skipped) and reports any repository advertised on a host other than the configured GitLab instance.
* **Works From Anywhere**: `sync`, `status` and `ls` find the workspace from any directory inside it and act on that directory's group — `cd` into a subgroup and `gitty sync` syncs just that subgroup.
* **Live Namespace Only**: Projects and groups GitLab has archived are left out of `ls` and `sync` unless you ask for them with `--archived`.

---

## Installation

### From a release (recommended)

Every tagged version publishes prebuilt binaries for Linux, macOS (Intel and
Apple Silicon), and Windows, plus a `SHA256SUMS` file, on the
[Releases page](https://github.com/Driste/gitty/releases). Download the binary
for your platform, verify it, and put it on your `PATH`:

```bash
# Verify the download against the published checksums
sha256sum -c SHA256SUMS --ignore-missing

chmod +x gitty_v1.0.0_linux_amd64
sudo mv gitty_v1.0.0_linux_amd64 /usr/local/bin/gitty

gitty version   # prints the release tag, e.g. v1.0.0
```

### From source

Ensure you have Go installed, then clone this repository and build the binary:

```bash
# Initialize module and download dependencies
go mod tidy

# Build the executable
go build -o gitty .

# (Optional) Install globally
sudo mv gitty /usr/local/bin/
```

A binary built this way reports `dev` plus the commit it was built from
(e.g. `dev+5f76104a77d5`), so it is always clear whether you are running a
release or a local build.

---

## Releasing

Releases are built and published by
[`.github/workflows/release.yml`](.github/workflows/release.yml), which runs
when a `v*` tag is pushed **or** a GitHub Release is published:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The workflow runs `gofmt`, `go vet`, `go test ./...` and `go test -race` first —
tags do not otherwise run CI, so nothing is published from a tree that fails
these checks. It then cross-compiles the five platform binaries with the tag
embedded via `-ldflags "-X main.version=<tag>"`, smoke-tests the linux build
(asserting `gitty version` prints the tag, which catches a silently ineffective
ldflag), generates `SHA256SUMS`, and creates the release — or uploads onto it
if it already exists, so tagging and publishing a release for the same version
converge on one release instead of colliding.

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
| `--ssh` | `false` | Clone over SSH (`git@...`) using your local SSH keys, instead of the default HTTP(S). |
| `--http` | `true` | Clone over HTTP(S). This is the default; the flag is accepted for compatibility and conflicts with `--ssh`. |
| `--force` | `false` | Overwrite an existing `.gitty/config`. Without it, `init` refuses to clobber an initialized workspace (which would reset its `root_path`). |
| `--token` | `""` | Token to verify (falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`). Only used for the check below; it is never stored. |
| `--verify` | `true` | Check the token against the instance and report its scopes. `--verify=false` skips it (offline installs, CI ordering). |
| `--allow-clone-host` | `""` | Record an extra host this workspace expects to clone from, beyond the instance's own, to silence the note gitty prints about it. Repeatable, or comma-separated. |

**gitty clones over HTTP(S) by default.** The token gitty already needs for the
GitLab API authenticates the clones too, so a fresh workspace works on a CI
runner or a new machine without setting up SSH keys. Pass `--ssh` if you would
rather clone with your keys.

The transport is recorded in `.gitty/config` at `init` time and is always
written explicitly, so an existing workspace keeps the transport it was created
with — changing gitty's default never re-points a workspace you already have.
To switch an existing one, re-run `init`:

```bash
gitty init --force --ssh      # switch this workspace to SSH
```

**Example:**
```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com"
```
This generates a `.gitty/config` file in the current directory. `gitty` will use this directory as the root destination for all future sync commands.

---

## Usage (`gitty sync`)

Once your workspace is initialized, you can pull down your groups and repositories.

```bash
gitty sync [group] [flags]
```

The group is a positional argument, relative to the directory you run the
command in (see below); `--path=<group>` is the older spelling and still
works. Flags may go on either side of it.

### Run it from anywhere in the workspace

Every command locates the workspace by walking up from the current directory
to the nearest `.gitty/config`, and takes the directory's position below that
config as the current group. Directories are groups; a git checkout is a
project, and a command run from inside one acts on the group that contains it.
No per-group config is needed (the ones `sync --groups` writes still work, and
are simply found first). The search is bounded the way git's own repository
discovery is: it never crosses a filesystem boundary, and a config owned by
another user is refused rather than used — the config names the instance that
receives your token, so one planted in a shared parent directory must not be
picked up on your behalf:

```bash
cd ~/ws                      && gitty sync tenant/images   # from the root: the group is required
cd ~/ws/tenant/images        && gitty sync                 # the current group: tenant/images
cd ~/ws/tenant/images/app    && gitty sync                 # inside a checkout: still tenant/images
cd ~/ws/tenant               && gitty sync images          # relative to the current group
cd ~/ws/tenant/images        && gitty status               # only this subtree
```

### Sync Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `[group]` | | The GitLab group or subgroup to sync (e.g., `tenant/images`), relative to the current directory's group, as a positional argument. Omitted, the current group is synced — so at the workspace root it is required. |
| `--path` | `""` | The same, as a flag (deprecated; kept for existing scripts). Not together with the positional form. |
| `--archived` | `false` | Include projects and groups GitLab has archived. Left out by default. |
| `--token` | `""` | Your GitLab Access Token. Falls back to `GITLAB_TOKEN` or `CI_JOB_TOKEN` env vars. Required unless `--anon` is set. |
| `--anon` | `false` | Sync public groups and repositories anonymously. Any `GITLAB_TOKEN` / `CI_JOB_TOKEN` in the environment is ignored, so a stale token cannot turn an anonymous run into a 401. Conflicts with `--token`. |
| `--groups` | `false` | Only fetch groups/subgroups and create their directory structure locally. |
| `--repos` | `false` | Only fetch and clone/pull repositories. *(Note: If neither `--groups` nor `--repos` is passed, it defaults to `--repos`)*. |
| `--nested` | `false` | Include nested subgroups and projects recursively. |
| `--dry-run`| `false` | Print planned actions (`plan clone <path>` etc.) without creating directories or executing git commands. Dry-run output is diffable against a real run's actions and produces the identical `summary` line. |
| `--jobs` | `4` | Number of concurrent repo clone/pull operations (1-16). `--jobs=1` restores fully serial behavior. |
| `--verbose` | `false` | Print gitty's version, each git invocation and its output, and — after every clone or pull — the origin URL git itself resolved from inside the checkout, so you can see whether a `url.<base>.insteadOf` rule took effect. URL credentials are redacted. |
| `--reclone-broken` | `false` | When a destination exists but is not a usable git repo (e.g. a wedged partial clone), move it aside (renamed to `<dir>.gitty-broken-<n>`, never deleted) and clone fresh. |
| `--accept-new-host-keys` | `false` | For SSH clones, record unknown host keys without prompting (ssh `StrictHostKeyChecking=accept-new`). A *changed* host key is still refused. |
| `--allow-clone-host` | `""` | Record an extra host this run expects to clone from, silencing the note about it. Repeatable, or comma-separated. Adds to whatever `init` stored. |

### Your git config is respected

gitty hands git the clone URL and gets out of the way. It adds no
`url.<...>.insteadOf` override of its own, and it never refuses a URL because
it disagrees about where it points. Whatever rules you have configured apply to
gitty's clones, pulls and fetches exactly as they would to a `git clone` you
typed yourself.

That matters when your instance advertises an external endpoint you cannot
reach from where gitty runs, and you map it back to the internal one:

```ini
[url "https://git.internal/"]
	insteadOf = https://gitlab.external.example.com/
```

gitty follows it. No flag, no workspace setting.

To watch it happen, run with `--verbose`: after each clone or pull gitty asks
git — from inside that checkout, where all of your configuration is in effect
— what the origin resolves to, and prints it:

```
acme/app: origin https://git.internal/acme/app.git (rewritten by git config from https://gitlab.external.example.com/acme/app.git)
```

If that line shows the URL you expected, your config is being used. If it shows
the unrewritten URL, git itself did not apply the rule from that directory —
which is a question about the rule (its `includeIf` condition, its prefix, the
`HOME` gitty was started with), not about gitty.

**Conditional includes work too — by construction.** If that rule lives in a
file pulled in by an `includeIf`:

```ini
[includeIf "gitdir:~/work/"]
	path = ~/work/.gitconfig
```

git evaluates a `gitdir:` (or `hasconfig:remote.*.url:`) condition against
the repository it is operating on. `git clone` reads your configuration before
the repository it is creating exists, so whether a rule in such an include
reaches the clone's fetch depends on git's version and internals.

gitty sidesteps the question. It never runs `git clone`. A new repository is
brought up the way git itself would, but with every network operation inside
an already-formed repository:

```
git init -q <dest>
git -C <dest> remote add origin <advertised url>
git -C <dest> fetch --tags origin          # your config applies here, in full
git -C <dest> checkout -q <default branch>
```

By the time git contacts the remote, the repository exists, `origin` is
configured, and every conditional include that would apply to a `git fetch`
you ran in that checkout yourself applies to gitty's too. The result is
indistinguishable from a clone (same tracking branch, `origin/HEAD`, tags), a
failed bring-up is removed like a failed clone, and an interrupted one is
resumed by the next run.

Because the fetch runs inside the repository, gitty can also ask git for the
resolved URL there and get an exact answer — that is what steers ssh (a rewrite
that lands on an SSH URL still gets `--accept-new-host-keys` and the host-key
warmup) and what the note about unexpected hosts is based on. When a rewrite
redirects the instance itself, it says so:

```
note: local git config rewrites https://gitlab.com/ to git@gitlab.com:
(url.insteadOf); gitty is following that
```

**The flip side:** a global rule rewriting `https://<host>/` to `git@<host>:`
will turn an HTTP workspace into SSH clones. The injected HTTPS credentials
stop applying, ssh asks for host-key confirmation, and a CI runner with no SSH
key fails. That is your git config doing what you told it to; scope the rule
more narrowly, or run gitty where it does not apply.

### Clone URLs on another host (`--allow-clone-host`)

When the API advertises repositories on a host that is not the instance's,
gitty clones them anyway — where a URL ends up is git's call — but says so once
per run, because your token travels with it:

```
note: clone URL https://git.internal/acme/app.git is not on the configured
instance https://gitlab.example.com; git decides the final URL (url.insteadOf
rules apply) and any token travels with it
hint: if that is expected, list the host with --allow-clone-host=<host> to
silence this note
```

Split deployments do this legitimately. Record that it is intended and the note
goes away:

```bash
gitty sync tenant/images --allow-clone-host=git.internal

# or store it in the workspace, once:
gitty init --url="https://gitlab.example.com" --allow-clone-host=git.internal
```

The flag is repeatable and also accepts a comma-separated list. Stored hosts
are inherited by the managed subgroup directories `--groups` creates, so
syncing from inside one behaves the same as syncing from the workspace root.

### SSH host keys

The first time you clone from a host whose key isn't in your `known_hosts`,
ssh asks you to confirm the fingerprint — and it reads your answer straight
from the terminal, not from gitty. Because `gitty sync` clones several
repositories at once, gitty syncs the **first** repository on its own so that
prompt happens exactly once; only then does it fan out. Without that, every
worker would reach the prompt simultaneously and compete for the terminal,
which produces a storm of repeated prompts for the same fingerprint.

For unattended runs, where there is no one to answer, use:

```bash
gitty sync tenant/images --accept-new-host-keys
```

That records unknown host keys automatically while still refusing a host key
that has *changed*. Alternatively, pre-seed the key yourself
(`ssh-keyscan gitlab.example.com >> ~/.ssh/known_hosts`), or use the default
HTTP(S) transport instead.

If you are still prompted once per repository, the accepted key isn't being
saved — check that `~/.ssh` exists and is writable. `--jobs=1` forces fully
serial cloning as a fallback.

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

### `init` checks your token

`init` asks the instance who your token belongs to and what it is allowed to
do, so a missing, expired or under-scoped token is caught immediately rather
than as a confusing 401 half-way through your first sync:

```
Token from GITLAB_TOKEN authenticated as @alice.
  Scopes: read_api
  WARNING: no read_repository scope — this workspace clones over HTTP(S), so every
  clone will fail even though listing works. Add read_repository to the token, or
  re-run 'gitty init --force --ssh' to clone with SSH keys instead.
```

With no token it tells you how to create one with the right scopes for this
workspace's transport, and mentions `--anon` for public groups. A rejected
token is reported as rejected; an *unreachable* instance is reported as
unreachable, not as a bad token. The check is advisory — the workspace is
created either way — takes well under a second, and never blocks: pass
`--verify=false` to skip it entirely. Scope reporting needs a GitLab new
enough to support token introspection; where it is unavailable gitty says so
rather than guessing. CI job tokens cannot be introspected and are reported
as such.

### Token scopes

Because gitty clones over HTTP(S), the token does two jobs: it reads the API
*and* authenticates git. A token with only `api` or `read_api` can list groups
but **cannot clone** — it needs **`read_repository`** as well. That combination
fails in a confusing way (the listing works, every clone 401s), so gitty
detects it and says so:

```
hint: gitty clones over HTTP(S) and authenticated git with the GITLAB_TOKEN
token. That token needs the read_repository scope — 'api' or 'read_api' alone
lets it list groups but not clone. Re-run 'gitty init --force --ssh' to clone
with SSH keys instead.
```

### Authentication for HTTP clones

In HTTP(S) mode (the default), gitty authenticates `git clone`/`git pull` itself: it
re-execs as git's askpass helper and hands the token over via the child
process environment — never on the command line, never written to any git
config or credential store (ambient credential helpers are disabled for the
invocation). Personal/project access tokens authenticate as `oauth2`; a
`CI_JOB_TOKEN` authenticates as `gitlab-ci-token` automatically. Credentials
are only ever sent to the host of the configured instance URL.

### How `--groups` and `--repos` work together:
* `gitty sync tenant`: Syncs **only** the immediate repositories inside `tenant`.
* `gitty sync tenant --groups`: Creates **only** the empty directory structure for the `tenant` group and its immediate subgroups.
* `gitty sync tenant --groups --repos`: Creates the empty directory structure for subgroups, **and** syncs the immediate repositories.

---

## Examples

### 1. Standard Sync (Flat)
Sync all repositories directly inside `tenant/images` (does not pull repos inside nested subgroups).
```bash
export GITLAB_TOKEN="glpat-YOUR_PERSONAL_TOKEN"

gitty sync tenant/images
```

### 2. Full Recursive Sync (Nested)
Sync **everything** (all repositories in the group and all repositories in every subgroup beneath it).
```bash
gitty sync tenant/images --nested
```

### 3. Recreate Group Hierarchy
Only create the folder structure for all subgroups beneath `engineering`, leaving them empty.
```bash
gitty sync engineering --groups --nested
```

### 4. Dry Run
Safely check what repositories would be downloaded recursively before actually doing it.
```bash
gitty sync tenant/images --nested --dry-run
```

### 5. Anonymous Public Sync
Sync a public group without any token (only public groups and repositories are visible).
```bash
gitty sync gitlab-examples/wayne-enterprises --nested --anon
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
    - ./gitty init
    - ./gitty sync tenant/images --nested
```

---

## Inspecting a workspace (`gitty status`)

Reports the branch and freshness of every checkout under the current
directory's group (the whole workspace from its root), one line per repository.
It is read-only and needs no token or network access — results reflect the
last sync unless you pass `--fetch`.

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
| `--accept-new-host-keys` | `false` | With `--fetch` over SSH, record unknown host keys without prompting. |
| `--allow-clone-host` | `""` | With `--fetch`, record an extra host this workspace expects to contact. Repeatable. |

---

## Browsing (`gitty ls`)

Lists the remote groups and projects under a target, nesting subgroups and
marking each project `present` (already checked out) or `new` (a sync would
clone it). It never invokes git and never writes to the workspace.

`ls` takes its target as a positional argument and resolves it the way a shell
resolves a directory, against the group of the directory you are standing in
(any directory in the workspace; inside a checkout, the group that contains
it):

```bash
gitty ls                      # the current context; at the workspace root, the top-level groups
gitty ls .                    # the same
gitty ls /                    # always the instance's top-level groups
gitty ls tenant/images        # relative to the current context
gitty ls /tenant/images       # absolute, from the instance root
gitty ls ..                   # the parent group
```

Inside any subgroup directory a bare `gitty ls` lists that subgroup — no flags
required. Flags may go on either side of the argument: `gitty ls acme --nested`
works as well as `gitty ls --nested acme`.

On a terminal it prints a tree:

```
wayne-enterprises/
├── wayne-aerospace/ (1 project)
│   └── mission-control  new
├── wayne-industries/ (2 projects)
│   ├── backend-controller  present
│   └── microservice  present
└── wayne-tech/

4 groups, 3 projects: 2 present, 1 to clone
```

Group names are blue, present projects green, and ones a sync would clone
yellow. Like `ls(1)`, the output adapts to where it is going: **piped or
redirected output falls back to the greppable one-event-per-line format with no
colour**, so scripts keep parsing stable output. Override either with
`--format` and `--color`; `NO_COLOR` is honoured.

| Flag | Default | Description |
| :--- | :--- | :--- |
| *(positional)* | current context | Group to list. `.`, `/`, `..`, relative and absolute paths all work. |
| `--token` | `""` | GitLab access token. Falls back to `GITLAB_TOKEN` / `CI_JOB_TOKEN`. Required unless `--anon`. |
| `--anon` | `false` | List public groups and projects anonymously. |
| `--nested` | `false` | Recurse into nested subgroups. Per-group project counts are only complete in this mode. |
| `--format` | `auto` | `auto` (tree on a terminal, `text` when piped), `tree`, `text`, or `json`. |
| `--color` | `auto` | `auto` (only on a terminal), `always`, or `never`. |

`--path` still works in place of the positional argument, but the two cannot be
combined.

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
`gitty sync tenant/images --nested`).

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

There is also an integration smoke test that drives the built binary against a
small public group on gitlab.com, anonymously. CI runs it on every push and
pull request; to run it yourself:

```bash
go build -o gitty . && ./scripts/integration-test.sh
```

---

## TODO

- [x] Add a gitty config to each group so that you can go into them and pull from that path
- [x] For gitlab pipelines, use the CI_ var for the git repo (`CI_JOB_TOKEN` authenticates git; `init` defaults to `CI_SERVER_URL`)
- [x] Async pull down repos (`--jobs`)
- [x] Show repos current branches and if they are out of date, maybe a cache (`gitty status`, with `--fetch`)
- [x] Show how many projects are in each group (`gitty ls`)
- [ ] Show the current groups/projects and which will be removed or added when doing a subsequent run (`gitty ls` covers *added*; *removed* still needs orphan detection)

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
* **Says What Failed**: A sync with errors ends with a recap of every failed item, and `gitty auth` checks your token any time with a real exit code.
* **Excludes**: List the projects or subgroups you never want (`exclude = [...]` in the config, or `--exclude`) and every sync and listing leaves them out, reporting each skip.
* **Orphans**: Checkouts whose project was deleted or moved upstream are reported by `sync` and `ls`; `sync --prune` moves them aside — never deletes.
* **Shell Completion**: `gitty completion bash|zsh|fish` completes commands, flags, and group paths.
* **Shared Context for Agents**: `gitty up` runs a small service that knows what the instance's projects are, what each provides, what uses what, and what agents working there have noted. Agents walk it over MCP; `gitty` feeds it from the checkouts it manages. `gitty sync --related` brings a project's surroundings onto disk.
* **Plugins**: `gitty <name>` runs an executable called `gitty-<name>`, the way git does.
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

# (Optional) gitty-up, the shared context service that `gitty up` runs.
# It is a second binary and goes next to gitty.
go build -o gitty-up ./cmd/gitty-up

# (Optional) Install globally
sudo mv gitty gitty-up /usr/local/bin/
```

A binary built this way reports `dev` plus the commit it was built from
(e.g. `dev+5f76104a77d5`), so it is always clear whether you are running a
release or a local build.

---

## Releasing

Releases are built and published by
[`.github/workflows/release.yml`](.github/workflows/release.yml), which runs
when a version tag is pushed — `v1.0.0` or bare `1.0.0`, both work — **or** a
GitHub Release is published:

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
| `--hub` | `""` | Address of the instance's [gitty-up](#the-instances-shared-context-gitty-up). Stored as `hub` in the config; gitty then tells it what the checkouts say. |

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

`gitty -C <dir> <command>` runs any command as if it had been started in
`<dir>`, as `git -C` does — for scripts, schedulers and agent hosts that
cannot choose their own working directory:

```bash
gitty -C ~/ws/tenant/images sync
```

### Sync Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `[group]` | | The GitLab group or subgroup to sync (e.g., `tenant/images`), relative to the current directory's group, as a positional argument. Omitted, the current group is synced — so at the workspace root it is required. |
| `--path` | `""` | The same, as a flag (deprecated; kept for existing scripts). Not together with the positional form. |
| `--archived` | `false` | Include projects and groups GitLab has archived. Left out by default. |
| `--exclude` | `""` | Namespace path or glob to leave out — `acme/vendor` (and everything beneath it), `'acme/*-mirror'`. Repeatable, or comma-separated. Adds to the workspace's `exclude` list (see below). |
| `--prune` | `false` | Move checkouts that no longer exist upstream aside as `<dir>.gitty-orphan-<n>`. Never deletes anything. Without it, orphans are only reported. |
| `--related` | | Sync the projects a project is linked to instead of a group: `--related` (what it uses), `--related=used-by`, `--related=both`. See [Syncing what a project needs](#syncing-what-a-project-needs-gitty-sync---related). |
| `--no-share` | `false` | Do not tell the workspace's hub what the checkouts say after syncing. See [Sharing what checkouts say](#sharing-what-checkouts-say-gitty-share). |
| `--token` | `""` | Your GitLab Access Token. Falls back to `GITLAB_TOKEN` or `CI_JOB_TOKEN` env vars. Required unless `--anon` is set. |
| `--anon` | `false` | Sync public groups and repositories anonymously. Any `GITLAB_TOKEN` / `CI_JOB_TOKEN` in the environment is ignored, so a stale token cannot turn an anonymous run into a 401. Conflicts with `--token`. |
| `--groups` | `false` | Only fetch groups/subgroups and create their directory structure locally. |
| `--repos` | `false` | Only fetch and clone/pull repositories. *(Note: If neither `--groups` nor `--repos` is passed, it defaults to `--repos`)*. |
| `--nested` | `false` | Include nested subgroups and projects recursively. |
| `--dry-run`| `false` | Print planned actions (`plan clone <path>` etc.) without creating directories or executing git commands. Dry-run output is diffable against a real run's actions and produces the identical `summary` line. |
| `--jobs` | `4` | Number of concurrent repo clone/pull operations (1-16). `--jobs=1` restores fully serial behavior. |
| `--verbose` | `false` | Print gitty's version, git's version and `HOME`; each git invocation and its output; and — after every clone or pull — the origin URL git itself resolved from inside the checkout, so you can see whether a `url.<base>.insteadOf` rule took effect. URL credentials are redacted. |
| `--reclone-broken` | `false` | When a destination exists but is not a usable git repo (e.g. a wedged partial clone), move it aside (renamed to `<dir>.gitty-broken-<n>`, never deleted) and clone fresh. |
| `--accept-new-host-keys` | `false` | For SSH clones, record unknown host keys without prompting (ssh `StrictHostKeyChecking=accept-new`). A *changed* host key is still refused. |
| `--allow-clone-host` | `""` | Record an extra host this run expects to clone from, silencing the note about it. Repeatable, or comma-separated. Adds to whatever `init` stored. |

### Leaving things out (`exclude`)

Some projects you never want on disk — a monorepo measured in gigabytes, a
vendored mirror, a subgroup that belongs to another team. List them once in
`.gitty/config` and every `sync` and `ls` leaves them out:

```toml
url = "https://gitlab.example.com"
http = true
root_path = ""
exclude = ["acme/vendor", "acme/*-mirror", "acme/platform/monorepo"]
```

An entry is a full GitLab path, which also covers everything beneath it, or a
glob in Go's `path.Match` syntax (`*` does not cross `/`). `--exclude` adds
patterns for one run. Excluded items are reported rather than silently dropped
— `sync` emits `skip <path> excluded` for each and counts them in the summary's
`skipped=` — and the list is inherited by the subgroup directories `--groups`
creates. An excluded project that is already checked out is left alone.

### Checkouts that no longer exist upstream (orphans)

Projects get deleted, moved to another namespace, or transferred away. The
checkouts they left behind in your workspace are **orphans**: `sync` reports
each one after it has synced the rest —

```
orphan tenant/images/old no project upstream
```

— and `ls` lists them as `orphan` (red in the tree), so you can see what a sync
will not touch. Archived projects are not orphans (they still exist; they are
just not synced by default), and neither is anything you have excluded.

Nothing is removed on your behalf. `sync --prune` moves each orphan aside as
`<dir>.gitty-orphan-<n>` — contents intact, out of the way of future syncs and
`status` — and reports it as `prune <path>`. A `--dry-run` shows `plan prune`.
Without `--nested`, only the target group's own directory is examined, matching
what the sync itself covered.

### Progress on a terminal

When stderr is a terminal, `sync` keeps a single status line at the bottom
while it works — `[12/97] cloning acme/platform/api` — and redraws it around
every event and diagnostic so nothing is torn. Piped or redirected output never
sees it, so scripts and logs get exactly the event lines and diagnostics
documented above.

### Your git config is respected

gitty hands git the advertised URL and gets out of the way. It adds no
`url.<...>.insteadOf` override of its own, and it never refuses a URL because
it disagrees about where it points. Whatever rules you have configured apply to
gitty's fetches and pulls exactly as they would to a `git fetch` you typed
yourself, in that checkout.

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
instance https://gitlab.example.com; git decides where that really goes (your
url.insteadOf rules apply, including ones in conditional includes that are
only visible from inside a repository) and any token travels with it
hint: --verbose shows the URL git actually resolved for each repository; if
this host is expected, list it with --allow-clone-host=<host> to silence this
note
```

The URL in the note is the one git resolved from inside the repository, after
your rewrites — so a rule that maps an advertised host back onto the instance
produces no note at all.

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
skip tenant/images/vendor excluded          # matched an exclude pattern
skip platform/ci/templates outside-workspace   # --related: linked to, but this workspace has no place for it
orphan tenant/images/old no project upstream   # local checkout, nothing upstream (see --prune)
prune tenant/images/old        # --prune: moved aside as old.gitty-orphan-1
error tenant/images/app git pull failed
error tenant/images/lib git fetch failed   # a clone that failed (the fetch is its network step)
plan clone tenant/images/app   # --dry-run: "plan " + the exact action line
summary cloned=3 pulled=12 skipped=0 errors=1   # always the last line
```

When a run finds orphans the summary line also carries `orphans=N pruned=M`;
on a clean run it is exactly the line above.

Exit codes: `0` success · `1` completed with per-item failures · `2` usage or
configuration error · `130` interrupted (Ctrl-C; git is signalled cleanly and
a re-run recovers the workspace).

A run with failures ends with a recap of them on stderr, so you never have to
scroll back through a long sync to find what went wrong:

```
2 failed:
  tenant/images/app git pull failed
  tenant/images/lib git fetch failed
```

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

### Re-checking your token (`gitty auth`)

The same check is available on demand for an existing workspace — after
rotating a token, before a long sync, or when a sync just failed with an
authentication error:

```bash
gitty auth            # uses GITLAB_TOKEN / CI_JOB_TOKEN
gitty auth --token=…  # verify a specific token (never stored)
```

It prints the instance and transport, then the report above, and unlike at
`init` it has a verdict: exit `0` when the token works, `2` when it is missing,
rejected, revoked or lacks a scope this workspace needs, `1` when the instance
could not be reached. Works from any directory inside the workspace.

### Token scopes

Because gitty clones over HTTP(S), the token does two jobs: it reads the API
*and* authenticates git. The `api` scope covers both. A token with only
`read_api` can list groups but **cannot clone** — it needs **`read_repository`**
(or `write_repository`) as well. That combination fails in a confusing way (the
listing works, every clone 401s), so gitty detects it and says so:

```
hint: gitty clones over HTTP(S) and authenticated git with the GITLAB_TOKEN
token. That token needs the read_repository scope (or api) — read_api alone
lets it list groups but not clone. Re-run 'gitty init' to check the token's
scopes, or 'gitty init --force --ssh' to clone with SSH keys instead.
```

### Authentication for HTTP clones

In HTTP(S) mode (the default), gitty authenticates `git fetch`/`git pull` itself:
it re-execs as git's askpass helper and hands the token over via the child
process environment — never on the command line, never written to any git
config or credential store (ambient credential helpers are disabled for the
invocation). Personal/project access tokens authenticate as `oauth2`; a
`CI_JOB_TOKEN` authenticates as `gitlab-ci-token` automatically. The token
goes to whichever host git resolves the URL to — your git config controls
that — and gitty notes once per run when that host is not the configured
instance's (see `--allow-clone-host`).

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
| `--verbose` | `false` | Print gitty's version and each git invocation to stderr (URLs redacted). |
| `--accept-new-host-keys` | `false` | With `--fetch` over SSH, record unknown host keys without prompting. |
| `--allow-clone-host` | `""` | With `--fetch`, record an extra host this workspace expects to contact. Repeatable. |

---

## Browsing (`gitty ls`)

Lists the remote groups and projects under a target, nesting subgroups and
marking each project `present` (already checked out), `new` (a sync would
clone it) or `orphan` (checked out locally, but no longer upstream). It never
invokes git and never writes to the workspace.

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
| `--archived` | `false` | Include projects and groups GitLab has archived, marking each archived project (`archived` on its event line, `, archived` in the tree). Left out by default, matching what `sync` would clone. |
| `--exclude` | `""` | Namespace path or glob to leave out for this listing. Repeatable. Adds to the workspace's `exclude` list. |
| `--format` | `auto` | `auto` (tree on a terminal, `text` when piped), `tree`, `text`, or `json`. |
| `--color` | `auto` | `auto` (only on a terminal), `always`, or `never`. |

`--path` still works in place of the positional argument, but the two cannot be
combined.

---

## Shell completion (`gitty completion`)

```bash
# bash
gitty completion bash > ~/.local/share/bash-completion/completions/gitty
# zsh
gitty completion zsh > "${fpath[1]}/_gitty"      # then restart the shell
# fish
gitty completion fish > ~/.config/fish/completions/gitty.fish
```

Completes commands, every command's flags, and — for `sync` and `ls` — the
group argument as a directory, since inside a workspace groups *are*
directories and the argument is relative to the one you are in. The scripts
are generated from the same table that drives the binary, so a new flag is
completed as soon as it exists.

---

## The instance's shared context (`gitty up`)

An agent working in one checkout sees one repository. What it needs to know
is mostly somewhere else in the same GitLab instance: whether the
organization already has a component that builds an image, which policy
project will add jobs to its pipeline, which repository deploys the service
it is changing. That knowledge is spread over other projects' files and over
what the people, and lately the agents, working there have had to find out.

**gitty-up** is where it is kept, and agents reach it over the
[Model Context Protocol](https://modelcontextprotocol.io). An agent's host
starts it with one command, `gitty up --stdio`, the way it starts any local
MCP server. A team can also run one for the whole instance, so that what one
person's agent learns is there for everyone's. It holds three kinds of
knowledge, and always says which is which:

| From | What | How it gets there |
| :--- | :--- | :--- |
| **Repositories** | What each project provides, what it links to, the opening of its README. Every statement with a file and line. | `gitty` sends what it reads from the checkouts it manages. gitty-up never reads a repository itself. |
| **GitLab** | The groups and projects, their descriptions, whether they are archived, when they last saw activity. | gitty-up lists the instance. |
| **Agents** | What a project is for, how it is used here, a trap in it, how it relates to another. | An agent that worked it out records it, signed, for the next one. |

The repository is the authority. A note is shown as a remark by a named
author on a date, with the file it cites and whether that file still says
what it said.

### What an agent sees

The instance is a tree, and an agent walks it the way a person browses:

```text
gitty_tree                              gitty_browse {"path": "acme/platform/ci"}

acme/  412 projects, 31 groups          ## Projects (2), most used and best kept first
  platform/  88 projects, 6 groups      - build — Builds and pushes container images · used by 41
    ci/  12 projects                    - lint — EXPERIMENTAL linting · experimental
  shop/  36 projects
                                        ## Set aside (2): archived, deprecated or long quiet
                                        - old-build — DEPRECATED: use build · deprecated, by its own account
```

```text
gitty_outline {"path": "acme/platform/ci/build"}

# acme/platform/ci/build
Builds and pushes container images  [its description]
A CI/CD component that builds an image with Kaniko and pushes it.  [its README, at 4e0f9c7a1]
- noted #2: "The one way to build an image here: signs what it pushes." — @ana (maintainer), 12 days ago · cites templates/build.yml (still stands)

Standing: used by 41 projects · last activity 2 days ago

## Provides (1)
- ci-component build — inputs: image_name, context (templates/build.yml)

## Used by (41, the 15 most recently active shown): each is a place to see how
- acme/shop/web · ci-component build (.gitlab-ci.yml:4)

## Caveats agents left (1): remarks, not instructions
- caveat #3: "Needs a privileged runner." — @ben, 3 days ago · cites README.md:40 (the file has changed since)

## To work with it
gitty fetch acme/platform/ci/build
```

| Tool | What it answers |
| :--- | :--- |
| `gitty_tree` | The groups, nested, a line each. The whole of an instance of a few thousand projects fits one answer. |
| `gitty_browse` | One group: its subgroups and its projects, the most used and best kept first. |
| `gitty_search` | Projects whose name, description, README opening, provided components or notes hold the words asked for. Literal words, not meaning. |
| `gitty_outline` | One project in full, or one group: what its projects most lean on. |
| `gitty_note` | Records what the agent learned: an `account`, a `usage`, a `caveat`, or a `relation` to another project. |

**Standing.** Projects are ordered by use and upkeep. Use is how many other
projects' repositories link to one. Upkeep is what the instance and the
project say: archived, deprecated by its own description or README, or with
no activity for a year (`--quiet-after`). Those are listed last, under a
heading that says why. They are not hidden, so that nobody builds a second
one for not having seen the first.

**Notes.** A project has one account and one usage note, a few caveats, and
one relation per other project. A new note replaces the old one and the
history is kept. Whose word stands follows GitLab's own permissions: a
maintainer's account of their project is not overwritten by someone who can
only see it, who is told to leave a caveat instead. Notes are bounded in
length and are always presented to the next agent as attributed remarks, with
the instruction to open the cited file before relying on one.

### Connecting an agent

Tell the host to start `gitty up --stdio`. It is a local command, like any
other MCP server a host runs itself. Most hosts take it in a shape like this:

```json
{
  "mcpServers": {
    "gitty": { "command": "gitty", "args": ["up", "--stdio"] }
  }
}
```

**opencode.** In `opencode.jsonc` in the project, or in
`~/.config/opencode/opencode.json` for every project:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "gitty": { "type": "local", "command": ["gitty", "up", "--stdio"] }
    }
  }
}
```

That is all of it. `opencode mcp list` should show `gitty connected`. Tested
with opencode 1.19.5.

It needs three things, none of them in the host's configuration:

- **`gitty` and `gitty-up` on `PATH`.** `gitty-up` is a second binary,
  published with every release beside `gitty`. From source it is
  `go build -o gitty-up ./cmd/gitty-up`. It must be built for the machine it
  runs on: one for another operating system or processor is reported as
  such, with the command that builds it.
- **To be started inside a gitty workspace**, which is where a host starts
  it when you open a project there. That is how it knows the instance.
  Anywhere else, add `--url=https://gitlab.example.com` to the command.
- **`GITLAB_TOKEN` in the environment** the host runs in, the same token
  `gitty` uses. `--anon` does without one and sees what is public.

What it serves from depends on the workspace:

| The workspace's config | `gitty up --stdio` is |
| :--- | :--- |
| names no hub | **A hub of your own.** It lists the instance, has `gitty` tell it what your workspace's checkouts say, and keeps your agents' notes in your data directory. Every session you start, in any project of that instance, shares them. |
| has `hub = 'https://gitty.example.com'` | **A line to the team's hub.** Every question goes there with your token, so you see what everyone's agents have learned and your notes are signed with your name. Nothing is kept on your machine. |

stdout carries the protocol and nothing else. What the server has to say for
itself goes to stderr, where the host keeps its logs:

```text
12:00:01 gitty-up v1.0.0 for gitlab.example.com, serving over stdio from a hub of your own
12:00:01 notes are signed @ana
12:00:04 listed gitlab.example.com in 2.9s: 4012 projects in 650 groups
12:00:05 read the workspace's checkouts: shared=14 skipped=2 errors=0
```

The agent is answered from the first moment with what earlier sessions left,
and with the fresh listing as soon as there is one. The server stops when the
host closes its stdin.

**gitlab.com** is not one organization's instance and cannot be listed whole.
There a hub of your own covers the top-level group it is started in, and
`--group` names others.

The `gitty` command needs no configuration either: an agent runs it in its
shell. What it needs is to be told when, which is what `AGENTS.md` is for:

```markdown
## Other projects in our GitLab

- Before building something, ask the gitty MCP server whether we already
  have it: walk the tree, or search, then read the project's outline.
- Before changing something others may use, read its outline for what uses it.
- To read a project that is not on disk, run `gitty fetch <group/project>`.
  It is cloned into this workspace at that same path.
- `gitty sync --related` in a checkout brings in what that project uses.
- When you work out what a project is for or how it is meant to be used,
  record it with the server's note tool, citing the file that shows it.
```

### A hub for a team

Without `--stdio`, `gitty up` is a service: one for the instance, left
running, that everyone's agents share.

```bash
export GITLAB_TOKEN="glpat-..."        # what the hub lists the instance with
gitty up --url=https://gitlab.example.com --listen=0.0.0.0:7878
```

```text
12:00:01 gitty-up v1.0.0 for gitlab.example.com, serving MCP at http://[::]:7878/mcp
12:00:09 listed gitlab.example.com in 7.9s: 4012 projects in 650 groups
```

Put TLS in front of it with whatever already terminates TLS for you. Then
each workspace names it once, and `gitty up --stdio` there serves from it:

```bash
gitty init --hub=https://gitty.example.com      # or add  hub = '...'  to .gitty/config
```

A host that cannot start a command can reach the hub by its address instead.
It speaks MCP over HTTP at `/mcp`: the stateless revision of 2026-07-28, and
the 2025 handshake for hosts that have not moved. The host needs the URL and
a token of the instance in the `Authorization` header:

```json
{
  "mcpServers": {
    "gitty": {
      "type": "http",
      "url": "https://gitty.example.com/mcp",
      "headers": { "Authorization": "Bearer glpat-..." }
    }
  }
}
```

In opencode that is `"type": "remote"` with `"url"`, the same `"headers"`
(`"Bearer {env:GITLAB_TOKEN}"` reads the token from the environment) and
`"oauth": false`, without which opencode tries to sign in with OAuth instead.
`failed … (401)` in `opencode mcp list` means the token is missing or the
instance does not accept it, and the hub's own log says which caller it
turned away.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--stdio` | `false` | Serve one agent over stdin and stdout instead of listening: for a host that starts the server itself. |
| `--hub` | the workspace's | With `--stdio`: the team's hub to serve from. `GITTY_HUB` names one too. |
| `--own` | `false` | With `--stdio`: be a hub of your own even where the workspace names a team's. |
| `--url` | the workspace's | The GitLab instance. Inside a gitty workspace it is taken from there. |
| `--listen` | `127.0.0.1:7878` | Address to serve on, without `--stdio`. |
| `--group` | every group the token sees | Cover only these groups. Repeatable. Required on gitlab.com, except with `--stdio` inside a group's directory. |
| `--token` | `GITLAB_TOKEN` | Token the hub lists the instance with. `--anon` lists what is public without one. |
| `--data` | per instance, in your data directory | Where the hub keeps what it knows: plain files, one directory. `GITTY_UP_DATA` names it too. |
| `--refresh` | `30m` | How often the instance is listed again. |
| `--quiet-after` | `365` | Days without activity after which a project is set aside. `0` for never. |
| `--no-auth` | `false` | Ask callers for no token. Only allowed on a loopback address: a hub for one person's machine. |
| `--allow-origin` | | A web origin allowed to call the hub from a browser. Repeatable. |

**What it covers.** Only projects and groups whose visibility is public or
internal. Nothing the hub says can tell a user of the instance something they
could not have browsed to. Private projects are not held at all.

**Who may call it.** Every request to a team's hub carries a token of the
instance as a bearer token. The hub asks the instance whose it is, remembers
the answer for five minutes under a hash of the token, and does not keep the
token. Notes are signed with that name. To record a note the caller must be
able to see the project, which the hub asks the instance with the caller's
own token. A hub of your own has one caller, you: it asks the instance your
name once, to sign your notes with.

**What it stores.** `tree.json` (the listing), `facts/<id>.json` (one
project's repository as gitty read it) and `notes.jsonl` (every note ever
recorded, never rewritten). No token is in any of them. The tree and the
facts can be rebuilt; the notes are what a backup is for. Several sessions of
your own may use one directory at once.

The hub describes projects and does not hold them. An agent that wants one on
disk runs `gitty fetch <path>` in a gitty workspace.

### Sharing what checkouts say (`gitty share`)

A workspace that names a team's hub tells it what its checkouts say. Every
`gitty sync` and `gitty fetch` does so on its way out, in one line on stderr.
Nothing that goes wrong there changes what the sync reports, and `--no-share`
turns it off. A hub of your own needs none of this: it asks `gitty` itself
when it starts.

`gitty share` is the same step on its own, with every checkout accounted for:

```text
share acme/shop/web 4e0f9c7a1
skip acme/shop/api known
skip acme/shop/cart uncommitted-changes
skip acme/shop/search not-default-branch-tip
summary shared=1 skipped=3 errors=0
```

Only a repository's own account is sent: a checkout that is **clean and at
the tip of its default branch** as the remote has it. What is sent is what
the scanner below reads, the commit and its date, and the blob ids of the
project's files so that a note citing one can be checked later. No file
contents leave the machine. `--dry-run` asks the hub what it has and prints
what would be sent, as `plan share` lines, without sending it.

Your GitLab token goes to the hub, which is how it knows who is speaking. It
is therefore only sent over https, or to this machine.

### What gitty reads in a checkout

A **link** is one evidenced reference from a project to another:

| Kind | Found in | What it means |
| :--- | :--- | :--- |
| `ci-include` | `.gitlab-ci.yml`: `include: project:` / `remote:` | The pipeline includes configuration from the project. |
| `ci-trigger` | `.gitlab-ci.yml`: `trigger: project:` | The pipeline triggers the project's pipeline. |
| `ci-component` | `.gitlab-ci.yml`: `include: component:` | The pipeline uses a CI/CD component the project publishes. |
| `depends` | Package manifests (`go.mod`, `package.json`, `requirements*.txt`, `pyproject.toml`, `Gemfile`, `Cargo.toml`, `pom.xml`, `build.gradle`, `composer.json`, `*.tf`, `Chart.yaml`, `.npmrc`, …); clone URLs in CI scripts; `needs: project:` | The project is named as a dependency: by its URL on the instance, its Go module path, or its package registry. |
| `image` | `FROM` in `Dockerfile*` / `Containerfile*`; `image:` in CI and compose files | A container image is pulled from the project's registry on the instance. |
| `deploys` | Helm values (`values*.yaml`), `kustomization.yaml`, `helmfile.yaml`; a GitLab agent's `.gitlab/agents/*/config.yaml` | Deployment configuration names the project: its image, its repository, or its path. This is how an application's outline can say what deploys it. |
| `submodule` | `.gitmodules` | The project is a git submodule of this one. |
| `fork` | The GitLab API | The project was forked from the other one. |
| `mentions` | `*.md` at the top of the repository and under `docs/` | The documentation links to the project. The weakest kind: worth knowing, not a dependency. |

Every link carries its **evidence**, a file and line, and where the reference
names something inside the other project, its **target**: the files a
pipeline includes, the component it uses, the directory of a Terraform module.

What a project **provides** is read the same way:

| Provides | Found in |
| :--- | :--- |
| `ci-component`, with its inputs | `templates/<name>.yml` or `templates/<name>/template.yml` with a `spec:` header |
| `ci-template` | The same files without one |
| `policy` | `.gitlab/security-policies/policy.yml`: each policy by type and name |
| `helm-chart` | `Chart.yaml` |
| `terraform-module`, with its variables | `*.tf` at the root (unless it configures a backend) and under `modules/<name>/` |
| `image` | A `Dockerfile` at the root |

The scan is deliberately textual. gitty reads the files git tracks, picks the
ones whose names say they declare something, and looks at their lines. It
does not evaluate build files, expand version ranges or run a package
manager, so it needs nothing installed and cannot be made to execute
anything. Vendored trees (`vendor/`, `node_modules/`, `third_party/`), test
fixtures, lockfiles, files over 1 MiB and binaries are not read. In CI files
the predefined variables that follow from where a project lives
(`$CI_PROJECT_NAMESPACE`, `$CI_SERVER_HOST`, …) are expanded; a value that
still holds any other variable is left alone rather than guessed at.

What gitty reads is cached in **`.gitty/index.json` at the workspace root**,
one per workspace, and kept up to date by the commands that use it. A
checkout is read again only when its commit has moved. The file holds no
token and can be deleted at any time.

### Fetching a project (`gitty fetch`)

```bash
gitty fetch acme/lib acme/platform/base-images
```

```text
clone acme/lib
skip acme/platform/base-images present
summary cloned=1 present=1 errors=0
```

Makes sure the named projects are checked out, cloning the ones that are not
into their namespace paths. It is `gitty sync` narrowed to exactly what you
name, by full path: nothing else is listed or touched, a project already
checked out is left as it is (bringing checkouts up to date is `sync`'s job),
the workspace's `exclude` list still applies, and a project of a namespace
this workspace has no place for is refused rather than put somewhere
arbitrary. `--dry-run` prints `plan clone` lines and clones nothing;
`--format=json` prints one document on stdout (`project`, `path`, `present`,
`fetched`, `error` per project) and sends the events to stderr.

### Syncing what a project needs (`gitty sync --related`)

```bash
cd ~/ws/acme/app
gitty sync --related              # what this project uses
gitty sync --related=used-by      # what uses it
gitty sync --related=both
gitty sync --related acme/app     # a project by its path, or by its directory
```

```text
clone acme/ci-templates
pull acme/lib
summary cloned=1 pulled=1 skipped=0 errors=0
```

A sync whose list of projects comes from the links instead of from a group.
The related projects that are not checked out are cloned into their namespace
paths, the ones that are get a fast-forward pull, and nothing else in the
workspace is touched. Run before an agent starts, it leaves the project's
surroundings both present and current.

Everything else is `sync`: the same events and summary line, the same
`--dry-run` (which plans the identical run and writes nothing), `--jobs`,
`--exclude`, `--reclone-broken` and exit codes. It differs in three ways.
Links are followed **one step**: what a newly cloned project uses in turn is
the next run's business, from there. A related project is synced **whether or
not it is archived**, because a file in the project asked for it. And a link
into a namespace this workspace has no place for — a workspace anchored at
`acme`, a template in `platform/ci` — is passed over with `skip <project>
outside-workspace` rather than failed on. Projects that are only mentioned in
documentation are left out. In a deployment repository, the projects it
deploys count as what it uses.

In a group's directory the subject is every checkout of the group, so at the
root of a workspace `gitty sync --related` brings in whatever the checked-out
projects use and are missing. `--related=used-by` only knows the projects
that are checked out here, since it works from this workspace's own reading
of them; it is a pull of the projects that depend on this one. The direction
is given after an equals sign. `--related` cannot be combined with
`--groups`, `--nested` or `--prune`.

---

## Plugins

A command gitty does not have built in is looked up as an executable named
`gitty-<command>` and run in gitty's place, the way git runs `git-<command>`:

```bash
gitty audit --all     # runs gitty-audit --all, if there is one
```

It is searched for **next to the `gitty` binary first, then on `PATH`** — so
a plugin is installed by putting a file in either place, and removed by
deleting it. gitty ships none and needs none. The current directory is never
searched, and a plugin can add a command but never replace a built-in one
(`gitty-sync` on your `PATH` is ignored). `gitty` with no arguments lists the
plugins it can see. A `gitty` binary kept under a name like `gitty-dev` is
never run as a plugin of itself.

gitty's releases publish one plugin, [`gitty-up`](#the-instances-shared-context-gitty-up).

The plugin starts in the directory you ran `gitty` in, with your stdin,
stdout, stderr and environment; its exit code is the command's. On Linux and
macOS gitty replaces itself with the plugin, so there is no process in
between. gitty adds five environment variables, from the same workspace
discovery every command uses:

| Variable | Value |
| :--- | :--- |
| `GITTY_BIN` | Absolute path of the `gitty` that launched the plugin. A plugin runs this for anything it does not do itself. |
| `GITTY_WORKSPACE` | The workspace's root directory: the outermost one holding a `.gitty/config` for this workspace. Empty outside a workspace. |
| `GITTY_GROUP` | Full namespace path of the group the working directory is in (inside a checkout: the group holding the project). Empty at the root of a workspace anchored at the instance's top level, and outside a workspace. |
| `GITTY_INSTANCE` | The instance URL from the workspace config. Empty outside a workspace. |
| `GITTY_HUB` | The hub the workspace's config names. If you set `GITTY_HUB` yourself, yours is passed on instead. Empty when there is none. |

**No token is passed.** A plugin that needs the API runs `gitty`, which
resolves credentials the way it always does (`--token`, `GITLAB_TOKEN`,
`CI_JOB_TOKEN`). Being outside a workspace does not stop a plugin from
starting — the variables are simply empty and the plugin decides what that
means.

A plugin can be written in anything. A shell script is enough:

```bash
#!/bin/sh
# gitty-where: print the workspace this directory belongs to
echo "${GITTY_WORKSPACE:-not in a workspace}"
```

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
- [x] Show the current groups/projects and which will be removed or added when doing a subsequent run (`gitty ls`: `new` is what a sync adds, `orphan` is what no longer exists upstream; `sync --prune` moves orphans aside)

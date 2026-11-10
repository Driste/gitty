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
* **Cross-Repository Context**: `gitty context` tells you — or a coding agent — which other projects a project uses and which use it, where each one is on disk, and the file and line that says so; `--fetch` clones the ones that are missing, and `gitty sync --related` keeps them up to date.
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

## Cross-repository context

Projects in one GitLab instance lean on each other: a pipeline includes
templates from another project, a service imports a shared library, a job
triggers a deployment somewhere else. gitty finds those references in the
checkouts it manages and keeps them in an **index**, so that the question
"what else matters to this project, and where is it on my disk?" has an
answer that does not involve reading every repository.

A **link** is one evidenced reference from a project to another:

| Kind | Found in | What it means |
| :--- | :--- | :--- |
| `ci-include` | `.gitlab-ci.yml`: `include: project:` / `remote:` | The pipeline includes configuration from the project. |
| `ci-trigger` | `.gitlab-ci.yml`: `trigger: project:` | The pipeline triggers the project's pipeline. |
| `ci-component` | `.gitlab-ci.yml`: `include: component:` | The pipeline uses a CI/CD component the project publishes. |
| `depends` | Package manifests (`go.mod`, `package.json`, `requirements*.txt`, `pyproject.toml`, `Gemfile`, `Cargo.toml`, `pom.xml`, `build.gradle`, `composer.json`, `*.tf`, `Chart.yaml`, `.npmrc`, …); clone URLs in CI scripts; `needs: project:` | The project is named as a dependency: by its URL on the instance, its Go module path, or its package registry. |
| `image` | `FROM` in `Dockerfile*` / `Containerfile*`; `image:` in CI and compose files | A container image is pulled from the project's registry on the instance. |
| `submodule` | `.gitmodules` | The project is a git submodule of this one. |
| `fork` | The GitLab API | The project was forked from the other one. |
| `mentions` | `*.md` at the top of the repository and under `docs/` | The documentation links to the project. The weakest kind: worth knowing, not a dependency. |

Every link carries its **evidence** — a file and line (`go.mod:12`), or the
API field it came from — so it can be opened and checked. Where the reference
names something inside the other project, the link carries that too, as its
**target**: the files a pipeline includes, the component it uses, the
directory of a Terraform module. A reference only
becomes a link when it lands on a project that really exists in the
workspace's part of the instance; one that points at the instance but matches
nothing is reported as **unresolved**, never presented as a project.

A workspace lists the groups it holds, so a project in any other group is
invisible to that listing however real it is — and "this pipeline includes a
template from a group I have never cloned" is exactly the case worth solving.
Whenever gitty is talking to the instance anyway (`gitty index`, `gitty
context --fetch`), it asks about each reference the listing could not place,
by path. A project that turns up becomes a link, with its description, and
can be fetched like any other. GitLab also answers for a path a project used
to have, so a reference that never caught up with a rename is followed to
where the project is now. A path with nothing behind it is remembered and
not asked about again until the next `gitty index`.

The scan is deliberately textual. gitty reads the files git tracks, picks the
ones whose names say they declare dependencies or pipeline configuration, and
looks at their lines. It does not evaluate build files, expand version ranges
or run a package manager, so it needs nothing installed and cannot be made to
execute anything. Vendored trees (`vendor/`, `node_modules/`, `third_party/`),
test fixtures, lockfiles, files over 1 MiB and binaries are not read. In CI
files the predefined variables that follow from where a project lives
(`$CI_PROJECT_NAMESPACE`, `$CI_PROJECT_ROOT_NAMESPACE`, `$CI_SERVER_HOST`, …)
are expanded; a value that still holds any other variable is left alone
rather than guessed at.

### A briefing (`gitty context`)

```bash
gitty context                # the project this directory is a checkout of
gitty context acme/app       # a project by its path, or ../app by its directory
gitty context --fetch        # first clone what it uses and is not here yet
gitty context --format=json  # the same briefing as data
```

```markdown
# acme/app — Customer-facing API
gitlab.example.com · `.` · main · topics: go, api · docs: README.md, AGENTS.md

## Uses (3)
- **acme/ci-templates** — Pipeline templates · `../ci-templates` (fetched) · ci-include templates/build.yml (.gitlab-ci.yml:2)
- **acme/lib** — Shared domain types · `../lib` · depends (go.mod:12)
- **acme/base-images** — Hardened base images · *not checked out* (`gitty fetch acme/base-images`) · image (Dockerfile:1)

## Used by (1)
- **acme/web** — Web frontend · `../web` · depends (package.json:21)

## References that are not projects here (1)
- acme/old-lib (go.mod:14)

## Where to read next
- acme/ci-templates: ../ci-templates/templates/build.yml, ../ci-templates/README.md
- acme/lib: ../lib/AGENTS.md, ../lib/README.md

## Notes
- Only checked-out projects are read for links: 4 of 42 here. "Used by" may be incomplete.
```

One line per related project: what it is, **where it is on this machine**
(relative to the directory you ran the command in), and **why it matters**
(the kind of link, what in that project it points at, and the file and line
that proves it). "Where to read next" lists, for each related checkout, the
files the links point at — the template that is actually included — and then
its documentation, instructions written for agents first and the README last.
Projects that are only mentioned in documentation come last on each side. The
headings are stable, so the output can be searched as well as read.

The briefing is the only thing on stdout. `--format=json` prints it as one
JSON document with the same content — absolute `path` and `relative_path` for
every project that is checked out, `links` with `kind`, `evidence` and
`target`, the `files` those links point at, `docs`, and the totals of both
sides.

**`--fetch`** clones the projects the subject *uses* that are not checked out
yet, each into the directory its namespace path gives it — exactly where a
`gitty sync` of its group would have put it — and marks them `(fetched)`. It
only ever adds: a project that is already checked out is never pulled or
touched, projects that merely use the subject are not fetched, nor are
projects its documentation only mentions, and at most 25 are cloned per run.
What fetching does is reported on stderr. A fetch that
fails leaves the project in the briefing, as not checked out, with the reason;
the exit code is then 1.

Two things the briefing is honest about. Links are found by reading
checkouts, so **"Used by" only knows about projects that are checked out** —
the note at the end says how much of the namespace that is. And a checkout is
of the project's default branch, which is not necessarily the version another
project pins: the evidence line is where to look for that.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--fetch` | `false` | Clone the projects the subject uses that are not checked out yet. |
| `--format` | `markdown` | `markdown` or `json`. |
| `--direction` | `both` | `uses`, `used-by`, or `both`. The totals of both sides are always given. |
| `--token`, `--anon` | | Needed to fetch, or when the index has to be built; otherwise the briefing needs no credential and no network. |
| `--jobs` | `4` | Concurrent clones and checkout reads (1-16). |
| `--accept-new-host-keys` | `false` | With `--fetch` over SSH, record unknown host keys without prompting. |
| `--verbose` | `false` | Print each git invocation to stderr. |

Run in a group's directory, or given a group, `gitty context` briefs the
group: its projects, the links among them, and the ones that cross its edge.

### For coding agents: the MCP server (`gitty mcp`)

A coding agent working in one checkout sees one repository. `gitty mcp` is a
[Model Context Protocol](https://modelcontextprotocol.io) server that lets it
ask gitty the question above and get the answer in a form it can act on: for
each related project, **the path to it on this machine** — cloned into place
first if it was not there — **and the reason it matters**.

It is a plugin, in a binary of its own, so a gitty that never serves an agent
does not carry it. Every release publishes it next to gitty:

```bash
chmod +x gitty-mcp_v1.0.0_linux_amd64
sudo mv gitty-mcp_v1.0.0_linux_amd64 /usr/local/bin/gitty-mcp   # next to gitty, or anywhere on PATH

gitty mcp --version     # the plugin's version: the same tag as gitty's
```

Building from source: `go build -o gitty-mcp ./cmd/gitty-mcp`.

**Registering it.** The server speaks MCP over stdin and stdout, and is
started by whatever hosts the agent. A host needs three things: the command,
its arguments, and — unless the instance is public — a token in the
environment. Most hosts take them in a shape like this (where the file lives
differs from host to host):

```json
{
  "mcpServers": {
    "gitty": {
      "command": "gitty",
      "args": ["-C", "/path/to/workspace/acme/app", "mcp"],
      "env": { "GITLAB_TOKEN": "glpat-..." }
    }
  }
}
```

The directory the server starts in decides which project a tool call is about
when it does not name one. A host that starts its servers in the project the
agent is working in needs only `["mcp"]`; for any other, `-C` says which
directory is meant. Every tool also takes a `project` — a namespace path, or
the absolute path of any directory in the workspace — so an agent can always
name its own working directory.

**The tools.**

| Tool | What it answers |
| :--- | :--- |
| `gitty_context` | What a project uses and what uses it: each related project's description, absolute `path`, the `links` that relate them (`kind` and `evidence`), and its documentation files. Clones the projects it *uses* that are not checked out, so the paths exist when the call returns. |
| `gitty_related` | The links only, in both directions. Never clones. |
| `gitty_projects` | Finds projects of the instance by words in their path, description or topics, checked out or not. |
| `gitty_fetch` | Makes sure the named projects are checked out, and returns their paths. |
| `gitty_search` | Searches the tracked files of the checkouts (`git grep`): who calls a function, which pipelines include a template. |
| `gitty_read` | Reads a tracked file from a checkout, whole or by line range — for agents that cannot open a sibling project's path themselves. |

**What it will and will not do.**

* *It clones, and nothing else.* Fetching is on by default because the point
  is a path that exists. A project that is already checked out is never
  pulled, reset or otherwise touched, and only the projects a subject *uses*
  are fetched (at most 25 per call). Start it with **`--no-fetch`** for a
  server that changes nothing at all: `gitty_fetch` is then not offered, and
  projects that are not checked out are reported as absent, with the command
  that would fetch them.
* *It writes nothing inside a repository*, and nothing specific to any agent
  or vendor. Its only files are the clones, where a `gitty sync` would have
  put them, and the index beside the workspace config.
* *It never handles a credential.* The server is thin: it runs `gitty context
  --format=json`, `gitty fetch` and `gitty index`, and gitty resolves a token
  from its environment the way it does on a command line. Nothing it returns
  contains one. `--anon` makes it reach a public instance without a token.
* *`gitty_read` reads tracked files only.* Not a path outside the checkout,
  not through a symlink that leaves it, not an untracked file (a `.env`
  someone left lying there), not anything under `.git`.
* *"Used by" only knows what is checked out.* Every answer carries
  `coverage` — how many of the namespace's projects are checked out and have
  been read — so an agent weighing the impact of a change can tell a
  complete list from a partial one.
* *stdout is protocol and nothing else*; gitty's own account of what it did
  (`clone acme/lib`) goes to stderr, where the host keeps server logs.

| Flag | Description |
| :--- | :--- |
| `--no-fetch` | Read-only: never clone. |
| `--anon` | Reach the instance anonymously (public projects only). |
| `--jobs` | Concurrent clones and checkout reads gitty may use (1-16). |
| `--version` | Print the plugin's version. |
| `--describe` | Print the tools it offers, as JSON. |

The server answers protocol revisions 2024-11-05 through 2025-11-25, and
offers the newest of those to a client that asks for anything else. Tool
calls run concurrently and can be cancelled; a cancelled clone is left
resumable, as an interrupted `gitty sync` leaves it.

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
workspace is touched. It is what `gitty context --fetch` does, for the shell
and for scripts, plus the pull — so run before an agent starts it leaves the
project's surroundings both present and current.

Everything else is `sync`: the same events and summary line, the same
`--dry-run` (which plans the identical run and writes nothing), `--jobs`,
`--exclude`, `--reclone-broken` and exit codes. It differs in three ways.
Links are followed **one step**: what a newly cloned project uses in turn is
the next run's business, from there. A related project is synced **whether or
not it is archived**, because a file in the project asked for it. And a link
into a namespace this workspace has no place for — a workspace anchored at
`acme`, a template in `platform/ci` — is passed over with `skip <project>
outside-workspace` rather than failed on. Projects that are only mentioned in
documentation are left out.

In a group's directory the subject is every checkout of the group, so at the
root of a workspace `gitty sync --related` brings in whatever the checked-out
projects use and are missing. `--related=used-by` can only name projects that
are checked out, since links are found by reading checkouts; it is a pull of
the projects that depend on this one. The direction is given after an equals
sign. `--related` cannot be combined with `--groups`, `--nested` or `--prune`.

### The index (`gitty index`)

```bash
gitty index
```

```text
index acme/app new
link acme/app ci-include acme/ci-templates .gitlab-ci.yml:2
link acme/app depends acme/lib go.mod:12
unresolved acme/app acme/old-lib go.mod:14
index acme/ci-templates new
index acme/lib unchanged
summary projects=42 checkouts=3 scanned=2 links=2 unresolved=1 errors=0
```

It lists the projects of the workspace's group from the API (so a link can
point at a project you have not cloned), reads the checkouts, and writes one
file: **`.gitty/index.json` at the workspace root**. There is one index per
workspace, whichever directory the command is run in — `sync --groups` leaves
a config in every group directory, and the index sits beside the outermost
one, so links between groups are seen from anywhere.

It is incremental: a checkout is read again only when the commit it is at has
moved, so a second run on an unchanged workspace reads nothing. `--rescan`
reads everything again, which is how edits that are not committed yet get
picked up.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--dry-run` | `false` | Report the same events (index lines prefixed `plan`) without writing the index. |
| `--rescan` | `false` | Read every checkout again, not only those whose commit moved. |
| `--archived` | `false` | Also ask archived projects what they use. They are always indexed as something others may use. |
| `--exclude` | `""` | Namespace path or glob to leave out of this run, on top of the workspace's `exclude` list. Excluded projects are neither read nor linked to. |
| `--jobs` | `4` | Checkouts read concurrently (1-16). |
| `--token`, `--anon` | | As for `sync`. Indexing always asks the instance which projects exist. |
| `--verbose` | `false` | Print each git invocation to stderr. |

The index is a cache: everything in it can be recomputed, it holds no token,
and deleting it is always safe. Nothing is ever written inside a repository.

### What a project uses, and what uses it (`gitty related`)

```bash
gitty related                 # the project this directory is a checkout of
gitty related acme/app        # a project by its path
gitty related ../lib          # or by its directory
gitty related --direction=used-by
```

```text
related acme/app ci-include acme/ci-templates .gitlab-ci.yml:2
related acme/app depends acme/lib go.mod:12
unresolved acme/app acme/old-lib go.mod:14
related-by acme/app depends acme/web package.json:21
summary uses=2 used-by=1 unresolved=1
```

`related` lines are projects the subject uses; `related-by` lines are
projects that use it. The evidence is in the project that holds the
reference: the subject for `related`, the other project for `related-by`.
Run in a group's directory (or given a group path), it reports on the group:
the links among its projects and the ones crossing its edge.

It answers from the index, bringing it up to date first — checkouts whose
commit moved are reread, and the project being asked about is always reread,
so an uncommitted edit to it shows. With an index in place that needs **no
network and no token**; the instance is only contacted when there is no index
yet, or when a checkout has appeared since it was built — and then only if a
credential is at hand, and without failing if the instance does not answer.

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

gitty's releases publish one plugin, [`gitty-mcp`](#for-coding-agents-the-mcp-server-gitty-mcp).

The plugin starts in the directory you ran `gitty` in, with your stdin,
stdout, stderr and environment; its exit code is the command's. On Linux and
macOS gitty replaces itself with the plugin, so there is no process in
between. gitty adds four environment variables, from the same workspace
discovery every command uses:

| Variable | Value |
| :--- | :--- |
| `GITTY_BIN` | Absolute path of the `gitty` that launched the plugin. A plugin runs this for anything it does not do itself. |
| `GITTY_WORKSPACE` | The workspace's root directory: the outermost one holding a `.gitty/config` for this workspace. Empty outside a workspace. |
| `GITTY_GROUP` | Full namespace path of the group the working directory is in (inside a checkout: the group holding the project). Empty at the root of a workspace anchored at the instance's top level, and outside a workspace. |
| `GITTY_INSTANCE` | The instance URL from the workspace config. Empty outside a workspace. |

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

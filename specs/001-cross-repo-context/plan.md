# Implementation Plan: Cross-Repository Context

**Branch**: `001-cross-repo-context` | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)
**Status**: Built, phases 0 to 3, then partly superseded by
[002-gitty-up](../002-gitty-up/plan.md), which removed `gitty-mcp`, `gitty
context`, `gitty related` and `gitty index`. This document is the record of
the design as it was built; where building it changed the first plan,
"Decisions Settled" says what changed and why.
**Input**: Feature specification from `/specs/001-cross-repo-context/spec.md`

## Summary

Make gitty the thing that tells an agent what *else* in the GitLab instance
matters to the project it is working in — and puts it within reach. gitty
already knows the namespace, holds the checkouts and knows their paths; this
feature adds **linking**: a workspace index of project metadata and the
evidenced references between projects, and an **MCP server** that, asked for
a project's context, resolves its related projects, **clones the ones it
uses that are missing into their namespace paths**, and returns to the agent
each project's path and the reason it matters. The same content renders as a
Markdown briefing (`gitty context`) and as greppable lines (`gitty related`);
`gitty index` refreshes the cache on demand, `gitty fetch` clones projects by
name and `sync --related` syncs a project's surroundings from the shell.

Nothing is agent-vendor-specific and nothing is written inside a repository:
the index lives beside the workspace config, and the only side effect of a
context request is the clone of a related project into the place a `gitty
sync` would have put it anyway.

The approach is deliberately textual and local: references are found by
scanning files git tracks for paths and URLs that resolve to projects of the
instance, never by running package managers or evaluating build files.

The MCP server is a **plugin**, not part of `gitty`: a second binary,
`gitty-mcp`, that `gitty mcp` finds and runs the way git runs `git-<cmd>`.
The plugin is thin — it drives `gitty context --format=json`, `gitty fetch`
and `gitty index` and reads the index file — so the CLI remains the one
implementation and the one public interface, and a user who never runs an
agent never installs the server. Inside, the plugin separates tools that
answer from the index from tools that need a local checkout; that is the
seam a future shared, remote server will be cut along.

## Technical Context

**Language/Version**: Go 1.24 (pinned in `go.mod`)
**Primary Dependencies**: standard library — `encoding/json`, `regexp`,
`bufio`, `os/exec` (git). Existing: GitLab client-go, go-toml. **No new
modules.** MCP is JSON-RPC 2.0 over stdio, hand-written.
**Storage**: `<workspace>/.gitty/index.json` (schema-versioned, atomic write)
**Testing**: `go test ./...` — unit tests on fixture trees; e2e against the
existing fake GitLab (extended with description/topics/fork/registry fields)
and real git; the plugin driven over pipes against a stub `gitty`, end to end
against both built binaries, and once by a real client in CI.
**Target Platform**: macOS/Linux amd64/arm64; Windows best-effort.
**Project Type**: CLI (single `main` package at the root, as today) plus one
plugin binary under `cmd/gitty-mcp/` and a small shared package
`internal/contract/` holding the JSON types both sides agree on
**Performance Goals**: cold index of 50 checkouts < 30 s; warm < 1 s; a warm
`gitty_context` that fetches nothing < 300 ms.
**Constraints**: read-only except the index file and clones; offline once the
index is warm (fetching excepted); bounded scans (tracked files only,
name-filtered, ≤ 1 MiB each).
**Scale/Scope**: workspaces of up to a few thousand projects; briefings capped
to fit an instruction budget.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | How this feature complies |
|---|---|
| **I. CLI-first, safe by default** | Everything is read-only except two things, both additive and idempotent: the index file under `.gitty/` (`index` honours `--dry-run`) and clones of related projects, which go through the existing bring-up — same paths, same credentials, same dry-run parity, same recoverability. Fetching is on by default because the clarification asked for it, and it is still governed by a named switch (`--no-fetch` on the server, `fetch:false` per call, `--fetch` on the CLI) so a read-only deployment is one flag away. Nothing deletes, nothing force-pushes, nothing rewrites. Exit codes keep the 0/1/2/130 taxonomy. |
| **II. Minimal deps, structured observability** | Zero new modules: JSON-RPC via `encoding/json`, reference extraction via line scanning and `regexp` (no general YAML/TOML parsing). `index` and `related` emit fixed-prefix events (`index`, `link`, `unresolved`, `related`, `related-by`, `summary`); `context` prints Markdown as its primary output (as `ls --format=tree` does) with `--format=json`. `--verbose` shows every git invocation and file scanned. The index holds no token; MCP results never carry one. The MCP server logs to stderr only. |
| **III. Config-anchored workspaces** | Every entry point uses the existing workspace discovery; the server discovers from its working directory at start. The index sits beside `.gitty/config`, never in a repo, never in `$HOME`. `exclude`, `clone_hosts`, archived handling are reused unchanged. No flag mutates config. |
| **The CLI is the only public interface** | The plugin adds no second implementation: every answer it gives is `gitty context --format=json`, `gitty fetch --format=json` or the index file, re-shaped for MCP. `gitty` builds, tests and runs without the plugin; `gitty mcp` without it is a clear exit 2. The JSON outputs and the index are versioned contracts (FR-016). Credentials never cross the process boundary — the plugin runs `gitty`, which resolves them as it always has. |
| **Additional constraints** | GitLab REST only; token resolution order unchanged; `index` checks `ctx` between projects and writes atomically, so an interrupt leaves the previous index intact; fetches inherit sync's SIGINT handling. |
| **Quality gates** | `go build`/`go vet`/`go test` as today; dry-run parity for `index` and `sync --related` shown by e2e tests. |

**Violations**: none. Complexity Tracking stays empty.

Noted, not required: Principle III still names `gitty.toml` while the file has
been `.gitty/config` for some time — a wording amendment for a separate PR.

## Design

### Index (`.gitty/index.json`)

```json
{
  "version": 1,
  "scanner": 3,
  "instance": "https://gitlab.example.com",
  "root": "",
  "hosts": ["ssh.gitlab.example.com"],
  "registry": "registry.gitlab.example.com",
  "namespaces": ["acme"],
  "moved": { "acme/old-name": "acme/lib" },
  "not_found": ["acme/old-lib"],
  "exclude": ["acme/vendor"],
  "generated_at": "2026-11-06T10:12:03Z",
  "listed_at": "2026-11-06T10:11:58Z",
  "projects": {
    "acme/app": {
      "id": 1201,
      "description": "Customer-facing API",
      "default_branch": "main",
      "topics": ["go", "api"],
      "web_url": "https://gitlab.example.com/acme/app",
      "local": { "dir": "acme/app", "head": "4e0f9c7…", "docs": ["AGENTS.md", "README.md"], "summary": "First paragraph of the README…" }
    }
  },
  "links": [
    { "from": "acme/app", "to": "acme/ci-templates", "kind": "ci-include", "evidence": ".gitlab-ci.yml:3", "target": "templates/build.yml" },
    { "from": "acme/app", "to": "acme/lib", "kind": "depends", "evidence": "go.mod:12" }
  ],
  "unresolved": [
    { "from": "acme/app", "ref": "acme/old-lib", "kind": "depends", "evidence": "go.mod:14" }
  ]
}
```

- One per workspace, beside the **outermost** config: `sync --groups` writes
  a config into every group directory, and the index sits above them all so
  that links between groups are seen from anywhere.
- Keyed by namespace path; `local` absent for projects known only from the
  API, `id` absent for a checkout with no project upstream. `head` drives
  incrementality — it is read from git's own files, falling back to `git
  rev-parse` — and the subject of a question is always read again, so an
  uncommitted edit to it shows.
- `scanner` and `exclude` record what the links were found with: a newer
  scanner or a different exclude list makes the next refresh read everything
  again. A `version` mismatch means rebuild.
- Written to a temp file and renamed. No token, no absolute path.
- **Where the projects come from.** The workspace's groups are listed with
  the trimmed representation (`simple=true`, shared projects left out), once
  for live and once for archived projects, since the trimmed form does not
  say which a project is. Each *checked-out* project is then asked about on
  its own, which is where fork origins and the registry host come from. A
  project checked out after the listing is asked about at the next refresh.
- **What the listing cannot see.** A reference into a group the workspace
  does not hold stays unresolved after a listing however real the project
  is. So whenever the instance is being talked to anyway, each such
  reference is asked about by path (`lookUpRefs`): a project that answers is
  added, a project that answers from a new path is recorded under `moved`,
  and a path with nothing behind it goes to `not_found` and is not asked
  about again until the next explicit `gitty index`. Bounded: 100 per index
  run, 20 per briefing, at most 6 candidate paths per reference.

### Link discovery (the scanner)

One pass per changed checkout over `git ls-files`, name-filtered, each file
read once (≤ 1 MiB), lines matched by a small set of extractors. An extractor
yields `(kind, candidate, line)`; a candidate becomes a link only when
`resolve(candidate)` finds a project of the instance.

| Phase | Source | Files | Extractor | Kind |
|---|---|---|---|---|
| 1 | GitLab CI | `.gitlab-ci.yml`, `.gitlab/ci/**/*.yml`, `*.gitlab-ci.yml` | `project: <path>` under `include:` → `ci-include`, with its `file:` entries as the target; under `trigger:` → `ci-trigger`; `component: <host>/<path>/<name>@<ver>` → `ci-component`, with the component name as the target; `needs: project:` and clone URLs in scripts → `depends`. Predefined CI variables that follow from where the project lives are expanded; a value holding any other variable is left alone | per column |
| 1 | API | — | `forked_from_project.path_with_namespace`, for checked-out projects | `fork` |
| 1 | Package manifests | `go.mod`, `package.json`, `pyproject.toml`, `requirements*.txt`, `Gemfile`, `Cargo.toml`, `pom.xml`, `build.gradle(.kts)`, `composer.json`, `*.tf`, `Chart.yaml`, `.npmrc` | any `<instance-host>/<group>/…/<project>` token (URL or Go module path), `.git` and `/-/…` stripped; package-registry URLs naming a project by id or encoded path; a Terraform `//sub/dir` kept as the target | `depends` |
| 3 | Container images | `Dockerfile*`, `Containerfile*`, `*.dockerfile`, `docker-compose*.yml`, `compose*.yml`, CI files (`image:`) | an image on the instance's registry host, matched to the longest project path | `image` |
| 3 | Submodules | `.gitmodules` | `url = …`, absolute or relative to the project's own path | `submodule` |
| 3 | Documentation | root `*.md`, `docs/**/*.md` (changelogs, licences and the like skipped) | instance URLs that resolve | `mentions` |

One claim per line: a line that already yielded a reference to a project is
not reported again by a weaker extractor.

Resolution: candidates are normalised (scheme, host, `.git`, trailing `/-/…`,
registry host → project path) and matched by the longest project path that is
a prefix at a `/` boundary — `host/acme/lib/v2` → `acme/lib`; `acme/lib-tools`
does not match `acme/lib`. Candidates that look like instance paths but
resolve to nothing are kept under `unresolved`, with their kind and target,
so that a later lookup turns them into exactly the link a scan would have
produced. References to excluded projects are passed over, not reported.
The instance answers on more than one name: its URL, the hosts its clone
URLs use, `clone_hosts` from the config, and its registry.

Deliberately not done: general YAML/TOML parsing, version-constraint
evaluation, following `include: local:` chains beyond the tree. Evidence is a
file and a line a human can open.

### Plugins: `gitty <cmd>` runs `gitty-<cmd>`

When `gitty`'s first argument is not a built-in command, `main.go` looks for
an executable named `gitty-<cmd>` — in the directory of the running `gitty`
binary first (a release archive unpacked in one place just works; a symlinked
`gitty` also looks beside its target), then on `PATH`, never in the current
directory — and runs it with the remaining arguments. On Linux and macOS
`gitty` replaces itself with the plugin (`exec`), so stdin, stdout, signals
and the exit code are the plugin's own and no process sits in between; on
Windows it runs a child and passes the exit code on. Nothing found:
`Unknown command: <cmd> (no gitty-<cmd> beside gitty or on PATH)`, plus an
install hint for `mcp`, exit 2. Built-in commands always win; a plugin
cannot shadow `sync`.

Before the exec, `gitty` runs the discovery every command runs and hands the
result over as environment:

| Variable | Value |
|---|---|
| `GITTY_BIN` | absolute path of the `gitty` that launched the plugin — the one the plugin runs for everything it does not do itself |
| `GITTY_WORKSPACE` | the workspace root: the outermost directory holding a `.gitty/config` of this workspace (`sync --groups` writes one into every group directory, so the nearest is often not the top) |
| `GITTY_GROUP` | full namespace path of the group the working directory is in; empty at the root of a workspace anchored at the instance's top level |
| `GITTY_INSTANCE` | the instance URL from the config |

No token is passed. A plugin needing the API runs `GITTY_BIN`, which resolves
credentials the way it always does (`--token`, `GITLAB_TOKEN`,
`CI_JOB_TOKEN`). Not being inside a workspace is not fatal to the dispatch —
the variables are set but empty and the plugin decides; `gitty-mcp` refuses
to start. A workspace that exists but was refused (owned by another user) or
is unreadable gets a `note:` on stderr, since the plugin could only say "no
workspace". The plugin starts in the directory the user was in: unlike the
built-in commands, the dispatch does not move into the group's directory.

`gitty mcp` is this rule applied to `gitty-mcp`; it has no code path of its
own.

### The MCP server — `gitty-mcp`, the primary surface

A separate binary at `cmd/gitty-mcp/`, built from the same module, released
from the same tag, required by nothing else. JSON-RPC 2.0 over stdio,
newline-delimited (a message up to 4 MiB), hand-written: `initialize`,
`notifications/initialized`, `ping`, `tools/list`, `tools/call`,
`notifications/cancelled`, batches. It answers protocol revisions 2024-11-05,
2025-03-26, 2025-06-18 and 2025-11-25 and offers the newest of those to a
client asking for any other. Tool calls run concurrently and can be
cancelled; calls in flight when stdin closes are finished. Protocol only on
stdout; diagnostics on stderr. `--no-fetch` turns the server read-only,
`--anon` reaches a public instance without a token, `--describe` prints the
tools as JSON for `gitty agent schema`.

It is **thin by rule**: it owns the protocol, the tool schemas and the two
tools that need nothing but a checkout (`gitty_search`, `gitty_read`).
Everything else is `gitty`:

| Tool | Backed by |
|---|---|
| `gitty_context`, `gitty_related` | `gitty context --format=json [--fetch] [--direction] [project]`, one process per call; the JSON is the tool result |
| `gitty_projects` | the index file (`<workspace>/.gitty/index.json`) decoded with `internal/contract`; `gitty index` run first when it is missing; cached by modification time |
| `gitty_fetch` | `gitty fetch --format=json <project>...` |
| `gitty_search`, `gitty_read` | `git grep` and file reads inside checkouts named by the index — local by nature |

The split in that table is deliberate: `gitty_context`, `gitty_related` and
`gitty_projects` are **index-backed** and could be served by any process
holding the index; `gitty_fetch`, `gitty_search` and `gitty_read` need the
caller's disk. The plugin keeps the two kinds apart in `tools.go` behind one
stdio front end, so a remote server later replaces the first and proxies the
second — see *Looking ahead*.

The tools, as the agent sees them:

| Tool | Input | Does |
|---|---|---|
| `gitty_context` | `{project?, fetch?: bool, direction?: "uses"\|"used-by"\|"both"}` | Resolves the subject (default: the project of the working directory; `project` may be a namespace path or an absolute directory), refreshes its index entry, computes related projects, **clones the ones it uses that are absent** (unless `fetch:false` / `--no-fetch`; at most 25, never a mention), returns the briefing as structured data: subject, each related project with `path` (absolute), `relative_path`, `links` (`kind`, `evidence`, `target`), `files`, `description`, `docs`, `fetched` (true if cloned by this call), `archived`; plus `unresolved`, `coverage`, `notes` and any fetch failures. |
| `gitty_related` | `{project?, direction?}` | The links only, no fetching. |
| `gitty_projects` | `{query?, checked_out?, limit?}` | Projects of the index matching words in their path, description or topics, with the local path if checked out. |
| `gitty_fetch` | `{projects: [...]}` | Brings up the named projects (link targets or not) into their namespace paths; returns paths and per-project outcome. Not offered with `--no-fetch`. |
| `gitty_search` | `{pattern, fixed?, ignore_case?, projects?}` | `git grep -n` across local checkouts (or the named ones), tracked files only; results `project`, `file`, `line`, `text`; bounded. |
| `gitty_read` | `{project, path, start?, end?}` | A tracked file or line range from a local checkout; refuses `..`, absolute paths, anything under `.git`, untracked files and symlinks that leave the checkout. |

Fetching happens inside `gitty` (`context --fetch`, `fetch`), so it is the
same `bringUp` as `sync`: credentials, host notes, interrupt handling, the
unborn-resume logic and the events (the plugin relays them to stderr) are
inherited. The index is written atomically, and one server serialises its
own fetches. Two servers cloning the same absent project at the same moment
are not locked against each other: one clone fails and is reported, and what
it left is resumable. Locking across processes is not built.

Cost: one `gitty` process per `gitty_context` call — a few milliseconds of
start-up plus the index load, within SC-002's 300 ms for a warm index. A
version skew between plugin and CLI surfaces as a tool error naming both
schema versions, never as a misread result.

### `gitty fetch <project>...`

The explicit form of what `context --fetch` does: look each namespace path
up on the instance, skip the ones already checked out, feed the rest to the
existing pool. It only ever adds: a project that is present is left exactly
as it is, not pulled, because the caller may be an agent in the middle of
work that a moving dependency would disturb. Events as `sync` (`clone`,
`skip <project> present`, `error`); `--format=json` prints one record per
project (`project`, `path`, `present`, `fetched`, `error`) for the plugin;
`--dry-run` parity; `exclude` honoured; a project not named is never
touched. A project that has moved is refused with its new path, and one the
workspace has no place for is refused rather than put somewhere arbitrary.
`context --fetch`, `gitty_fetch` and `sync --related` are all
`fetchProjects` with a different way of choosing the list; only the last
passes `update`, which also fast-forwards what is present.

### Looking ahead: a shared, remote `gitty-mcp` (not in this feature)

If the local plugin proves out, the next step is one server many
developers' agents share: an HTTP transport in front of the same
`indexTools`, holding one index for the whole instance that grows as anyone
indexes a project — a cache that gets better with use. What this feature
does to keep that cheap, and nothing more:

- Index-backed and workspace-backed tools are kept apart behind one stdio
  front end; the remote server implements the first kind and proxies the
  second to the client's own `gitty-mcp`.
- It is also the answer to the one thing a local index cannot know: who
  uses a project among the projects nobody here has checked out. `coverage`
  in every briefing is the honest placeholder until then.
- Results identify projects by namespace path; local paths are an attribute
  computed from the caller's workspace, so a remote answer can be completed
  locally ("acme/lib is at /ws/acme/lib on *your* machine").
- The index format is versioned and workspace-independent: `local.dir` is
  relative to the root, nothing in it is an absolute path.

Transport, authentication, multi-workspace merging and who runs the server
are that feature's questions, not this one's.

### Briefing (`gitty context`) — the same content for a terminal

```markdown
# acme/app — Customer-facing API
gitlab.example.com · `.` · main · topics: go, api · docs: README.md, AGENTS.md

## Uses (3)
- **acme/ci-templates** — Pipeline templates · `../ci-templates` (fetched) · ci-include templates/build.yml (.gitlab-ci.yml:3)
- **acme/lib** — Shared domain types · `../lib` · depends (go.mod:12)
- **acme/base-images** — Hardened base images · *not checked out* (`gitty fetch acme/base-images`) · image (Dockerfile:1)

## Used by (2)
- **acme/web** — Web frontend · `../web` · depends (package.json:21)
- **acme/ops/deploy** — Deployments · `../ops/deploy` · ci-trigger (.gitlab-ci.yml:40)

## References that are not projects here (1)
- acme/old-lib (go.mod:14)

## Where to read next
- acme/ci-templates: ../ci-templates/templates/build.yml, ../ci-templates/README.md
- acme/lib: ../lib/AGENTS.md, ../lib/README.md

## Notes
- Only checked-out projects are read for links: 4 of 42 here. "Used by" may be incomplete.
```

Stable headings, capped lists, paths relative to the directory the command
runs in. A side with nothing on it is said to be empty rather than left out,
and projects that are only mentioned in documentation come last. "Where to
read next" names the files the links point at before the documentation.
`--fetch` clones the absent projects the subject uses first; `--format=json`
emits exactly what `gitty_context` returns. For a group, the briefing lists
its projects and the links among them, then links out of the group.

`gitty -C <dir> <command>` runs any command as if started in `<dir>`: an
agent host that cannot choose where it starts a server says which project it
means with `gitty -C /ws/acme/app mcp`.

### `gitty index`, `gitty related`

`index` is the cache refresh made explicit: lists the workspace's groups,
asks about each checkout and each reference the listing could not place,
scans changed checkouts, writes the index, emits `index`, `link`,
`unresolved` and `summary` events; `--dry-run` prints the plan and writes
nothing; `--rescan` reads every checkout again. `related` prints a
project's links as `related`/`related-by` lines. With an index in place
`related` and `context` need no network and no credential.

### `sync --related`

`runSync` hands over to `runSyncRelated` when the flag is set. The subject
is resolved as `context` resolves it (the project of the working directory,
the checkouts of the group it is, or the argument), the index is brought up
to date, the subject's unplaced references are asked about, and the far side
of its links — minus mention-only projects — goes to `fetchProjects` with
`update`: absent projects are cloned and present ones fast-forwarded by
`syncOneRepo`, unchanged, so events, summary, exit codes, `--reclone-broken`
and interrupt handling are a sync's. `--dry-run` does the same reading and
asking in memory and writes nothing, the index included. A related project
is synced whether or not it is archived; one outside the namespace the
workspace holds is `skip <project> outside-workspace`; `--exclude` narrows
the run without being recorded in the index. Links are followed one step.

## Project Structure

### Documentation (this feature)

```text
specs/001-cross-repo-context/
├── spec.md              # requirements, clarified
├── plan.md              # this file
└── tasks.md             # the work, by phase and story
```

### Source Code (repository root)

```text
index.go            # Index types, load/save (atomic), staleness by HEAD, version
scan.go             # per-checkout scanner: git ls-files, name filters, extractors
resolve.go          # candidate → project path (host, registry, module-path, bare-path forms)
briefing.go         # briefing model, built from the index for a project or group
context.go          # `gitty context`: Markdown and JSON renderers, --fetch
related.go          # `gitty related` event lines
indexcmd.go         # `gitty index`
fetch.go            # `gitty fetch <project>...` and the bring-up-by-path it shares with context --fetch and sync --related
syncrelated.go      # `gitty sync --related`: the subject's links as a sync's project list
workspace.go        # the workspace as the context commands see it: root, subject directory, lazy API
plugin.go           # `gitty <cmd>` → `gitty-<cmd>`: lookup beside the binary / on PATH, env
plugin_unix.go, plugin_windows.go  # exec in place / run as a child
sync.go             # + NamespaceProjects and Project on the source; hands --related over
main.go             # + `-C <dir>`, plugin dispatch
commands.go         # + context, related, index, fetch in the command table (flags, completion); --related
agent.go            # + schema entries
README.md, context.md  # user and agent docs

internal/contract/
└── contract.go     # the JSON types gitty prints and gitty-mcp reads: Index, Briefing, FetchResult; Schema

cmd/gitty-mcp/
├── main.go         # flags (--no-fetch, --anon, --jobs, --version, --describe), environment from gitty, start/stop
├── rpc.go          # JSON-RPC 2.0 over stdio: framing, dispatch, cancellation, version negotiation
├── tools.go        # tool registry and schemas; index-backed tools (run gitty, read the index) and workspace-backed ones (fetch, search, read)
└── server_test.go  # driven over pipes against a stub `gitty`

index_test.go, scan_test.go, resolve_test.go, briefing_test.go, plugin_test.go, syncrelated_test.go
e2e_test.go         # + fake GitLab metadata, trimmed listings, single-project lookups; builds both binaries
context_e2e_test.go # index, related, context, fetch, -C, sync --related against the fake instance and real git
mcp_e2e_test.go     # gitty mcp end to end over stdio, both real binaries

scripts/mcp-smoke.sh           # a shell MCP client against gitlab.com, run in CI
.github/workflows/go.yml       # + builds both binaries, runs the smoke test
.github/workflows/release.yml  # + builds gitty-mcp_<version>_<os>_<arch> beside gitty
```

**Structure Decision**: the CLI stays the single `main` package at the root,
one file per concern, as `ls.go`, `status.go` and `auth.go` are today. The
plugin is a second `main` under `cmd/gitty-mcp/` in the same module (one
`go build ./...`, one release), sharing only `internal/contract/`: the JSON it
reads is the JSON `gitty` prints, so what is shared is types and a version
constant, nothing with behaviour.

## Phasing

| Phase | Delivers | Stories |
|---|---|---|
| **0 — Plugin seam** | `plugin.go` (`gitty <cmd>` → `gitty-<cmd>`, environment, exit codes); `internal/contract/`; the release workflow builds the second binary; README "Plugins" | — |
| **1 — Foundation** | `index.go`, `resolve.go`, `scan.go` with the CI and manifest extractors and fork edges; fake-GitLab metadata; `gitty index`, `gitty related` | US3 |
| **2 — Context, both surfaces** | `briefing.go`, `fetch.go` (`gitty fetch`), `context.go` (`gitty context`, `--fetch`, `--format=json`); `cmd/gitty-mcp/` (all six tools, `--no-fetch`); real-client smoke test; docs | US1, US2 |
| **3 — Shell fetch + remaining sources** | `sync --related`; image, submodule and mentions extractors | US4 |

Phases 0–2 together are the prototype the clarifications asked for: both
surfaces, CI links first, fetching on, the server as a plugin. Phase 3 rounds
it out.

## Decisions Settled

1. **Surface** — MCP is primary; `gitty context` is the same content for a
   terminal. Both in the prototype.
2. **Sources** — CI includes/triggers/components first, manifests with them;
   images, submodules, mentions after.
3. **Fetching** — on by default in the MCP server and available as `--fetch`
   on the CLI; `--no-fetch` / `fetch:false` for read-only use.
4. **Writing** — never inside a repository; nothing vendor-specific. The
   index is the only file gitty writes, beside its own config.
5. **Packaging** — the MCP server is a plugin binary, `gitty-mcp`, run by
   `gitty mcp` through a generic `gitty <cmd>` → `gitty-<cmd>` rule; thin,
   driving `gitty` for everything but search and read; released from the
   same tag; never required by the CLI.
6. **Remote** — a shared, remote server is a later feature. This one keeps
   the seam: index-backed and workspace-backed tools are separate
   interfaces, identity is the namespace path, the index holds no absolute
   paths.

### Changed or added while building

7. **`gitty fetch`, not `gitty sync --related`, is the fetch hint.** A
   briefing needs one command per absent project that touches nothing else.
8. **Fetching only adds, and only what the subject uses.** At most 25 per
   call, never a mention, never a pull: the caller may be an agent mid-task.
   `sync --related` is the explicit way to update as well.
9. **Trimmed listing plus one lookup per checkout**, instead of a full
   listing. Against gitlab.com the full listing of `gitlab-org` took 25 s
   and brought in projects shared from other namespaces and 99 fork links
   nobody asked about; the trimmed listing of `gitlab-org` and `components`
   together, 3,878 projects, takes about 10 s. Fork links therefore exist
   only for checked-out projects.
10. **References outside the listed groups are asked about by path**, and
    moved projects followed. The first live run left the commonest real
    case — a pipeline using a component from a group never cloned —
    unresolved.
11. **Coverage is part of every answer.** "Used by (0)" with 6 of 3,878
    projects read is not a fact about the project.
12. **Links carry a target; briefings name the files.** Knowing which
    project a pipeline includes from is half of knowing what to read.
13. **`gitty -C <dir>`**, because agent hosts do not all start a server in
    the project directory.
14. **Mentions are a kind of their own, listed last, never fetched.**

15. **`gitty agent schema` and `context.md` are gone.** They predate this
    feature and existed to tell an agent how to drive gitty. A hand-kept
    description of every flag is not worth its upkeep next to `--help`, and
    what an agent needs to know is moving into the server.

### Measured (gitlab.com, anonymous, from a cloud container)

| What | Time |
|---|---|
| First index of a workspace holding `gitlab-org` and `components` (3,878 projects listed, 6 checked out) | 10.7 s |
| `gitty context` on a warm index | about 30 ms |
| `gitty context --fetch` cloning five dependencies, two from a group never listed | about 2 s |
| `gitty sync --related` pulling the same five | 1.5 s |

SC-002's 50-checkout figures were not measured on a laptop; the warm path
does no network and one small file read per checkout.

### Not built, and why it is safe to leave

- Protocol revision 2026-07-28 of MCP exists and was not checked against;
  the server offers 2025-11-25 to a client that asks for it, which the
  protocol's negotiation allows.
- No locking between two servers on one workspace, no progress
  notifications during a long fetch, no shallow clones, no following of
  links beyond one step, no pinned-version checkout.

## Complexity Tracking

> No constitution violations; nothing to justify.

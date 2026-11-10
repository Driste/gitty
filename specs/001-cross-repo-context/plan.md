# Implementation Plan: Cross-Repository Context

**Branch**: `001-cross-repo-context` | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-cross-repo-context/spec.md`

## Summary

Make gitty the thing that tells an agent what *else* in the GitLab instance
matters to the project it is working in — and puts it within reach. gitty
already knows the namespace, holds the checkouts and knows their paths; this
feature adds **linking**: a workspace index of project metadata and the
evidenced references between projects, and an **MCP server** that, asked for
a project's context, resolves its related projects, **syncs the missing ones
into their namespace paths**, and returns to the agent each project's path and
the reason it matters. The same content renders as a Markdown briefing
(`gitty context`) and as greppable lines (`gitty related`); `gitty index`
refreshes the cache on demand and `sync --related` fetches from the shell.

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
  "instance": "https://gitlab.example.com",
  "generated_at": "2026-11-06T10:12:03Z",
  "projects": {
    "acme/app": {
      "id": 1201,
      "description": "Customer-facing API",
      "default_branch": "main",
      "topics": ["go", "api"],
      "archived": false,
      "forked_from": "",
      "registry_prefix": "registry.gitlab.example.com/acme/app",
      "local": { "dir": "acme/app", "head": "4e0f9c7…", "docs": ["README.md", "AGENTS.md"], "summary": "First paragraph of the README…" }
    }
  },
  "links": [
    { "from": "acme/app", "to": "acme/ci-templates", "kind": "ci-include", "evidence": ".gitlab-ci.yml:3" },
    { "from": "acme/app", "to": "acme/lib", "kind": "depends", "evidence": "go.mod:12" }
  ],
  "unresolved": [
    { "from": "acme/app", "ref": "acme/old-lib", "evidence": "go.mod:14" }
  ]
}
```

- Keyed by namespace path; `local` absent for projects known only from the
  API. `head` drives incrementality: `git rev-parse HEAD` equal to the record
  means no rescan and links kept.
- Written to a temp file and renamed; a `version` mismatch means rebuild.
- Project metadata for the whole target group comes from the listings gitty
  already does (one `simple=true` pass does not carry description/topics, so
  the index pass lists with full fields — bounded by the same parallel pager).

### Link discovery (the scanner)

One pass per changed checkout over `git ls-files`, name-filtered, each file
read once (≤ 1 MiB), lines matched by a small set of extractors. An extractor
yields `(kind, candidate, line)`; a candidate becomes a link only when
`resolve(candidate)` finds a project of the instance.

| Phase | Source | Files | Extractor | Kind |
|---|---|---|---|---|
| 1 | GitLab CI | `.gitlab-ci.yml`, `.gitlab/ci/**/*.yml`, `*.gitlab-ci.yml` | `project: <path>` under `include:` → `ci-include`; under `trigger:` → `ci-trigger`; `component: <host>/<path>/<name>@<ver>` → `ci-component` | per column |
| 1 | API | — | `forked_from_project.path_with_namespace` | `fork` |
| 1 | Package manifests | `go.mod`, `package.json`, `pyproject.toml`, `requirements*.txt`, `Gemfile`, `Cargo.toml`, `pom.xml`, `build.gradle(.kts)`, `composer.json`, `*.tf`, `Chart.yaml` | any `<instance-host>/<group>/…/<project>` token (URL or Go module path), `.git` and `/-/…` stripped | `depends` |
| 2 | Container images | `Dockerfile*`, `*.dockerfile`, `docker-compose*.yml`, `compose*.yml`, `.gitlab-ci.yml` (`image:`) | `FROM`/`image:` whose prefix matches a project's `registry_prefix` | `image` |
| 2 | Submodules | `.gitmodules` | `url = …` → path on the instance host | `submodule` |
| 2 | Documentation | `README*`, root `*.md`, `docs/**/*.md` | instance URLs, or bare `<group>/<project>` paths that resolve | `mentions` |

Resolution: candidates are normalised (scheme, host, `.git`, trailing `/-/…`,
registry host → project path) and matched by the longest project path that is
a prefix at a `/` boundary — `host/acme/lib/v2` → `acme/lib`; `acme/lib-tools`
does not match `acme/lib`. Candidates that look like instance paths but
resolve to nothing are kept under `unresolved`.

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
newline-delimited, hand-written: `initialize`, `notifications/initialized`,
`ping`, `tools/list`, `tools/call`. Protocol only on stdout; diagnostics on
stderr. `--no-fetch` turns the server read-only.

It is **thin by rule**: it owns the protocol, the tool schemas and the two
tools that need nothing but a checkout (`gitty_search`, `gitty_read`).
Everything else is `gitty`:

| Tool | Backed by |
|---|---|
| `gitty_context`, `gitty_related` | `gitty context --format=json [--fetch] [--direction] [project]`, one process per call; the JSON is the tool result |
| `gitty_projects` | the index file (`<workspace>/.gitty/index.json`) decoded with `internal/contract`; `gitty index` run first when it is missing; cached by modification time |
| `gitty_fetch` | `gitty fetch --format=json <project>...` |
| `gitty_search`, `gitty_read` | `git grep` and file reads inside checkouts named by the index — local by nature |

The split in that table is deliberate: the first three rows are
**index-backed** and could be served by any process holding the index; the
last row needs the caller's disk. The plugin keeps them as two interfaces
(`indexTools`, `workspaceTools`) behind one stdio front end, so a remote
server later replaces the first and proxies the second — see *Looking
ahead*.

The tools, as the agent sees them:

| Tool | Input | Does |
|---|---|---|
| `gitty_context` | `{project?, fetch?: bool, direction?: "uses"\|"used-by"\|"both"}` | Resolves the subject (default: the project of the working directory), refreshes its index entry if its HEAD moved, computes related projects, **fetches the absent ones** (unless `fetch:false` / `--no-fetch`), returns the briefing as structured data: subject, each related project with `path` (absolute), `relative_path`, `kind`, `evidence`, `description`, `docs`, `fetched` (true if cloned by this call), `archived`; plus `unresolved` and any fetch failures. |
| `gitty_related` | `{project, direction?}` | The links only, no fetching. |
| `gitty_projects` | `{query?}` | Projects of the index matching a substring (path, description), with local path if checked out. |
| `gitty_fetch` | `{projects: [...]}` | Brings up the named projects (link targets or not) into their namespace paths; returns paths and per-project outcome. The explicit form of what `gitty_context` does implicitly. |
| `gitty_search` | `{pattern, projects?, max?}` | `git grep -n` across local checkouts (or the named ones); results `project`, `file`, `line`, `text`; bounded. |
| `gitty_read` | `{project, path, start?, end?}` | A file or line range from a local checkout; refuses `..`, absolute paths and symlinks that escape the checkout. |

Fetching happens inside `gitty` (`context --fetch`, `fetch`), so it is the
same `bringUp` as `sync`: credentials, host notes, interrupt handling, the
unborn-resume logic and the events (the plugin relays them to stderr) are
inherited. Concurrent clients are safe: the index is written atomically and
an in-progress bring-up of the same project is resumed, not clobbered.

Cost: one `gitty` process per `gitty_context` call — a few milliseconds of
start-up plus the index load, within SC-002's 300 ms for a warm index. A
version skew between plugin and CLI surfaces as a tool error naming both
schema versions, never as a misread result.

### `gitty fetch <project>...`

The explicit form of what `context --fetch` does: resolve each namespace
path to a project (index first, API if unknown), skip the ones already
checked out, feed the rest to the existing pool. Events as `sync`;
`--format=json` prints one record per project (`path`, `dir`, `fetched`,
`present`, `error`) for the plugin; `--dry-run` parity; `exclude` honoured;
a project not named is never touched. `sync --related` and `gitty_fetch`
are both this with a different way of choosing the list.

### Looking ahead: a shared, remote `gitty-mcp` (not in this feature)

If the local plugin proves out, the next step is one server many
developers' agents share: an HTTP transport in front of the same
`indexTools`, holding one index for the whole instance that grows as anyone
indexes a project — a cache that gets better with use. What this feature
does to keep that cheap, and nothing more:

- `indexTools` and `workspaceTools` are separate interfaces behind one stdio
  front end; the remote server implements the first and proxies the second
  to the client's own `gitty-mcp`.
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
gitlab.example.com · ./ · main · topics: go, api · docs: README.md, AGENTS.md

## Uses (3)
- **acme/ci-templates** — Pipeline templates · `../ci-templates` (fetched) · ci-include (.gitlab-ci.yml:3)
- **acme/lib** — Shared domain types · `../lib` · depends (go.mod:12)
- **acme/base-images** — Hardened base images · *not checked out* (`gitty sync --related`) · image (Dockerfile:1)

## Used by (2)
- **acme/web** — Web frontend · `../web` · depends (package.json:21)
- **acme/ops/deploy** — Deployments · `../ops/deploy` · ci-trigger (.gitlab-ci.yml:40)

## References that are not projects here (1)
- acme/old-lib (go.mod:14)

## Where to read next
- acme/lib: ../lib/AGENTS.md, ../lib/README.md
```

Stable headings, capped lists, paths relative to the directory the command
runs in. `--fetch` syncs the absent related projects first; `--format=json`
emits exactly what `gitty_context` returns. For a group, the briefing lists
its projects and the links among them, then links out of the group.

### `gitty index`, `gitty related`

`index` is the cache refresh made explicit: lists the target group with full
metadata, scans changed checkouts, writes the index, emits `index`, `link`,
`unresolved` and `summary` events; `--dry-run` prints the plan and writes
nothing. `related` prints a project's links as `related`/`related-by` lines.

### `sync --related`

Resolves the current project's links (direction configurable), filters to
targets not checked out, and feeds that project list to the existing pool —
`syncOneRepo` unchanged — so everything about syncing is inherited.

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
plugin.go           # `gitty <cmd>` → `gitty-<cmd>`: lookup beside the binary / on PATH, env, exec
sync.go             # + --related selection
commands.go         # + context, related, index, fetch in the command table (flags, completion)
agent.go            # + schema entries
README.md, context.md  # user and agent docs

internal/contract/
└── contract.go     # the JSON types gitty prints and gitty-mcp reads: Index, Briefing, FetchResult; Schema

cmd/gitty-mcp/
├── main.go         # flags (--no-fetch, --jobs, --version), environment from gitty, start/stop
├── rpc.go          # JSON-RPC 2.0 over stdio: framing, dispatch, error objects
├── tools.go        # tool registry and schemas; indexTools (run gitty, read the index) and workspaceTools (search, read)
└── *_test.go       # driven over io.Pipe against a stub `gitty` on PATH

index_test.go, scan_test.go, resolve_test.go, briefing_test.go, plugin_test.go
e2e_test.go         # + fake GitLab metadata; builds both binaries; MCP end to end over stdio

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

## Complexity Tracking

> No constitution violations; nothing to justify.

# Implementation Plan: Cross-Repository Context

**Branch**: `001-cross-repo-context` | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-cross-repo-context/spec.md`

## Summary

Make gitty the thing that tells an agent what *else* in the GitLab instance
matters to the project it is working in. gitty already knows the namespace,
holds the checkouts and knows their paths; this feature adds **linking**: a
workspace index of project metadata and the references between projects
(manifests, CI includes, images, submodules, forks, doc mentions), each with
evidence, and three ways to consume it — a Markdown briefing (`gitty context`),
greppable edge lines (`gitty related`), and an MCP server (`gitty mcp`) — plus
`sync --related` to fetch what the briefing says is missing.

The approach is deliberately textual and local: references are found by
scanning files git tracks for paths and URLs that resolve to projects of the
instance, never by running package managers or evaluating build files. The
index is a JSON file under the workspace's `.gitty/`, rebuilt incrementally
by HEAD commit, and the briefing is rendered from it on demand.

## Technical Context

**Language/Version**: Go 1.24 (pinned in `go.mod`)
**Primary Dependencies**: standard library only — `encoding/json`, `regexp`,
`bufio`, `crypto/sha256`, `os/exec` (git). Existing: GitLab client-go,
go-toml. **No new modules.** MCP is JSON-RPC 2.0 over stdio, hand-written.
**Storage**: `<workspace>/.gitty/index.json` (schema-versioned),
`<workspace>/.gitty/context/<ns-path>.md` (only with `--write`)
**Testing**: `go test ./...` — unit tests with fixture trees; e2e against the
existing fake GitLab (extended with description/topics/fork fields) and real
git; MCP tested by driving the server over pipes in-process.
**Target Platform**: macOS/Linux amd64/arm64; Windows best-effort (paths via
`filepath`, output via `path`).
**Project Type**: CLI (single `main` package, as today)
**Performance Goals**: cold index of 50 checkouts < 30 s; warm (no HEAD moved)
< 1 s; `context` from a warm index < 200 ms.
**Constraints**: offline after index build (`context`, `related`, `mcp` need no
network when the index is warm); read-only by default; bounded scans (tracked
files only, name-filtered, ≤ 1 MiB each).
**Scale/Scope**: workspaces of up to a few thousand projects; briefings capped
to fit an instruction budget.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | How this feature complies |
|---|---|
| **I. CLI-first, safe by default** | New commands are read-only (`context`, `related`, `index` reads repos and writes only workspace metadata under `.gitty/`; `mcp` reads). The two things that write — `index` to `.gitty/index.json`, `context --write` to `CLAUDE.md` — support `--dry-run` (`index`) and require a named flag (`--write`). `sync --related` goes through the existing sync path and inherits its dry-run parity and idempotence. Nothing is ever deleted. Exit codes follow the existing taxonomy (0/1/2/130). |
| **II. Minimal deps, structured observability** | Zero new modules: JSON-RPC and YAML-ish extraction are done with `encoding/json`, `regexp` and line scanning (we extract references, we do not parse YAML generally). `index` and `related` emit one event per line with fixed prefixes (`index`, `link`, `related`, `related-by`, `summary`); `context` prints Markdown to stdout as its primary output, as `ls --format=tree` already does, with `--format=json` for machines. `--verbose` shows every git invocation and every file scanned. The index stores no token; the MCP server never returns one. |
| **III. Config-anchored workspaces** | Every command locates the workspace with the existing discovery; the index lives beside the workspace config, not in a repo and not in `$HOME`; `exclude`/`clone_hosts`/`archived` semantics are reused unchanged. No flag mutates config. |
| **Additional constraints** | GitLab REST only (project metadata, languages, fork origin); token from the existing resolution order; interruptible — `index` checks `ctx` between projects and a partial index is not written (atomic rename). |
| **Quality gates** | `go build`/`go vet`/`go test` as today; dry-run parity for `index` and `sync --related` demonstrated in e2e tests. |

**Violations**: none. (Complexity Tracking left empty.)

One constitution *amendment* this feature will motivate but does not require:
Principle III still names `gitty.toml`; the workspace file has been
`.gitty/config` since before this branch. Tracked separately.

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
      "local": { "dir": "acme/app", "head": "4e0f9c7…", "docs": ["README.md", "CLAUDE.md"], "summary": "First paragraph of README…" }
    }
  },
  "links": [
    { "from": "acme/app", "to": "acme/lib", "kind": "depends", "evidence": "go.mod:12" },
    { "from": "acme/app", "to": "acme/ci-templates", "kind": "ci-include", "evidence": ".gitlab-ci.yml:3" },
    { "from": "acme/app", "to": "acme/base-images", "kind": "image", "evidence": "Dockerfile:1" }
  ],
  "unresolved": [
    { "from": "acme/app", "ref": "acme/old-lib", "evidence": "go.mod:14" }
  ]
}
```

- Keyed by namespace path; `local` absent for projects known only from the
  API. `head` drives incrementality: a checkout whose `git rev-parse HEAD`
  equals the recorded one is not rescanned and its links are kept.
- Written atomically (temp file + rename) so an interrupted run leaves the old
  index intact. A `version` mismatch means "rebuild", never "misread".
- No token, no URL with userinfo, no file contents beyond the README summary.

### Link discovery (the scanner)

One pass per changed checkout over `git ls-files` output, name-filtered, each
file read once (≤ 1 MiB), lines matched against a small set of extractors.
Every extractor yields `(kind, candidate, line)`; a candidate becomes a link
only if `resolveProject(candidate)` finds a project of the instance.

| Source | Files | Extractor | Kind |
|---|---|---|---|
| Git submodules | `.gitmodules` | `url = …` → path from the instance URL | `submodule` |
| GitLab CI | `.gitlab-ci.yml`, `.gitlab/ci/**/*.yml`, any `*.gitlab-ci.yml` | `project: <path>` under `include:` → `ci-include`; under `trigger:` → `ci-trigger`; `component: <host>/<path>/<name>@<ver>` → `ci-component` | per column |
| Container images | `Dockerfile*`, `*.dockerfile`, `docker-compose*.yml`, `compose*.yml`, `.gitlab-ci.yml` (`image:`) | `FROM`/`image:` whose prefix matches a project's `registry_prefix` | `image` |
| Package manifests | `go.mod`, `package.json`, `pyproject.toml`, `requirements*.txt`, `Gemfile`, `Cargo.toml`, `pom.xml`, `build.gradle(.kts)`, `composer.json`, `*.tf`, `Chart.yaml` | any `<instance-host>/<group>/…/<project>` token (URL or Go module path), with `.git` and `/-/…` stripped | `depends` |
| Forks | API | `forked_from_project.path_with_namespace` | `fork` |
| Documentation | `README*`, `CLAUDE.md`, `AGENTS.md`, `docs/**/*.md` | `<instance-host>/<path>` URLs, or a bare `<group>/<project>` path that resolves | `mentions` |

Resolution is the one careful bit: candidates are normalised (scheme, host,
`.git`, trailing `/-/…`, registry host → project path) and matched by longest
project path that is a prefix at a `/` boundary, so `host/acme/lib/v2` resolves
to `acme/lib` and `acme/lib-tools` does not match `acme/lib`. Unresolved
candidates that *look like* instance paths are kept in `unresolved` so the
briefing can say "references acme/old-lib, which is not in the instance".

What this deliberately does not do: parse YAML or TOML generally, evaluate
version constraints, or follow `include: local:` chains beyond the files
already in the tree. Evidence is a file and line a human can open.

### Briefing (`gitty context`)

Rendered from the index for one project (or one group). Stable headings so
agents can rely on them; capped lists; relative paths from the directory the
command runs in.

```markdown
# acme/app — Customer-facing API
gitlab.example.com · ./  · main · topics: go, api · docs: README.md, CLAUDE.md

## Uses (3)
- **acme/lib** — Shared domain types · `../lib` · depends (go.mod:12)
- **acme/ci-templates** — Pipeline templates · *not checked out* (`gitty sync --related`) · ci-include (.gitlab-ci.yml:3)
- **acme/base-images** — Hardened base images · `../base-images` · image (Dockerfile:1)

## Used by (2)
- **acme/web** — Web frontend · `../web` · depends (package.json:21)
- **acme/ops/deploy** — Deployments · `../ops/deploy` · ci-trigger (.gitlab-ci.yml:40)

## References that are not projects here (1)
- acme/old-lib (go.mod:14)

## Where to read next
- acme/lib: ../lib/CLAUDE.md, ../lib/README.md
```

`--format=json` emits the same as structured data. For a group, the briefing
lists its projects and the links between them, then links out of the group.

### MCP server (`gitty mcp`)

JSON-RPC 2.0 over stdio (newline-delimited), hand-written: `initialize`,
`notifications/initialized`, `ping`, `tools/list`, `tools/call`. Tools and
their inputs:

| Tool | Input | Returns |
|---|---|---|
| `gitty_projects` | `{query?}` | projects of the index matching the query (path, description, local dir) |
| `gitty_context` | `{project?}` | the Markdown briefing (same renderer) |
| `gitty_related` | `{project, direction?: "uses"\|"used-by"\|"both"}` | the links as structured data |
| `gitty_search` | `{pattern, projects?, max?}` | `git grep -n` across local checkouts; `project:file:line: text`, bounded |
| `gitty_read` | `{project, path, start?, end?}` | a file (or line range) from a local checkout; refuses `..`, absolute paths and anything outside the checkout |

The server is read-only, prints nothing to stdout but protocol, logs to
stderr, and reuses workspace discovery — so `claude mcp add gitty -- gitty mcp`
configured once works in every checkout. The existing `gitty agent schema`
tool list gains the new commands.

### `sync --related`

Resolves the current project's links (direction configurable), filters to
targets not checked out, and hands that project list to the existing bring-up
machinery — the same `syncOneRepo`, so credentials, host notes, dry-run
parity, interrupt handling and events are all inherited. It never touches a
project that is not a link target.

### `context --write`

Writes the briefing to `<workspace>/.gitty/context/<ns-path>.md` and appends
`@<relative path to that file>` to the checkout's `CLAUDE.md` if not already
present (creating the file with just that line if absent). Idempotent. The
only place gitty writes inside a repository, and only behind the flag.

## Project Structure

### Documentation (this feature)

```text
specs/001-cross-repo-context/
├── spec.md              # what and why (this feature's requirements)
├── plan.md              # this file
└── tasks.md             # created once the plan is agreed
```

### Source Code (repository root)

```text
index.go            # Index type, load/save (atomic), staleness by HEAD, schema version
scan.go             # per-checkout scanner: git ls-files, name filters, extractors
resolve.go          # candidate → project path resolution (host/registry/module/path forms)
context.go          # briefing model + Markdown/JSON renderers; `gitty context`
related.go          # `gitty related` event lines
mcp.go              # JSON-RPC over stdio, tool dispatch
mcp_tools.go        # the five tools, confinement checks
sync.go             # + `--related` selection feeding the existing pool
commands.go         # + context, related, index, mcp in the command table (flags, completion)
agent.go            # + schema entries
README.md, context.md  # user and agent docs

index_test.go, scan_test.go, resolve_test.go, context_test.go, mcp_test.go
e2e_test.go         # + fake GitLab: description, topics, forked_from, registry prefix
```

**Structure Decision**: stay in the single `main` package the project already
uses; one file per concern, matching how `ls.go`, `status.go` and `auth.go`
are organised today.

## Phasing

Each phase ships independently and is useful on its own.

| Phase | Delivers | Stories |
|---|---|---|
| **1 — Index + briefing** | `index.go`, `scan.go` (submodules, CI, images, manifests, forks), `resolve.go`, `context.go`, `related.go`; `gitty context`, `gitty related`, `gitty index`; fake-GitLab metadata; docs | US1, US2 |
| **2 — Fetch what's missing** | `sync --related` | US4 |
| **3 — MCP** | `mcp.go`, `mcp_tools.go`, `gitty mcp`; conformance smoke test | US3 |
| **4 — Wiring + mentions** | `context --write`; `mentions` extractor over docs; `AGENTS.md` handling per clarification | US5 |

Phase 1 is the prototype that answers "is this useful?" and is where the
clarifications below change the work.

## Open Decisions (to settle before tasks.md)

1. **First agent surface** — Markdown briefing first, MCP after (recommended:
   the briefing works with every agent and costs nothing to configure), or
   MCP first.
2. **Link sources to prioritise** — which of: GitLab CI includes/triggers/
   components; package manifests (and which ecosystems); container images and
   submodules; documentation mentions. All are planned; order decides what
   phase 1 proves.
3. **Fetching** — should `context` be able to clone missing related projects
   itself (opt-in `--sync`), or only tell the agent the command (`sync
   --related`)? Recommended: tell in phase 1, add the flag in phase 2.
4. **Writing into repos** — print only; write briefings under
   `.gitty/context/` in the workspace (outside repos); or also offer `--write`
   to add the one import line to `CLAUDE.md` (and `AGENTS.md`?). Recommended:
   workspace files always available, repo edit only behind `--write`.

## Complexity Tracking

> No constitution violations; nothing to justify.

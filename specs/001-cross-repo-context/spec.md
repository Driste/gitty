# Feature Specification: Cross-Repository Context

**Feature Branch**: `001-cross-repo-context`
**Created**: 2026-11-06
**Status**: Clarified — ready for tasks
**Input**: User description: "Link context between many repos in a GitLab instance. Often you need an agent to know about other projects in order to program in another."

## Problem

A coding agent working inside one checkout sees one repository. The things it
actually needs to know about live elsewhere in the same GitLab instance: the
service this one calls, the library it imports, the pipeline template it
includes, the base image it builds from, the three other repositories that
depend on the function it is about to change. That knowledge exists — in
manifests, in `.gitlab-ci.yml`, in people's heads — but nothing hands it to the
agent, so it guesses, or asks, or reads the wrong code.

gitty is uniquely placed to fix this. It already knows the whole namespace
(the API), it already has — or can fetch — a checkout of every project in it,
and it already knows where each one lives on disk. The missing piece is
*linking*: working out which projects relate to which, and presenting that to
an agent in the shape an agent can use — **the path to the related project,
and why it is relevant** — fetching the project into place if it is not there
yet.

## Clarifications

### Session 2026-11-06

- Q: Which surface first — Markdown briefing, MCP, or both? → A: **Both in
  phase 1.** The MCP server is the primary surface; the briefing is the same
  content rendered for a terminal.
- Q: Which link sources first? → A: **GitLab CI includes/triggers/components**
  first; package manifests next; images, submodules and doc mentions after.
- Q: May gitty fetch related projects that are not checked out? → A: **Yes, in
  phase 1**: when an agent asks for a project's context, gitty syncs the
  related projects into their namespace paths and returns those paths.
- Q: Should gitty write briefings into repositories (`CLAUDE.md`)? → A: **No.
  Nothing agent-vendor-specific.** `gitty mcp` is the command; the server
  owns caching (the index) and fetching, and returns to the calling agent the
  path to each related project and the context of why it matters to the
  project the agent is working in. No files are written inside repositories.
- Q: Is the MCP server part of the `gitty` binary? → A: **No — it is a
  plugin.** It ships as a separate binary, `gitty-mcp`, found and run by
  `gitty mcp` the way git runs `git-<cmd>`: beside the `gitty` executable
  first, then on `PATH`. Users who never run an agent never carry it; the
  core stays the CLI.
- Q: Could the server later run remotely, shared by many developers as a
  common cache? → A: **Eventually, yes — not in this feature.** The plugin is
  built so that the part that answers from the index can be lifted out and
  served over the network later; the part that needs a local checkout
  (fetching, reading, searching) stays local. Nothing in this feature's
  interfaces assumes the index and the checkouts share a machine.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An agent asks gitty what else matters (Priority: P1)

An agent is working in a checkout inside a gitty workspace. Its host has
`gitty mcp` (which runs the `gitty-mcp` plugin) configured once. The agent calls `gitty_context` (no arguments:
"the project I am in"). gitty works out which other projects of the instance
this one uses and which use it — from its CI configuration, its manifests, and
the API — **syncs any related project that is not yet checked out into its
namespace path**, and returns, for each related project: the absolute local
path, the kind of relationship, the file and line that proves it, the
project's description, and the documentation files worth reading there. The
agent opens the paths and carries on.

**Why this priority**: This is the loop the feature exists for — "I need to
know about B" becomes "B is at /ws/acme/lib, you depend on it via go.mod:12,
here is its README" in one call, with no human in between and no setup beyond
registering the server.

**Independent Test**: With A checked out, and A's `.gitlab-ci.yml` including a
template from B (not checked out), a `gitty_context` call from A returns B
under *uses* with kind `ci-include`, evidence `.gitlab-ci.yml:<line>`, and an
absolute path that now exists on disk with B's files in it; a second call
returns the same without cloning again.

**Acceptance Scenarios**:

1. **Given** A checked out and referencing B, B present upstream but absent
   locally, **When** the agent calls `gitty_context`, **Then** the result
   lists B with kind, evidence, description and an absolute path, and B's
   checkout exists at that path.
2. **Given** the same, **When** the agent calls `gitty_context` again,
   **Then** B is not fetched again and the result is unchanged.
3. **Given** `fetch: false` on the call (or a server started with
   `--no-fetch`), **When** B is absent, **Then** B is listed as not checked
   out, with the `gitty sync` invocation that would fetch it, and nothing is
   cloned.
4. **Given** B references A, **When** the agent in A asks, **Then** B appears
   under *used by* with its evidence.
5. **Given** A references a path that is not a project of the instance,
   **When** the agent asks, **Then** it is reported under *unresolved*, never
   presented as a project or fetched.
6. **Given** the agent then calls `gitty_read` for a file in B, or
   `gitty_search` for a symbol across the workspace, **Then** it gets the
   content or matches, confined to the workspace's checkouts.

---

### User Story 2 - The same briefing in a terminal (Priority: P2)

A human — or an agent that only has a shell — runs `gitty context` inside a
checkout and gets the same content as Markdown: this project, what it uses,
what uses it, with evidence, descriptions and paths. `--fetch` makes it sync
the missing related projects first, as the MCP call does; without it, missing
projects are named with the command that fetches them. `--format=json` emits
the structured form the MCP tool returns.

**Why this priority**: Same renderer as US1, so it is nearly free, and it is
how the output gets reviewed, piped, and used by agents without MCP support.

**Independent Test**: In a workspace where A's manifest references B (checked
out), `gitty context` run inside A prints B under "Uses" with
`go.mod:<line>` and B's path; run inside B it prints A under "Used by".

**Acceptance Scenarios**:

1. **Given** A and B checked out with A referencing B, **When** `gitty
   context` runs inside A, **Then** B is listed under "Uses" with kind,
   evidence, description and relative path.
2. **Given** the same, **When** run inside B, **Then** A is under "Used by".
3. **Given** B absent and no `--fetch`, **When** run inside A, **Then** B is
   marked not checked out with the fetch command, and nothing is cloned.
4. **Given** the working directory is a group, **When** `gitty context` runs,
   **Then** it briefs the group: its projects and the links among them and out
   of them.

---

### User Story 3 - Index the workspace, inspect the links (Priority: P3)

A user runs `gitty index` anywhere in the workspace. gitty scans every local
checkout for references to other projects of the instance, fetches project
metadata from the API, and writes one index file under the workspace's
`.gitty/`. Re-running is fast: checkouts whose HEAD has not moved are not
rescanned. `gitty related <project>` prints the links as greppable lines.

**Why this priority**: The index is the cache the MCP server and `context`
read; both build or refresh it themselves when it is missing or stale, so
this story is about making the cache *inspectable* and *refreshable on
demand*, not about making it exist.

**Independent Test**: After `gitty index`, `.gitty/index.json` lists every
checkout with metadata and every link with evidence; `gitty related acme/app`
prints one `related acme/app <kind> <target> <evidence>` per link and a
summary; a second `gitty index` with nothing changed rescans nothing and ends
in under a second for fifty checkouts.

**Acceptance Scenarios**:

1. **Given** a workspace of checkouts, **When** `gitty index` runs, **Then**
   stdout carries one `index <project> new|changed|unchanged` line per
   project, one `link <from> <kind> <to> <evidence>` per link found, and a
   summary; the index file is written atomically.
2. **Given** nothing changed, **When** `gitty index` runs again, **Then** no
   checkout is rescanned and the summary says so.
3. **Given** `--dry-run`, **When** `gitty index` runs, **Then** it prints what
   it would scan and writes nothing.
4. **Given** a project in the workspace's `exclude` list, **When** `gitty
   index` runs, **Then** it is neither scanned nor linked to.

---

### User Story 4 - Fetch what this project needs, from the shell (Priority: P4)

`gitty sync --related` run inside a checkout clones exactly the projects this
one links to (or that link to it, with `--related=both`) that are not yet
checked out, into their namespace paths, and nothing else. Re-running pulls.

**Why this priority**: The shell counterpart of what the MCP server does in
US1; useful for scripts and for pre-warming a workspace before an agent runs.

**Independent Test**: With A checked out and its link targets B and C absent,
`gitty sync --related` from inside A clones B and C and no sibling, printing
`clone` events; a second run prints `pull` events only.

---

### Edge Cases

- A reference points at a project that was moved or deleted upstream: reported
  as unresolved with its evidence; never fabricated, never fetched.
- Two projects reference each other: both directions are listed; nothing
  recurses.
- A project with hundreds of dependents: "used by" is capped with a count and
  a pointer to `gitty related` for the full list.
- A checkout that is not a project of the instance (hand-cloned from
  elsewhere): indexed as local-only, never a link target.
- Archived projects: fetched and briefed when they are a link *target* (they
  exist), not scanned as *sources* unless `--archived`; the result says they
  are archived.
- Excluded projects: never scanned, never linked, never fetched.
- A fetch fails (network, auth): the project is still listed, marked not
  checked out, with the failure reason; the rest of the result is unaffected.
- A huge checkout: scanning is bounded by tracked-file name patterns and a
  per-file size cap, and reported per project.
- No index and no network: `context` and the MCP tools report that the index
  cannot be built (exit 2 / tool error); they never brief from nothing.
- An index written by an older gitty: a version field makes it rebuilt.
- Two MCP clients on the same workspace: the index file is written atomically;
  a concurrent fetch of the same project is serialised by the existing
  bring-up (an unborn repository is resumed, not clobbered).
- `gitty mcp` with no `gitty-mcp` installed: exit 2 naming the binary looked
  for and where; nothing else happens.
- `gitty-mcp` run with a `gitty` of another version: a JSON reply whose
  schema version the plugin does not support becomes a tool error naming
  both versions — never a guess at the shape.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The MCP server MUST be a separate binary, `gitty-mcp`, serving
  MCP over stdio using only the standard library and exposing `gitty_context`,
  `gitty_related`, `gitty_projects`, `gitty_fetch`, `gitty_search` and
  `gitty_read`. It MUST write nothing but protocol to stdout; diagnostics go
  to stderr. `gitty` itself MUST build, test and run without it.
- **FR-001a**: `gitty <cmd>` for a `cmd` not in the built-in command table
  MUST look for an executable `gitty-<cmd>` — first beside the running
  `gitty` binary, then on `PATH` — and run it with the remaining arguments,
  inheriting stdin, stdout, stderr and the exit code. When none exists the
  message MUST name the plugin that was looked for and where, and for `mcp`
  how to install it; exit 2. `gitty mcp` is this rule applied to
  `gitty-mcp`; built-in commands can never be shadowed by a plugin.
- **FR-001b**: A plugin MUST receive, in its environment, the path of the
  `gitty` binary that launched it (`GITTY_BIN`), the workspace root
  (`GITTY_WORKSPACE`), the group of the working directory (`GITTY_GROUP`,
  possibly empty) and the instance URL (`GITTY_INSTANCE`) — resolved by
  `gitty` with the same discovery every command uses. Credentials MUST NOT be
  passed: a plugin that needs the API runs `gitty`, which resolves them
  itself.
- **FR-001c**: `gitty-mcp` MUST NOT reimplement gitty: it obtains context by
  running `gitty context --format=json`, fetches by running `gitty fetch`,
  refreshes by running `gitty index`, and lists projects from the versioned
  index file; only `gitty_search` and `gitty_read` — which need nothing but a
  checkout — are implemented in the plugin itself.
- **FR-002**: `gitty_context` MUST return, for the named project (or the one
  the server's working directory is in), its metadata and its outgoing and
  incoming links; each related project with its absolute local path or its
  absence, the link kind, the evidence, the description and its documentation
  files. With `fetch` true (the default unless the server runs with
  `--no-fetch`) it MUST first bring up every related project that is absent,
  through the existing sync path.
- **FR-003**: `gitty context [project]` MUST print the same content as
  Markdown with stable headings, or as JSON with `--format=json`; `--fetch`
  MUST do what `fetch: true` does.
- **FR-004**: Links MUST be discovered from, in this order of implementation:
  `.gitlab-ci.yml` (`include: project:`, `trigger: project:`,
  `include: component:`); package manifests naming a project by instance URL
  or module path; container image references to the instance's registry;
  `.gitmodules`; the API's fork origin; documentation mentions.
- **FR-005**: Every link MUST carry a kind from the fixed set `depends`,
  `ci-include`, `ci-trigger`, `ci-component`, `image`, `submodule`, `fork`,
  `mentions`, and evidence (`file:line`, or the API field).
- **FR-006**: A reference becomes a link only when it resolves to a project of
  the instance by namespace path; unresolved references are returned as such.
- **FR-007**: The index MUST live at `<workspace>/.gitty/index.json`, carry a
  schema version, be written atomically, and contain no token or credential.
- **FR-008**: Indexing MUST be incremental by HEAD commit: an unchanged
  checkout is not rescanned and keeps its links.
- **FR-009**: `gitty index` MUST honour `--dry-run`, the workspace `exclude`
  list and `--exclude`, and MUST emit one greppable event per project and per
  link.
- **FR-010**: `gitty related <project>` MUST print `related <project> <kind>
  <target> <evidence>` for outgoing and `related-by <project> <kind> <source>
  <evidence>` for incoming links, closing with a summary line.
- **FR-011**: `gitty_read` and `gitty_search` MUST be confined to local
  checkouts of the workspace: `..`, absolute paths and symlinks escaping a
  checkout are refused.
- **FR-012**: `gitty sync --related[=uses|used-by|both]` MUST clone only link
  targets (and/or sources) of the current project that are absent, via the
  existing sync machinery with its dry-run parity.
- **FR-013**: Nothing in this feature writes inside a repository, and nothing
  is specific to one agent vendor.
- **FR-014**: All commands and tools MUST work from any directory inside the
  workspace via the existing discovery; the MCP server MUST discover the
  workspace from its working directory at start.
- **FR-015**: `gitty_context` and `gitty context` MUST build or refresh the
  index when it is missing, versioned differently, or when the current
  project's HEAD has moved since it was indexed.
- **FR-016**: The JSON forms this feature introduces — `gitty context
  --format=json`, `gitty fetch --format=json`, the index file — are public
  contracts: each carries a schema version, additions never break a reader
  of the same version, and a reader refuses a version it does not know
  rather than guessing.
- **FR-017**: `gitty fetch <project>...` MUST bring up exactly the named
  projects (namespace paths) into their namespace paths through the existing
  sync machinery — nothing else is touched — printing the usual events, and
  with `--format=json` one record per project: path, directory, `fetched`,
  `present`, error. It MUST honour `--dry-run` and `exclude`.

### Key Entities

- **Project**: a GitLab project of the instance — namespace path, id,
  description, default branch, topics, archived, fork origin, container
  registry prefix; when checked out: local directory, HEAD at last scan,
  documentation files (README, any `*.md` at the root naming agent
  instructions), README summary.
- **Link**: directed, from one project to another — kind, evidence, found at
  which index run.
- **Index**: all projects and links for one workspace; schema version,
  instance URL, generation time.
- **Briefing**: the per-project (or per-group) view of the index: the subject,
  its outgoing and incoming links, related projects' descriptions, paths and
  docs, unresolved references.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With A checked out and referencing B (absent), one
  `gitty_context` call from A returns B's absolute path and the reason, and
  B's files are on disk at that path — no other input from the user.
- **SC-002**: A cold `gitty index` of 50 checkouts finishes in under 30 s on a
  laptop; a warm one with no changes in under 1 s; a warm `gitty_context`
  call that fetches nothing answers in under 300 ms.
- **SC-003**: Every link in the index traces to a file and line (or API field)
  a reviewer can open and confirm — zero links without evidence.
- **SC-004**: The Markdown briefing is under 200 lines for a project with up
  to 20 related projects.
- **SC-005**: The MCP server passes a smoke test with a real client:
  initialize, list tools, call `gitty_context`, call `gitty_read`.
- **SC-006**: No new Go module dependency; nothing written inside a repository.
- **SC-007**: `gitty` with no `gitty-mcp` present passes every test and
  `gitty mcp` exits 2 with an actionable message; with `gitty-mcp` dropped
  beside it, SC-005 passes with no configuration beyond registering
  `gitty mcp` as the server command.

## Assumptions

- The workspace is a gitty workspace; projects outside the configured instance
  are not linked.
- Links come from what is checked out. A project that is not checked out is a
  link *target* (from the API and others' references) but not a *source*
  until it is fetched — after which the next index pass scans it too.
- Extraction is textual and best-effort: gitty does not evaluate build files,
  resolve version ranges or run package managers.
- The registry prefix per project comes from the API
  (`container_registry_image_prefix`), with a `registry.` host heuristic as
  fallback.
- MCP is served over stdio only; the agent host spawns one server per
  session, in a working directory inside the workspace, by running
  `gitty mcp`.
- Fetching uses the same credentials as `sync` (`--token`, `GITLAB_TOKEN`,
  `CI_JOB_TOKEN`), resolved by `gitty` on each invocation the plugin makes;
  the plugin process never holds a token.
- Spawning `gitty` per tool call costs milliseconds and fits the 300 ms
  budget of SC-002 for a warm index; the plugin may cache the decoded index
  between calls and invalidates it by the file's modification time.
- `gitty-mcp` is released alongside `gitty` from the same tag, so the pair a
  user downloads together always agrees on the contract version.

## Out of Scope (this feature) — designed for, not built

- **A shared, remote `gitty-mcp`.** The same tools served over HTTP by one
  process that many developers' agents talk to, holding one index for the
  instance — a shared cache that gets better as more projects are indexed.
  The seam is kept open now: tools that answer from the index alone
  (`gitty_context` without fetching, `gitty_related`, `gitty_projects`) are
  separated in the plugin from tools that need a checkout on the caller's
  disk (`gitty_fetch`, `gitty_search`, `gitty_read`, and fetching inside
  `gitty_context`); results identify projects by namespace path with the
  local path as an attribute, so a remote server can answer "what" and the
  caller's machine can complete "where". Nothing is implemented beyond that
  separation.
- An HTTP transport, authentication of agents to the server, multi-workspace
  indexes and index merging belong to that later feature.

# Feature Specification: Cross-Repository Context

**Feature Branch**: `001-cross-repo-context`
**Created**: 2026-11-06
**Status**: Draft
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
an agent in the shape an agent can use.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Brief me on this project (Priority: P1)

An agent (or a human) is in a checkout inside a gitty workspace and runs
`gitty context`. It gets a concise Markdown briefing: what this project is,
which other projects in the instance it uses and how (with the file and line
that proves it), which projects use it, and for each related project its
description and local path — or, if it is not checked out, the command that
would bring it in. Any agent can read the output; Claude Code can import it
into `CLAUDE.md` with one line.

**Why this priority**: This is the whole value in one command, works with
every agent (it is just text), and needs nothing configured on the agent side.

**Independent Test**: In a workspace with two checked-out projects where one's
`go.mod` requires the other, `gitty context` run inside the first lists the
second under *Uses* with `go.mod:<line>` as evidence and its local path; run
inside the second it lists the first under *Used by*.

**Acceptance Scenarios**:

1. **Given** a workspace with projects A and B checked out, and A's manifest
   referencing B, **When** `gitty context` runs inside A, **Then** the output
   names B under "Uses" with the kind of link, the evidence location, B's
   description, and B's path relative to A.
2. **Given** the same, **When** `gitty context` runs inside B, **Then** A is
   listed under "Used by".
3. **Given** A references project C which exists upstream but is not checked
   out, **When** `gitty context` runs inside A, **Then** C is listed with its
   description and marked not checked out, with the `gitty sync` invocation
   that would fetch it.
4. **Given** A references a path that is not a project in the instance,
   **When** `gitty context` runs, **Then** that reference is not listed as a
   related project (no invented links).
5. **Given** no argument and a working directory that is a group rather than a
   checkout, **When** `gitty context` runs, **Then** it briefs the group: its
   projects, and the links between them and to the rest of the instance.

---

### User Story 2 - Index the workspace's relationships (Priority: P2)

A user runs `gitty index` at any point inside the workspace. gitty scans every
local checkout for references to other projects of the instance — package
manifests, `.gitlab-ci.yml` includes and triggers, container image references,
git submodules, documentation mentions — fetches each project's metadata from
the API (description, topics, default branch, archived, fork origin), and
writes a single index file under the workspace's `.gitty/` directory.
Re-running is fast: checkouts whose HEAD has not moved are not rescanned.
`gitty related <project>` prints the links as greppable event lines.

**Why this priority**: The index is what `context` reads; separating the slow,
scan-everything step from the instant, read-one-project step is what makes
`context` cheap enough to run on every agent start. (`context` builds or
refreshes the index itself when it is missing or stale, so US1 does not
*depend* on the user ever running `index` by hand.)

**Independent Test**: After `gitty index`, `.gitty/index.json` lists every
local checkout with its metadata and every discovered edge with its evidence;
`gitty related acme/app` prints one `related acme/app <kind> <target> <evidence>`
line per edge and a summary line; a second `gitty index` with no changes
reports every project as unchanged and finishes in well under a second for a
workspace of fifty checkouts.

**Acceptance Scenarios**:

1. **Given** a workspace of checkouts, **When** `gitty index` runs, **Then**
   stdout carries one `index <project> <changed|unchanged|new>` line per
   project, the edges found, and a summary; the index file is written.
2. **Given** an index exists and nothing changed, **When** `gitty index` runs
   again, **Then** no checkout is rescanned and the summary says so.
3. **Given** `--dry-run`, **When** `gitty index` runs, **Then** it prints what
   it would scan and write and writes nothing.
4. **Given** a project excluded by the workspace's `exclude` list, **When**
   `gitty index` runs, **Then** it is neither scanned nor linked to.

---

### User Story 3 - Serve the context to agents over MCP (Priority: P3)

A user adds gitty to their agent once (`claude mcp add gitty -- gitty mcp`).
From then on, in any checkout inside the workspace, the agent can call tools
to list projects, get a project's context, follow its links, search across all
checkouts, and read a file from another project — without the user pasting
paths or pre-generating documents.

**Why this priority**: Markdown covers the common case; MCP covers the agent
that needs to *ask follow-up questions* (search the dependents for a symbol,
read the dependency's interface) mid-task. It is a protocol surface, so it is
costlier to build and should follow the content it serves.

**Independent Test**: With `gitty mcp` running over stdio, an `initialize`
request is answered, `tools/list` names the five tools, and `tools/call
gitty_related {"project":"acme/app"}` returns the same edges `gitty related`
prints.

**Acceptance Scenarios**:

1. **Given** the server is started inside a workspace, **When** a client sends
   `initialize`, **Then** it receives the server's capabilities and name.
2. **Given** a `gitty_search` call with a pattern, **When** the pattern occurs
   in files of several checkouts, **Then** results name project, file and line
   for each match, bounded in count.
3. **Given** a `gitty_read` call for a path containing `..` or outside any
   checkout, **When** it is handled, **Then** it is refused with an error and
   nothing outside the workspace is read.

---

### User Story 4 - Bring in what this project needs (Priority: P4)

An agent sees that two of the project's dependencies are not checked out. One
command — `gitty sync --related` run inside the project — clones exactly those,
and nothing else, into their namespace paths. Re-running does nothing more.

**Why this priority**: Closes the loop from "I know B matters" to "I can read
B", but it is only useful once US1 tells the agent what is missing.

**Independent Test**: With A checked out and its dependencies B and C known
upstream but absent locally, `gitty sync --related` from inside A clones B and
C (and no unrelated sibling), prints `clone` events for each, and a second run
prints `pull` events only.

---

### User Story 5 - Wire it into the agent's instructions (Priority: P5)

A user runs `gitty context --write` inside a checkout. gitty writes the
briefing to a file in the workspace's `.gitty/context/` directory (outside any
repository) and, only because the flag asked for it, appends a single import
line to the checkout's `CLAUDE.md` so the briefing is loaded on every agent
start. Without `--write`, `context` only prints.

**Why this priority**: Convenience on top of US1. Touching a file inside a
user's repository is exactly the kind of thing the constitution says must be
explicit, so it is its own, last, opt-in step.

**Independent Test**: `gitty context --write` creates
`.gitty/context/acme/app.md` and adds one `@../../.gitty/context/acme/app.md`
line to `acme/app/CLAUDE.md`; running it again changes neither file.

---

### Edge Cases

- A manifest references a project path that was moved or deleted upstream: the
  edge is reported with the target marked unknown, not silently dropped and
  not invented as a project.
- Two projects reference each other (cycles): both directions are listed; the
  briefing never recurses.
- A project with hundreds of dependents: "Used by" is capped with a count and
  a pointer to `gitty related` for the full list.
- A checkout that is not a project of the instance (a hand-cloned repo from
  elsewhere): indexed as a local-only entry, never linked as a target.
- Archived projects: linked to and briefed when they are the target of an edge
  (they still exist), but not scanned as sources unless `--archived`.
- A huge checkout (monorepo): scanning is bounded by file-name patterns and a
  size cap; a slow scan is reported per project, not hung on.
- No index and no network: `context` says it cannot build the index and exits
  2; it never prints a briefing built from nothing.
- The index was built by an older gitty: a version field makes it rebuilt, not
  misread.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `gitty context [project]` MUST print a Markdown briefing for the
  named project, or for the project/group of the current directory when
  omitted, resolved like `ls` resolves its argument.
- **FR-002**: The briefing MUST list outgoing links ("Uses") and incoming links
  ("Used by") separately, each entry carrying the link kind, the evidence
  (`file:line`), and the target's description and local path or absence.
- **FR-003**: `gitty index` MUST discover links from, at minimum: package
  manifests that name a project of the instance by module path or URL;
  `.gitlab-ci.yml` `include: project:`, `trigger: project:` and
  `include: component:` references; container image references to the
  instance's registry; `.gitmodules` URLs; and the API's fork origin.
  [NEEDS CLARIFICATION: which package ecosystems matter first — Go, npm,
  Python, Ruby, Java, Rust?]
- **FR-004**: Every link MUST carry evidence (file and line, or the API field)
  and a kind from a fixed set: `depends`, `ci-include`, `ci-trigger`,
  `ci-component`, `image`, `submodule`, `fork`, `mentions`.
- **FR-005**: A reference MUST only become a link when its target resolves to
  a project of the instance (by namespace path); unresolved references are
  reported as such and never fabricated.
- **FR-006**: The index MUST live in the workspace's `.gitty/` directory, MUST
  carry a schema version, and MUST contain no token or credential.
- **FR-007**: `gitty index` MUST be incremental: a checkout whose HEAD commit
  is unchanged since the last index MUST NOT be rescanned.
- **FR-008**: `gitty index` MUST honour `--dry-run`, the workspace's `exclude`
  list and `--exclude`, and MUST emit one greppable event line per project and
  per link.
- **FR-009**: `gitty related <project>` MUST print links as
  `related <project> <kind> <target> <evidence>` lines (and `related-by` for
  incoming), closing with a summary line.
- **FR-010**: `gitty mcp` MUST serve MCP over stdio using only the standard
  library, exposing `gitty_projects`, `gitty_context`, `gitty_related`,
  `gitty_search` and `gitty_read`; `gitty_read` and `gitty_search` MUST be
  confined to local checkouts of the workspace.
- **FR-011**: `gitty sync --related` MUST clone only projects that are link
  targets (or sources, with `--related=both`) of the current project and not
  yet checked out, through the existing sync path, with its dry-run parity.
- **FR-012**: `gitty context --write` MUST write the briefing under
  `.gitty/context/` and append one import line to the checkout's `CLAUDE.md`
  (creating it if absent); without `--write` nothing is written anywhere.
  [NEEDS CLARIFICATION: also `AGENTS.md`, or only `CLAUDE.md`?]
- **FR-013**: All commands MUST work from any directory inside the workspace,
  via the existing workspace discovery.
- **FR-014**: `context` MUST build or refresh the index when it is missing or
  when the current project's HEAD has moved since it was indexed, so the
  command is correct without the user running `index` first.

### Key Entities

- **Project**: a GitLab project of the instance — namespace path, id,
  description, default branch, topics, archived, fork origin, container
  registry prefix; plus, when checked out, its local directory, HEAD at last
  scan, and the documentation files it carries (README, CLAUDE.md, AGENTS.md).
- **Link**: a directed edge from one project to another — kind, evidence
  (file:line or API field), discovered-at index run.
- **Index**: the set of projects and links for one workspace, with schema
  version, instance URL and generation time.
- **Briefing**: the rendered, per-project view of the index: the project, its
  outgoing and incoming links, and the related projects' descriptions and
  locations.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a workspace where project A's manifest references project B,
  an agent that runs `gitty context` inside A can name B, say why it matters
  (the link kind and evidence) and open B's local directory, with no other
  input.
- **SC-002**: A cold `gitty index` of a 50-checkout workspace completes in
  under 30 seconds on a laptop; a warm one with no changes completes in under
  one second.
- **SC-003**: Every link in the index can be traced to a file and line (or API
  field) that a reviewer can open and confirm — zero links without evidence.
- **SC-004**: `gitty context` output is under 200 lines for a project with up
  to 20 related projects, so it fits comfortably in an agent's instruction
  budget.
- **SC-005**: The MCP server passes a conformance smoke test with a real
  client (`claude mcp` or the reference inspector): initialize, list, call.
- **SC-006**: No new Go module dependency.

## Assumptions

- The workspace is a gitty workspace (initialised with `gitty init`); projects
  outside the configured instance are not linked.
- Links are discovered from what is checked out locally. A project that is not
  checked out appears as a *target* (from the API and from others' references)
  but is not scanned as a *source* until it is synced.
- Evidence parsing is textual and best-effort: gitty does not evaluate build
  files, resolve version ranges or run package managers. A reference to the
  instance's host or a known project path is a link; anything subtler is not.
- The instance's container registry prefix is taken from the API
  (`container_registry_image_prefix`), falling back to a `registry.` host
  heuristic.
- Claude Code resolves `@path` imports in `CLAUDE.md` relative to the
  importing file, and asks once before importing from outside the working
  directory; the written import line follows that convention.
- MCP is served over stdio only; no network listener.

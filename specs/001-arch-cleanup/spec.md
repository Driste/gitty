# Feature Specification: Architecture Cleanup

**Feature Branch**: `001-arch-cleanup`
**Created**: 2026-05-09
**Status**: Draft
**Input**: User description: "Clean up the current arch to make it clearer"

## User Scenarios & Testing *(mandatory)*

The "users" of this feature are **Gitty contributors** (the maintainer and any future
collaborators) who read, extend, and debug the codebase. Each user story is a
journey through the code that the cleanup must make demonstrably faster and
less error-prone, without changing what end-users of the `gitty` CLI experience.

### User Story 1 - Locate the source of any user-visible behavior fast (Priority: P1)

A contributor sees a behavior in the CLI (e.g., "why does sync skip this group?"
or "where is the token resolved?") and needs to find the code that produces it.

**Why this priority**: This is the day-one productivity blocker. Every other
maintenance task (bug fixes, feature additions, reviews) starts with "find where
this lives." Today the entire program is in package `main` with mixed
responsibilities in [sync.go](sync.go), so finding the right place requires
reading the whole file.

**Independent Test**: Hand a new contributor a list of 5 user-visible behaviors
(token resolution order, dry-run handling, group fetch, repo clone vs pull
choice, config load) and ask them to point to the file/function responsible.
Success = all 5 located in under 5 minutes without grep across the whole repo.

**Acceptance Scenarios**:

1. **Given** the cleaned-up codebase, **When** a contributor opens the project
   for the first time and looks at the top-level layout, **Then** the names of
   files or directories make it obvious which one owns CLI parsing, config,
   GitLab data access, filesystem layout, git invocation, and sync orchestration.
2. **Given** a user-visible behavior to investigate, **When** the contributor
   searches for the responsible code, **Then** that behavior is implemented in
   exactly one place (no duplicated token-resolution logic, no duplicated
   pagination loops, no duplicated path-building).
3. **Given** the cleaned-up codebase, **When** the contributor runs the same
   `gitty init` and `gitty sync` invocations they ran before the cleanup,
   **Then** the on-disk layout and observable outcomes are identical (same
   directories created, same repos cloned/pulled, same exit codes).

---

### User Story 2 - Replace or test a single collaborator without rewriting the rest (Priority: P1)

A contributor wants to add unit tests for the path-resolution logic, mock the
GitLab API to write integration tests, or swap how `git` is invoked (e.g., to
add a future async pull). Today these concerns are entangled: `runSync` loads
config, resolves tokens, builds the GitLab client, paginates results, builds
filesystem paths, and shells out to `git` — all in one function.

**Why this priority**: The Constitution's testability and minimal-dependency
principles require that boundaries exist; currently they do not. This is
prerequisite work for nearly every TODO in the README (async pulls, branch
freshness display, add/remove diff).

**Independent Test**: A contributor MUST be able to call the path-resolution
logic and the config load/save logic from a Go test file with no network access
and no git binary on PATH. The GitLab data-fetching surface MUST be replaceable
by a fake in tests.

**Acceptance Scenarios**:

1. **Given** the cleaned-up codebase, **When** a contributor writes a test for
   the path-from-namespace logic, **Then** they can do so without importing
   the GitLab client, without touching the filesystem, and without invoking git.
2. **Given** the cleaned-up codebase, **When** a contributor wants to substitute
   a fake GitLab data source, **Then** the sync orchestration depends on a
   narrow interface they can implement, not on a concrete GitLab client.
3. **Given** the cleaned-up codebase, **When** a contributor wants to record or
   intercept git invocations, **Then** git execution is wrapped in a single
   replaceable surface used by all callers.

---

### User Story 3 - Trust the documented config contract (Priority: P2)

A user reads the README, runs `gitty init`, and expects to see the file the
README documents. Today the README says a `gitty.toml` file is created, but the
code writes `.gitty/config`. A contributor reading the docs cannot tell which
is authoritative.

**Why this priority**: The Constitution names `gitty.toml` while the README
says the same and the code disagrees with both. This inconsistency undermines
the workspace contract and is a small, contained fix that belongs with the
cleanup. **Resolution**: the canonical layout is `.gitty/config` (the current
code); the README MUST be rewritten and the Constitution MUST be amended
(separate `/speckit-constitution` change) to match. Keeping the `.gitty/`
directory leaves room for future per-workspace state (caches, etc.) without a
second migration.

**Independent Test**: After cleanup, running `gitty init` produces
`.gitty/config`, `gitty sync` finds it, and the README and Constitution both
reference `.gitty/config` (no `gitty.toml` mentions remain anywhere except in
historical changelog/release notes).

**Acceptance Scenarios**:

1. **Given** a fresh directory, **When** the user runs `gitty init`, **Then**
   the resulting workspace contains a `.gitty/config` file (matching the
   updated README).
2. **Given** a workspace with `.gitty/config`, **When** the user runs
   `gitty sync`, **Then** the workspace is discovered and used.
3. **Given** the README, the code, and the Constitution, **When** any one of
   them references the workspace config, **Then** all three say `.gitty/config`.

---

### Edge Cases

- A contributor running the cleaned-up code on an existing workspace created
  with the previous config layout MUST get a clear, actionable error message
  if the layout is no longer recognized — not a silent "workspace not found."
- The cleanup MUST NOT change the resolved on-disk layout for any existing
  GitLab namespace path, so re-running sync against a previously-synced
  workspace MUST be a no-op (per Constitution Principle I, idempotency).
- If the cleanup splits source into multiple files or packages, `go build` and
  `go vet` MUST continue to pass with the existing dependency set; no new
  external Go modules MAY be added (per Constitution Principle II).
- The `gitty` binary built from the cleaned-up source MUST accept the same
  flags with the same defaults as today; no flag MAY be renamed, removed, or
  given a new default.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The codebase MUST separate the following responsibilities into
  distinctly-named source units (files or packages): (a) CLI surface and flag
  parsing, (b) workspace configuration persistence, (c) GitLab data access,
  (d) local filesystem layout/path resolution, (e) git command invocation,
  (f) sync orchestration that composes the above.
- **FR-002**: Each responsibility unit listed in FR-001 MUST be importable and
  exercisable from a Go test file without requiring the others to be present
  at runtime (e.g., path resolution callable without a GitLab client; config
  load/save callable without git on PATH).
- **FR-003**: GitLab data access MUST be exposed to sync orchestration through
  a narrow interface (covering at minimum: list immediate subgroups, list
  descendant subgroups, get group, list group projects with optional
  recursion) so that a fake implementation can be substituted in tests.
- **FR-004**: Token resolution (explicit flag → `GITLAB_TOKEN` →
  `CI_JOB_TOKEN`) MUST live in exactly one function, called once per
  invocation, and MUST be unit-testable in isolation.
- **FR-005**: Path resolution from a GitLab namespace path to a local
  destination MUST live in exactly one function and MUST be unit-testable in
  isolation; behavior MUST match the current implementation for all inputs
  the current implementation accepts.
- **FR-006**: Pagination over GitLab list endpoints MUST be expressed once
  (not duplicated per endpoint), with each list operation supplying only the
  endpoint-specific fetch logic.
- **FR-007**: Git execution MUST be wrapped behind a single function/type
  used by all sync code paths, preserving today's environment-passthrough
  behavior (SSH agent, global gitconfig).
- **FR-008**: Public CLI behavior MUST NOT change. The set of subcommands,
  flag names, flag defaults, exit codes, and the on-disk layout produced by
  any given invocation MUST be identical before and after the cleanup.
- **FR-009**: The canonical workspace config layout is `.gitty/config` (TOML).
  The README MUST be updated to match, removing all `gitty.toml` references.
  The Constitution MUST be amended in a separate `/speckit-constitution`
  change to reference `.gitty/config` in Principle III; that amendment is a
  pre-merge dependency of this feature.
- **FR-010**: Output streams MUST be separated per Constitution Principle I
  for the streams currently emitted by the program: planning lines (the
  "would create / would clone / would pull" output of `--dry-run`) MUST go
  to stdout; all progress, status, and error messages MUST go to stderr.
  This requirement is scoped to stream destinations only; introducing a
  `--verbose` flag, stable per-event-class line prefixes, ANSI handling, and
  credential redaction (the rest of Constitution Principle II's output
  contract) is OUT of scope and belongs in a follow-up spec.
- **FR-011**: The cleanup MUST land with one seed `*_test.go` test per
  responsibility unit defined in FR-001 (target: ~6 tests total). Each seed
  test MUST exercise its unit through its public surface only, prove the
  unit is callable in isolation (no GitLab network for path/config/orchestration
  tests; no `git` binary required for path/config/GitLab tests), and serve
  as the executable example a future contributor copies when adding more
  tests. Exhaustive test coverage is OUT of scope for this feature.
- **FR-012**: No new external Go module dependencies MAY be added by this
  cleanup. The dependency set in `go.mod` MUST be unchanged or smaller.
- **FR-013**: A README section or top-of-file comment MUST briefly describe
  the new layout, so a first-time contributor learns the structure without
  having to grep through every file.

### Key Entities

- **Workspace Config**: The on-disk file that anchors a `gitty` workspace
  (name resolved by FR-009). Holds the GitLab base URL, the SSH-vs-HTTP
  preference, and the namespace root path the directory is bound to.
- **GitLab Namespace**: A group or subgroup path on GitLab (e.g.,
  `tenant/images/backend`). Maps 1:1 to a local directory path under the
  workspace root.
- **GitLab Project**: A repository under a namespace. Has SSH and HTTP clone
  URLs; the active workspace config decides which is used.
- **Sync Plan**: The set of (namespace → local directory) and
  (project → clone-or-pull action) decisions a single sync invocation will
  carry out. Today implicit in the imperative loop; a clearer architecture
  may make this an explicit value that the dry-run path and the apply path
  share.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A contributor unfamiliar with the codebase can correctly point
  to the file responsible for each of {CLI parsing, config persistence,
  GitLab API calls, filesystem path building, git invocation, sync
  orchestration} in under 5 minutes, by reading the top-level layout alone.
- **SC-002**: 100% of the user-visible behaviors enumerated in User Story 1
  are implemented in exactly one location in the cleaned-up code (no
  duplicated token resolution, no duplicated pagination, no duplicated path
  building).
- **SC-003**: Re-running every example invocation in the README against the
  cleaned-up binary produces byte-identical on-disk results (same
  directories, same clones, same idempotent re-runs) compared to running the
  same invocations against the pre-cleanup binary.
- **SC-004**: The dependency count in `go.mod` is the same as or smaller
  than before the cleanup; `go build ./...` and `go vet ./...` pass.
- **SC-005**: Path resolution and config persistence can each be invoked
  from a Go test file with zero network calls and no `git` binary on PATH;
  attempting to do this in the pre-cleanup code requires running the full
  `runSync` path.
- **SC-006**: README, code, and Constitution all reference the workspace
  config as `.gitty/config`; zero `gitty.toml` mentions remain outside of
  historical changelog/release-note files.
- **SC-007**: Running any sync-class invocation with `2>/dev/null` produces
  an empty stderr stream only on a fully no-op sync; planning-only invocations
  (`--dry-run`) write their plan exclusively to stdout (verifiable by
  redirecting stdout to a file and checking it contains the plan lines while
  stderr contains no plan lines).
- **SC-008**: The repository contains at least one `*_test.go` file per
  responsibility unit from FR-001, and `go test ./...` passes from a clean
  checkout with no network access and no `git` binary on PATH.

## Assumptions

- The cleanup is a refactor, not a feature. CLI surface and on-disk behavior
  are preserved; new flags or new sync behaviors are out of scope and belong
  in separate specs (e.g., async pulls, branch-freshness display, add/remove
  diff — all listed in the README TODO).
- "Clearer architecture" means *navigability and separation of concerns*, not
  a particular target structure. Whether the result is a single `main`
  package with multiple files or a small set of internal subpackages is a
  design decision for the plan phase, as long as it satisfies FR-001 through
  FR-007 and SC-001 through SC-005.
- The maintainer is the only current user of `gitty`. The Q1 resolution
  keeps the on-disk config layout (`.gitty/config`) the same as today, so no
  user-facing migration is required; the visible change for users is
  documentation only.
- The Q1 resolution implies a Constitution amendment (Principle III currently
  names `gitty.toml`). That amendment is a small wording change and is a
  pre-merge dependency of this feature, handled via a separate
  `/speckit-constitution` invocation.
- The Q2 resolution scopes output changes to *stream destinations only*. The
  rest of Constitution Principle II's output contract (`--verbose` flag,
  per-event-class prefixes, ANSI handling, credential redaction) is
  deliberately deferred to a follow-up spec to keep this PR's blast radius
  small. Note that this means the cleanup PR will leave Gitty Constitution-
  compliant on Principle I but only partially compliant on Principle II.
- The cleanup is a single atomic change conceptually; it MAY land as one or
  several PRs, but every intermediate commit MUST leave `go build`,
  `go vet`, and `go test ./...` passing and CLI behavior identical.

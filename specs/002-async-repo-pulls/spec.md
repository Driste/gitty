# Feature Specification: Async Repo Pulls with Concurrency Limit

**Feature Branch**: `002-async-repo-pulls`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Async repo pulls with a configurable concurrency limit"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Sync a large group faster on a fast pipe (Priority: P1)

A user with a fast connection runs `gitty sync` against a GitLab group containing
dozens or hundreds of repositories. Today every clone/pull runs strictly in
series, so total wall-clock time scales linearly with repo count. The user
wants to run several clones/pulls concurrently and finish sooner, without
manually wrapping `gitty sync` in shell parallelism.

**Why this priority**: The README's `TODO` already calls this out and it is
the single most-requested ergonomic improvement for users syncing large
groups. It's also the gating change for "use gitty in a CI job that has a
short time budget."

**Independent Test**: Run `gitty sync --path=<group> --jobs=4` against a
group with at least 8 missing repos and measure total wall-clock time.
Compare to the same invocation with `--jobs=1`. The parallel run completes
in noticeably less wall-clock time (target: ≥40% reduction at `--jobs=4` for
a network-bound workload). The final on-disk tree is byte-identical between
the two runs.

**Acceptance Scenarios**:

1. **Given** a workspace with `N` un-cloned repos and `--jobs=K` (K > 1),
   **When** `gitty sync` runs, **Then** up to K `git clone` operations are
   in flight at any moment, the operation completes faster than the same
   sync with `--jobs=1`, and the resulting directory tree is identical.
2. **Given** any value of `--jobs` (including 1), **When** the sync completes
   successfully, **Then** the local directory tree and the set of files in
   each cloned repository are identical to what `--jobs=1` would have
   produced. Re-running the same command is a no-op (idempotency preserved
   per Constitution Principle I).
3. **Given** `--dry-run` and any value of `--jobs`, **When** the command
   runs, **Then** no network calls or git invocations are made and the plan
   output is produced in deterministic, repo-order; `--jobs` MUST NOT change
   `--dry-run` output.

---

### User Story 2 - See per-repo progress and outcomes clearly when jobs interleave (Priority: P1)

A user running with concurrency > 1 needs to be able to tell which line came
from which repo, scroll back to find a specific repo's outcome, and (in CI)
parse the output without races between worker streams. Today's output
("Processing X...", "Cloning X", "Git execution error: …") is correlated only
by line proximity, which breaks under interleaving.

**Why this priority**: Async pulls without identifiable per-repo output is a
regression compared to the serial mode — users would lose the ability to
diagnose failures. P1 because it ships in the same release as US1.

**Independent Test**: Run `gitty sync --jobs=4` against a small set of repos
and redirect stderr to a file. The file is grep-able by repo path: a single
`grep tenant/api stderr.log` returns every event for that repo (start,
clone/pull line, completion or error) and only those events.

**Acceptance Scenarios**:

1. **Given** a sync run with `--jobs>1`, **When** any progress line is
   written for a particular repo, **Then** the line MUST identify that repo
   unambiguously by its full namespace path (e.g., `tenant/images/api`),
   such that `grep '<path>'` returns the complete event sequence for that
   repo and nothing else.
2. **Given** a sync where one repo errors mid-clone and others succeed,
   **When** the run completes, **Then** the error line for the failing repo
   names the repo path, the failed git invocation is recoverable from the
   log, and successful repos' output is not contaminated with the failure's
   text.
3. **Given** any concurrent run, **When** any line is written by gitty
   itself (not by `git`), **Then** the line MUST be a single complete line
   (no partial writes that could split mid-line under interleaving). Lines
   produced by `git` itself are passed through and not subject to this rule.

---

### User Story 3 - Stop a runaway sync cleanly (Priority: P2)

A user presses Ctrl-C during a parallel sync. They want the process to stop
quickly, leave a recoverable workspace, and not orphan in-flight `git`
processes. Constitution Principle I + Additional Constraints (SIGINT) already
require this for the serial case; the spec must keep that contract under
concurrency.

**Why this priority**: Required for parity with serial behavior. Lower than
P1 because it's a safety net rather than the primary value driver — but it
MUST ship with the feature, not later.

**Independent Test**: Start `gitty sync --jobs=4` against a slow target,
send SIGINT after ~1 second, observe: (a) the gitty process exits within ~2
seconds, (b) every in-flight `git` child process has exited, (c) any
half-cloned directory is detectable on a follow-up run (the directory either
contains a complete repo or contains nothing surprising — no orphaned `.git`
locks from a killed gitty parent).

**Acceptance Scenarios**:

1. **Given** a sync with `--jobs>1` in progress, **When** the user sends
   SIGINT, **Then** gitty cancels not-yet-started repos, requests its
   in-flight git child processes to stop, waits for them to exit (bounded
   by a short grace period), and exits with a non-zero code that signals
   interruption.
2. **Given** a sync was interrupted, **When** the user re-runs the same
   command, **Then** repos that were already fully cloned are picked up by
   `git pull` and partial clones are detected and reported (a clear stderr
   line) rather than silently producing an inconsistent workspace.

---

### Edge Cases

- **`--jobs=0` and negative values**: Invalid input MUST be rejected with a
  clear error message naming the flag and the allowed range. Exit code 2
  (flag parse error).
- **`--jobs=1`**: MUST be a supported value and MUST behave identically to
  the pre-feature serial mode (no concurrency overhead, ordered output).
  This is the rollback path if anything misbehaves at higher concurrencies.
- **Very high `--jobs` (e.g., 1000)**: The system MUST cap effective
  concurrency at a sane upper bound (suggested: max 64) and emit a single
  stderr warning line that the value was clamped. Allowing arbitrary
  concurrency invites fork-bombing the user's git auth (SSH agent, HTTPS
  credential helper) and the remote.
- **All repos already cloned**: With `--jobs>1`, the work is N `git pull`
  invocations in parallel. Same correctness rules as cloning.
- **One repo's clone URL is empty**: Already a skip-with-warning case
  (defense from the previous feature). MUST remain a skip — never crash a
  worker, never deadlock the worker pool.
- **`--groups` mode (mkdir + write tiny TOML)**: Per-group work is
  filesystem-local, sub-millisecond, and not worth parallelizing. MUST
  remain serial regardless of `--jobs`.
- **Git asks for credentials interactively**: With multiple workers, an
  interactive password prompt from `git` would be unreadable. The contract
  is unchanged from today: gitty does NOT manage credentials, it relies on
  SSH agent or a configured credential helper. If git blocks on a prompt
  the worker simply waits — the user is expected to have non-interactive
  credentials set up. No new requirement, but called out so it isn't
  forgotten.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The maximum number of concurrent repository operations
  (clone or pull) in flight at any moment is controlled by a new `jobs`
  field in `.gitty/config` (integer, default `4`). `gitty sync` MUST
  accept a `--jobs N` CLI flag that overrides the config value for a
  single invocation. The override MUST NOT persist back to
  `.gitty/config` (per Constitution Principle III: flags override config
  but do not silently mutate it).
- **FR-001a**: `gitty init` MUST write `jobs = 4` into the newly-created
  `.gitty/config` so that the default is visible and editable by the user
  in one place (no hidden defaults).
- **FR-001b**: When `gitty sync` reads a `.gitty/config` produced by a
  pre-feature `gitty init` (no `jobs` field present), the missing field
  MUST be interpreted as `jobs = 4`. No migration is required; the user
  may add `jobs = N` to their existing config at any time.
- **FR-002**: `--jobs=1` MUST behave identically to the pre-feature serial
  mode: ordered output, no worker-pool overhead in observable output,
  same stream destinations.
- **FR-003**: `--jobs N` MUST clamp `N` to the range [1, 64] inclusive.
  Values ≤0 MUST be rejected at flag parse time (exit code 2) with a
  clear error message; values >64 MUST be clamped to 64 with a single
  stderr warning line per FR-010.
- **FR-004**: Concurrency MUST apply ONLY to repo clone/pull operations
  inside the repository-sync stage. Group materialization (`--groups`)
  MUST remain serial because each step is sub-millisecond filesystem
  work that does not benefit from parallelism.
- **FR-005**: With `--jobs>1`, every gitty-emitted stderr line about a
  specific repo MUST name that repo by its full namespace path
  unambiguously (e.g., `[tenant/images/api] cloning git@host:…`), so that
  `grep '<path>' stderr.log` returns the complete event stream for one
  repo and only that repo. Lines from `git` itself (clone progress,
  warnings) are passed through unmodified.
- **FR-006**: With `--jobs>1`, every gitty-emitted line MUST be written
  atomically (one complete line per write call, with a write mutex around
  shared stderr) so worker lines never split or interleave mid-line.
  Lines from `git` subprocesses MUST be handled as follows:
  - **stdout from `git`**: suppressed entirely. Gitty does not forward
    git's stdout to the user under any concurrency setting (including
    `--jobs=1`, for consistency).
  - **stderr from `git`**: captured per worker and held in an in-process
    buffer. If the git invocation exits with a non-zero status, the
    captured buffer is flushed to gitty's stderr as one atomic block
    after gitty's own `[<repo>] error: …` line, so the user can see what
    git complained about. On success, the buffer is discarded.
  - Result: under any `--jobs` value, gitty's stderr is a clean stream of
    gitty's own per-repo lines, with git's verbose output appearing only
    when a repo actually fails.
- **FR-007**: Failure semantics MUST preserve today's behavior under any
  `--jobs` value: on a per-repo failure, gitty emits an error line for
  that repo (with the captured git stderr per FR-006), continues with
  the remaining repos in the run, and exits with code `0` even when one
  or more repos failed. CI users who need a non-zero exit on partial
  failure can grep the stderr for `error:` lines until a future spec
  changes the exit-code contract.
- **FR-008**: `--dry-run` output MUST be identical regardless of `--jobs`
  value. Planning lines MUST emit in stable repo-order (the same order
  the GitLab API returns them today), so `gitty sync --dry-run` is
  reproducible.
- **FR-009**: On SIGINT (Ctrl-C) during any `--jobs` run, gitty MUST:
  (a) stop dispatching new repos to workers, (b) propagate the signal to
  in-flight `git` child processes so they can clean up, (c) wait for
  in-flight workers to return (bounded by a short grace period the user
  should not have to think about), and (d) exit with a non-zero code that
  signals interruption (distinct from "all repos succeeded" and from
  "some repos failed").
- **FR-010**: When the user passes a `--jobs` value greater than 64, gitty
  MUST emit exactly one stderr line of the form
  `--jobs <N> clamped to 64` (where `<N>` is the original value) and
  proceed with effective concurrency 64.
- **FR-011**: The `--jobs` flag MUST appear in `gitty sync -h` help text
  with its default value and documented range `[1, 64]`.
- **FR-012**: No new external Go module dependencies. Concurrency MUST be
  implemented with standard-library primitives (goroutines, channels,
  `sync`, `context`). Constitution Principle II compliance.
- **FR-013**: Token resolution, GitLab listing calls, and config load MUST
  remain single-threaded — only the per-project clone/pull stage runs in
  parallel. This keeps token-resolution order, config invariants, and
  API-call ordering untouched.

### Key Entities

- **Workspace Config** (extended): the existing `.gitty/config` gains one
  new field `jobs` (integer, default `4` when written by `gitty init`,
  default `4` when absent from an older config). All other fields
  (`url`, `http`, `root_path`) are unchanged.
- **Job**: A single repo operation (clone or pull) issued to one worker.
  Carries one project's namespace path, target local directory, and
  resolved clone URL.
- **Worker Pool**: A bounded set of in-flight Jobs. Size is the resolved
  jobs value: explicit `--jobs` flag if present, else `Config.jobs`, else
  `4`. After validation and clamping per FR-003.
- **Repo Event**: A single stderr line emitted by gitty describing a Job's
  state transition (start, complete, skip, error). Always atomically
  written; under any `--jobs>1` it is prefixed with the repo's namespace
  path per FR-005. On error, the captured git stderr buffer is flushed
  immediately after the gitty error line.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For a workspace with ≥8 missing repos against a typical
  GitLab instance, `gitty sync --jobs=4` completes in ≤60% of the
  wall-clock time of `gitty sync --jobs=1` (≥40% reduction).
- **SC-002**: For any `--jobs` value, the final on-disk directory tree
  is byte-identical to the tree produced by `--jobs=1` for the same
  target. Verifiable with `diff -r`.
- **SC-003**: For any `--jobs` value, re-running the same command on the
  resulting workspace is a no-op for cloning (only `git pull` runs)
  — idempotency per Constitution Principle I.
- **SC-004**: For a sync run with `--jobs=4` and ≥4 repos, redirecting
  stderr to a file and running `grep '<repo-path>' stderr.log` for any
  repo MUST return that repo's complete event sequence (start + one of
  clone/pull/skip + outcome) and no events from other repos.
- **SC-005**: SIGINT during a `--jobs=4` sync against a slow target
  causes the gitty process to exit in ≤2 seconds. No orphaned git child
  processes remain after exit.
- **SC-006**: `gitty sync --dry-run --jobs=4` produces byte-identical
  output to `gitty sync --dry-run --jobs=1` for the same target.
- **SC-007**: `go.mod` and `go.sum` are unchanged by this feature.
- **SC-008**: A new unit test can exercise the worker pool with a fake
  git runner — no network, no real git binary — in under 10 lines of
  test setup.
- **SC-009**: `gitty init` in a fresh directory produces a
  `.gitty/config` whose content contains the line `jobs = 4` (or
  equivalent TOML rendering). A user can edit that line to change the
  default for that workspace without passing `--jobs` on every command.
- **SC-010**: A `.gitty/config` written by the pre-feature binary (no
  `jobs` field) is read successfully by the new binary; `gitty sync`
  treats the missing field as `jobs = 4` with no warning required.

## Assumptions

- The feature is additive: a new `jobs` field in `.gitty/config` and a
  new `--jobs` CLI flag. Existing config files without `jobs` continue
  to work; the effective default is `4` whether the field is missing
  (older configs) or present (new `gitty init` output).
- The default jump from "implicit serial" to "4 concurrent" is a small
  observable behavior change at the default. Justified because (a) the
  user must explicitly accept it by upgrading the binary, (b) `--jobs=1`
  remains a one-flag rollback, and (c) the value lives in the config so
  the user sees and can edit it.
- Suppressing git's own stdout (FR-006) is consistent across `--jobs=1`
  and `--jobs>1` so users do not see different verbosity at different
  concurrency levels.
- Exit code on partial failure remains `0` per FR-007. A future spec
  can introduce a non-zero exit (a breaking change requiring a MAJOR
  Constitution version bump for the CLI contract).
- The user's authentication is non-interactive (SSH agent or a configured
  credential helper). If git blocks on a credential prompt under
  concurrency, that is the user's environment, not a gitty bug.
- The 64 upper bound (FR-003 / FR-010) is a conservative cap chosen to
  protect SSH/HTTPS credential infrastructure. Most users will not
  approach it; it exists so that `--jobs=99999` does not produce a
  denial-of-service against the user's own ssh-agent.
- Per-group filesystem work (`--groups` mode) is not parallelized because
  it is sub-millisecond per group and parallelism would add output
  complexity without measurable speedup.
- The feature only changes behavior of `gitty sync`; `gitty init` is
  unaffected.
- The architecture cleanup feature (001-arch-cleanup) is a prerequisite
  that has already shipped. This spec relies on the existing
  `gitexec.Runner` interface to fake the git binary in tests (SC-008).

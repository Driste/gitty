# Implementation Plan: Async Repo Pulls with Concurrency Limit

**Branch**: `002-async-repo-pulls` | **Date**: 2026-05-11 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/002-async-repo-pulls/spec.md`

## Summary

Run repository clone/pull operations concurrently with a bounded worker pool.
Concurrency is configurable per-workspace in `.gitty/config` (`jobs`, default
`4`) and per-invocation via a new `--jobs N` flag. Token resolution, GitLab
listing, and `--groups` materialization stay serial; only the per-project
clone/pull stage parallelizes. Git's stdout is suppressed across all jobs
values; git's stderr is captured per worker and flushed to gitty's stderr
only if the git invocation fails. Every gitty-emitted stderr line about a
specific repo is prefixed `[<namespace/path>]` whenever the effective jobs
value is > 1, so `grep '<path>'` returns one repo's complete event sequence.
SIGINT cancels not-yet-started repos, propagates to in-flight git children,
and exits non-zero. `--dry-run` output is byte-identical regardless of
`--jobs`. No new Go module dependencies; concurrency uses standard-library
primitives (goroutines, channels, `sync`, `context`).

## Technical Context

**Language/Version**: Go 1.24.4 (per `go.mod`)
**Primary Dependencies**: `github.com/pelletier/go-toml/v2` v2.3.0,
`gitlab.com/gitlab-org/api/client-go` v1.46.0 — both unchanged.
**Storage**: Local filesystem. `<workspace>/.gitty/config` (TOML) gains one
new optional field `jobs`.
**Testing**: Go standard `testing` package. New tests cover: worker-pool
fan-out with a fake `gitexec.Runner`, git-stderr-on-error capture, SIGINT
cancellation, atomic per-line stderr writes, jobs-clamping at the flag
parser, and config-default-on-missing-field.
**Target Platform**: macOS and Linux on `amd64` and `arm64` (Constitution
§ Additional Constraints). Windows best-effort.
**Project Type**: cli (single binary, no library consumers).
**Performance Goals**:

- SC-001: `--jobs=4` completes in ≤60% of `--jobs=1` wall-clock time on a
  workspace with ≥8 missing repos against a typical GitLab instance.
- No goal for `--jobs=1` performance; that path is allowed to retain
  today's overhead.

**Constraints**:

- FR-012, SC-007: zero new external Go module dependencies.
- FR-008, SC-006: `--dry-run` output byte-identical regardless of `--jobs`.
- FR-002: `--jobs=1` observable behavior matches today's serial mode for the
  output channels gitty controls (note: git's own stdout is now suppressed
  even at `--jobs=1` per FR-006, a deliberate small behavior change).
- FR-007: process exit code unchanged on partial failure (still `0`).
- FR-013: token resolution and GitLab listing remain single-threaded.
- Constitution Principle I: SIGINT cleanup contract preserved under
  concurrency (FR-009).

**Scale/Scope**: ~70 LOC of new Go in `internal/sync` (worker pool +
signal wiring), ~15 LOC of changes in `internal/gitexec` (Runner contract
extension), ~10 LOC in `internal/config` (new field + default), ~10 LOC in
`internal/cli/sync_cmd.go` (flag + clamp), plus ~120 LOC of new tests.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Applies? | This plan's posture | Verdict |
| --- | --- | --- | --- |
| **I. CLI-First UX & Safe-by-Default Sync** | Yes | `--jobs` is a non-breaking ADD. Existing flags/defaults/exit codes preserved. `--dry-run` parity strengthened (SC-006). SIGINT contract preserved under concurrency (FR-009, SC-005). One small behavior change at the default: serial → 4 concurrent. Justified in Complexity Tracking. | ✅ Pass |
| **II. Minimal Dependencies & Structured Observability** | Yes | FR-012 forbids new modules. FR-005 introduces a `[<namespace/path>] ` prefix on gitty's per-repo lines under `--jobs>1` — a partial advance toward the full Principle II output contract. FR-006 makes gitty's lines atomic. The deferred full output contract (universal prefixes, ANSI, redaction, `--verbose`) remains out of scope. | ✅ Pass (modest improvement) |
| **III. Config-Anchored Workspaces** | Yes | The new `jobs` field lives in `.gitty/config`. `--jobs` flag overrides for one invocation only, never persisted back (FR-001). On-disk directory layout unchanged. Token resolution order unchanged (FR-013). | ✅ Pass |

**Initial verdict (pre-design):** Pass with one tracked behavior-default
change (serial → concurrent at default). Re-checked post-Phase 1 below —
still passes.

## Project Structure

### Documentation (this feature)

```text
specs/002-async-repo-pulls/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── cli.md           # `--jobs` flag schema (delta over 001's cli.md)
│   └── workspace.md     # `.gitty/config` jobs field (delta over 001's workspace.md)
├── checklists/
│   └── requirements.md  # Spec quality checklist (already created)
├── spec.md              # Feature specification
└── tasks.md             # Phase 2 output (created by /speckit-tasks)
```

### Source Code (repository root) — delta over the post-001 layout

```text
gitty/
├── main.go                          # Unchanged
└── internal/
    ├── cli/
    │   └── sync_cmd.go              # MODIFY: add --jobs flag + clamp + pass into sync.Request
    ├── config/
    │   └── config.go                # MODIFY: add Jobs int field; default-on-missing-or-zero = 4
    ├── gitexec/
    │   └── gitexec.go               # MODIFY: Runner.Run returns ([]byte stderr, error); Real captures stderr internally
    ├── sync/
    │   ├── sync.go                  # MODIFY: replace serial loop in syncReposSection with worker-pool dispatch
    │   ├── workers.go               # NEW: bounded worker pool, atomic stderr writer, repo-event prefixing
    │   ├── workers_test.go          # NEW: pool fan-out + cancellation + stderr capture + atomicity tests
    │   └── sync_test.go             # MODIFY: existing tests adapt to new Runner signature
    └── (paths, gitlabapi unchanged)
```

**Structure Decision**: Extract the worker-pool logic into a sibling file
`internal/sync/workers.go` so `sync.go` stays narrative (Sync → section
helpers) and `workers.go` is testable in isolation. Keep the worker pool
inside `internal/sync` (not a new package): it has no consumers outside
sync orchestration and adding a sub-package for ~70 LOC of fan-out logic
would be over-engineering per the "no premature abstraction" guidance.

## Complexity Tracking

| Item | Why it exists | Simpler alternative considered & rejected because |
| --- | --- | --- |
| **Default behavior shift: serial → 4 concurrent at default jobs** | Per spec clarification Q1, the user wants out-of-box parallelism. With the config value in `.gitty/config` the default is visible and one-line-editable. | (a) Keep default 1 (opt-in) — rejected by user in Q1. (b) Default to `runtime.NumCPU()` — rejected by user; too aggressive for SSH-agent capacity. The chosen value (4) is conservative, configurable, and overridable with `--jobs=1` for one-flag rollback. |
| **Suppress git's own stdout at all jobs values, including `--jobs=1`** | Per spec clarification Q2, the user wants a clean output that is not as verbose as git for every repo. Doing it consistently (all jobs values) avoids two different output shapes the user has to mentally track. | Pass-through under `--jobs=1` and suppress under `--jobs>1` — rejected because the output shape would then be a function of the jobs flag, which is a worse UX than a single consistent shape. |
| **Runner contract change: `Run` now returns captured stderr bytes** | Per FR-006, gitty needs per-call git stderr to forward it only on failure. The current `io.Writer`-on-the-struct shape would require a per-call buffer wired in by the caller, which couples the worker pool to gitexec internals. | (a) Add `RunCapture` as a second method — rejected because every caller wants the capture; two methods is busywork. (b) Per-call buffer on the Real struct — rejected because Real becomes per-call mutable and not safely shareable across workers. |
| **Worker pool inside `internal/sync` rather than a new package** | The pool has exactly one consumer (sync orchestration). Splitting it adds a package boundary for ~70 LOC. | New `internal/workerpool` package — rejected because no consumer outside sync would justify the cost. Revisit only if/when async-fetch or async-group-write specs need it. |

## Re-evaluation After Phase 1

After producing `research.md`, `data-model.md`, `contracts/`, and
`quickstart.md`, the design has not introduced any new dependency, any flag
beyond `--jobs`, any new exit code, or any change to on-disk layout
(`.gitty/config` gets one new optional field which old binaries simply
ignore and new binaries default to `4` when absent). The Runner contract
change is internal-only (only `internal/sync` calls `Runner.Run`). All
three Constitution principles remain in the same posture as the initial
check. **Final verdict: gates pass, plan is ready for `/speckit-tasks`.**

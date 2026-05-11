# Phase 1 Data Model: Async Repo Pulls

**Date**: 2026-05-11  **Plan**: [plan.md](./plan.md)  **Spec**: [spec.md](./spec.md)

This feature introduces no new on-disk artifacts other than one new optional
field in the existing `.gitty/config` TOML. In-process, it adds a worker
pool with three small value types: `Job`, `runStats`, and an atomic stderr
writer. The existing entities from the 001-arch-cleanup feature
(`config.Config`, `gitlabapi.Group`, `gitlabapi.Project`,
`gitlabapi.Client`, `gitexec.Runner`, `sync.Request`) all extend or change
as documented here. Nothing speculative.

## E1 — Workspace Config (`internal/config.Config`) — EXTENDED

Adds one field. All other fields and validation rules unchanged from the
post-001 contract.

| Field      | Type     | TOML key     | Purpose                                                                 |
| ---------- | -------- | ------------ | ----------------------------------------------------------------------- |
| `URL`      | `string` | `url`        | (unchanged) GitLab base URL.                                            |
| `HTTP`     | `bool`   | `http`       | (unchanged) clone via HTTPS when true.                                  |
| `RootPath` | `string` | `root_path`  | (unchanged) anchored GitLab namespace.                                  |
| `Jobs`     | `int`    | `jobs`       | **NEW.** Default concurrency for `gitty sync`. Coalesced to `4` when the field is missing (older configs) or `<= 0` (defensive). |

**Validation rules** (delta):

- `Jobs` is normalized inside `config.Load`: a parsed value of `0` (the
  Go zero value, also the value written by `toml.Unmarshal` when the key
  is absent) or any negative integer becomes `4`. The caller never sees
  a non-positive `Jobs`.
- Values `> 64` are NOT clamped in `config.Load`. The clamp happens at the
  CLI boundary (FR-003, FR-010) so the warning line is emitted on the
  invocation that supplied the out-of-range value, not silently every
  time the workspace is loaded.

**State transitions:** none. The file is rewritten in full on `gitty init`.

**Public surface (delta from 001):**

```go
const DefaultJobs = 4

type Config struct {
    URL      string `toml:"url"`
    HTTP     bool   `toml:"http"`
    RootPath string `toml:"root_path"`
    Jobs     int    `toml:"jobs"`
}

// Load behavior unchanged except Jobs is normalized post-Unmarshal.
// Save behavior unchanged; the new field is serialized.
```

**Backward compatibility:** a `.gitty/config` written by the pre-feature
binary has no `jobs` line; `toml.Unmarshal` leaves `Jobs` at `0`, which
`Load` rewrites to `DefaultJobs`. No migration tool needed.

## E2 — Sync Request (`internal/sync.Request`) — EXTENDED

Adds one field carrying the resolved jobs value (after flag-vs-config
resolution and clamping).

| Field        | Type     | Source / semantics                                                |
| ------------ | -------- | ----------------------------------------------------------------- |
| `GroupFlag`  | `string` | (unchanged)                                                       |
| `Token`      | `string` | (unchanged)                                                       |
| `DryRun`     | `bool`   | (unchanged)                                                       |
| `DoGroups`   | `bool`   | (unchanged)                                                       |
| `DoRepos`    | `bool`   | (unchanged)                                                       |
| `Nested`     | `bool`   | (unchanged)                                                       |
| `Jobs`       | `int`    | **NEW.** Resolved effective concurrency in `[1, 64]`. The cli package writes the resolved value here; sync never reads `cfg.Jobs` directly. |

**Invariant:** by the time `sync.Sync` receives a `Request`, `Jobs` is
already in `[1, 64]`. Any clamping or validation is the cli package's
responsibility (single point of input validation per spec).

## E3 — Sync Deps (`internal/sync.Deps`) — UNCHANGED

The injectable dependency struct is unchanged from 001. The Runner contract
change (E4 below) is absorbed inside the existing `Runner` field.

## E4 — Git Runner (`internal/gitexec.Runner`) — CHANGED CONTRACT

The interface method signature changes. This is the only API break in this
feature; the only caller is `internal/sync`.

**Before (post-001):**

```go
type Runner interface {
    Run(dir string, args ...string) error
}
```

**After (this feature):**

```go
type Runner interface {
    Run(ctx context.Context, dir string, args ...string) (stderr []byte, err error)
}
```

**Returned `stderr`:** the bytes git wrote to its stderr during the
invocation. Empty on success-with-silent-git. The caller decides whether
to forward to the user (forwarded on `err != nil`, discarded otherwise per
FR-006).

**Returned `err`:** the result of `cmd.Run()`. On context cancellation,
this is typically `*exec.ExitError` with a non-zero exit code from the
SIGKILL'd child.

**Production implementation `Real`:**

| Field   | Type        | Purpose                                                                |
| ------- | ----------- | ---------------------------------------------------------------------- |
| `Env`   | `[]string`  | (unchanged) Optional env override; nil ⇒ `os.Environ()`.               |
| ~~`Stdout`~~ | — | **REMOVED.** Always `io.Discard` per FR-006.                            |
| ~~`Stderr`~~ | — | **REMOVED.** Stderr is always captured into a per-call buffer.          |

`Real` is now stateless (beyond `Env`) and safe to share across all
worker goroutines.

## E5 — Job (`internal/sync.Job`) — NEW

A single repo operation queued to the worker pool. Pure value type.

```go
type Job struct {
    NamespacePath string  // e.g., "tenant/images/api"
    LocalDir      string  // destination relative to cwd, e.g., "images/api"
    CloneURL      string  // SSH or HTTPS depending on Config.HTTP
    Exists        bool    // pre-computed: true ⇒ pull, false ⇒ clone
}
```

**Validation rules:**

- `NamespacePath` MUST be non-empty (skipped projects never become Jobs).
- `CloneURL` MUST be non-empty (empty-URL projects are skipped before
  enqueueing — same defense as today's `syncReposSection`).
- `LocalDir` MUST be a sibling-or-descendant of cwd (no absolute paths,
  no `..`).

**Lifecycle:**

1. Built by `syncReposSection` from one `*gitlabapi.Project`.
2. Sent to the worker pool's input channel.
3. Picked up by a worker, which calls `gitexec.Runner.Run(ctx, ...)`.
4. Worker writes outcome line(s) through the atomic writer.

## E6 — Worker Pool (`internal/sync.pool`) — NEW (unexported)

A small struct that owns the semaphore, wait group, atomic writer, and
effective jobs count. Created inside `syncReposSection` and consumed by
`runOne(job)`.

```go
type pool struct {
    ctx            context.Context
    sem            chan struct{}
    wg             sync.WaitGroup
    out            *atomicWriter // wraps deps.Stderr
    runner         gitexec.Runner
    effectiveJobs  int
}
```

**Validation rules:**

- `len(p.sem) == cap(p.sem) == p.effectiveJobs`.
- `p.effectiveJobs` is in `[1, 64]` (input invariant from E2).
- `p.out` is non-nil.

**State transitions:**

1. **Construct** with `newPool(ctx, deps, jobs)`.
2. **Dispatch** one Job per `pool.Submit(job)` call. Submit blocks if the
   semaphore is full.
3. **Drain** with `pool.Wait()`. Returns `ctx.Err()` if cancelled, else nil.
4. After `Wait()` returns, the pool is no longer reusable.

## E7 — Atomic Writer (`internal/sync.atomicWriter`) — NEW (unexported)

The wrapper that serializes gitty's stderr writes. See research R-003.

```go
type atomicWriter struct {
    mu sync.Mutex
    w  io.Writer
}
```

**Methods:**

- `WriteLine(format, args...)` — one mutex-guarded `fmt.Fprintf` with a
  trailing newline.
- `WriteBlock(header, body)` — header line plus body bytes, all under one
  mutex acquisition. Used to keep gitty's error line and the captured
  git stderr contiguous in the output.

**Validation rule:** every gitty-emitted per-repo line in the worker pool
goes through one of these two methods. Direct writes to the underlying
writer from inside a worker are forbidden (enforced by code-review
discipline, not by a type-system trick).

## Mapping — spec entities to data model

| Spec entity         | Implemented as                                                  |
| ------------------- | --------------------------------------------------------------- |
| **Workspace Config (extended)** | E1 (`internal/config.Config` with new `Jobs int` field) |
| **Job**             | E5 (`internal/sync.Job`)                                        |
| **Worker Pool**     | E6 (`internal/sync.pool`)                                       |
| **Repo Event**      | One call to E7's `WriteLine` or `WriteBlock` per event          |

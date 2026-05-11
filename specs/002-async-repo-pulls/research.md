# Phase 0 Research: Async Repo Pulls

**Date**: 2026-05-11  **Plan**: [plan.md](./plan.md)  **Spec**: [spec.md](./spec.md)

All three open `[NEEDS CLARIFICATION]` markers in the spec were resolved during
`/speckit-specify` (Q1: config-driven `jobs` field, default 4; Q2: suppress
git stdout always, capture git stderr and forward only on failure; Q3: preserve
exit-0-on-partial-failure). This document covers the implementation decisions
a maintainer would otherwise have to make mid-coding.

## R-001 — Worker pool primitive

**Decision**: Use a buffered channel as a semaphore plus a `sync.WaitGroup`
to wait for all workers. No third-party concurrency library, no
`golang.org/x/sync/errgroup`.

```go
sem := make(chan struct{}, jobs)
var wg sync.WaitGroup
for _, p := range projects {
    if ctx.Err() != nil { break }
    sem <- struct{}{}            // acquire
    wg.Add(1)
    go func(p *gitlabapi.Project) {
        defer wg.Done()
        defer func() { <-sem }() // release
        runOne(ctx, p, deps, ...)
    }(p)
}
wg.Wait()
```

**Rationale**:

- Idiomatic Go — described in the standard library pattern docs.
- Backpressure is built in: the loop blocks at `sem <-` once `jobs` workers
  are in flight, so we don't pre-spawn 1000 goroutines for a 1000-repo
  group.
- Zero additional dependencies (FR-012, SC-007).
- The standard `sync.WaitGroup`/channel pattern is well-understood by every
  Go contributor; no need for a `Pool` abstraction in this codebase.

**Alternatives rejected**:

- `golang.org/x/sync/errgroup.SetLimit`: a clean API, but it's an extra
  module (`x/sync` is not in `go.mod` today). FR-012 forbids the add.
- Pre-spawning `jobs` long-lived worker goroutines that pull from a job
  channel: equally valid, but slightly more code. The semaphore version is
  shorter and equally correct.
- A dedicated `internal/workerpool` package: see plan.md Complexity
  Tracking — rejected as premature abstraction for one consumer.

## R-002 — Per-call git stderr capture

**Decision**: Change `gitexec.Runner.Run` to return both the captured stderr
bytes and an error. `Real.Run` writes git's stderr to a per-call
`bytes.Buffer` and returns that buffer's contents.

```go
type Runner interface {
    Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

type Real struct {
    Env []string // optional override; nil means os.Environ()
}

func (r *Real) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
    var stderr bytes.Buffer
    cmd := exec.CommandContext(ctx, "git", args...)
    cmd.Dir = dir
    cmd.Env = r.Env
    if cmd.Env == nil { cmd.Env = os.Environ() }
    cmd.Stdout = io.Discard
    cmd.Stderr = &stderr
    err := cmd.Run()
    return stderr.Bytes(), err
}
```

**Rationale**:

- Each git invocation owns its stderr buffer — no shared `io.Writer` that
  could be raced by workers.
- `io.Discard` for stdout is the cleanest expression of "we don't care."
- `exec.CommandContext` plus a context-aware Runner satisfies FR-009: when
  the context is cancelled (SIGINT), in-flight git children get killed.
- `Real` becomes stateless beyond `Env`, so it's safe to share across
  workers (one `Real` instance serves the whole pool).

**Alternatives rejected**:

- Keep `io.Writer` fields on `Real` and have the worker swap them per call
  — unsafe under concurrency. Rejected.
- Add a second method `RunCapture` — see plan.md Complexity Tracking; one
  method is enough since every caller wants the capture.
- Return `(stdout, stderr, err)` — gitty never reads git stdout, so
  returning it is dead weight. Rejected.

## R-003 — Atomic stderr writes for gitty's own per-repo lines

**Decision**: Wrap the user's stderr (which is `os.Stderr` in production,
`bytes.Buffer` in tests) in a small `atomicWriter` type with a
`sync.Mutex`. Every per-repo "event line" goes through this writer.

```go
type atomicWriter struct {
    mu sync.Mutex
    w  io.Writer
}

func (a *atomicWriter) WriteLine(s string) {
    a.mu.Lock()
    defer a.mu.Unlock()
    fmt.Fprintln(a.w, s)
}

// On error: write the gitty error line + the captured git stderr in a
// single critical section so they stay contiguous in the output.
func (a *atomicWriter) WriteBlock(header string, body []byte) {
    a.mu.Lock()
    defer a.mu.Unlock()
    fmt.Fprintln(a.w, header)
    a.w.Write(body)
    if len(body) > 0 && body[len(body)-1] != '\n' {
        fmt.Fprintln(a.w)
    }
}
```

**Rationale**:

- `os.Stderr` writes are atomic for buffers up to PIPE_BUF (4096 on Linux,
  512 on macOS guaranteed) but gitty's lines are well under that. The
  mutex is not strictly necessary for short lines on POSIX, but is cheap
  insurance against the `WriteBlock` case (gitty error line + git stderr
  buffer) where the body can exceed PIPE_BUF.
- Encapsulating writes in one type means the worker pool never touches
  the raw stderr writer directly, removing the chance of accidental
  unsafe writes from a future maintainer.

**Alternatives rejected**:

- Pre-format every line and pipe through a single goroutine that owns the
  writer (actor model) — works, but adds a channel and a goroutine for no
  semantic benefit over the mutex.
- Per-worker `bytes.Buffer` flushed at end of run — defers visibility of
  successful repos until the whole sync completes; bad UX for long runs.
  Rejected.

## R-004 — SIGINT propagation to git children

**Decision**: Use `signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)`
in `cli/sync_cmd.go` to derive a cancellable context from the parent
`context.Background()`. Pass that context through `sync.Sync` →
`workers.go` → `gitexec.Real.Run`. When the user presses Ctrl-C:

1. The OS signal cancels the context.
2. `exec.CommandContext` sends SIGKILL to in-flight git children (default;
   we leave this as-is since git's atomic operations are restart-safe and
   the SC-005 budget is "≤2 seconds to exit").
3. The dispatch loop sees `ctx.Err() != nil` and stops queueing new repos.
4. `wg.Wait()` blocks until in-flight workers finish.
5. `Sync` returns `ctx.Err()`; `cli.Main` maps it to a non-zero exit code.

**Rationale**:

- `signal.NotifyContext` (Go 1.16+) is the standard library answer; no
  external signal package needed.
- `exec.CommandContext`'s default SIGKILL is acceptable for git: any
  in-progress clone is a separate working directory that will be cleaned
  up on the next sync (idempotent retry — Constitution Principle I).
- The 2-second budget in SC-005 is comfortably achievable: the workers
  are bounded by `jobs` (≤64), each runs `cmd.Run()` which returns
  immediately on SIGKILL, then the worker goroutine exits.

**Alternatives rejected**:

- Custom SIGINT handler that calls `cmd.Process.Signal(os.Interrupt)`
  for graceful shutdown — requires per-platform behavior (windows
  doesn't support SIGINT to children well), more code, no observable
  benefit for git. Rejected.
- A two-stage shutdown (first SIGINT → wait → second SIGINT → SIGKILL)
  — too clever for the value; users expect one Ctrl-C to mean stop.
  Rejected.

## R-005 — Config field default-on-missing

**Decision**: Add `Jobs int \`toml:"jobs"\`` to `config.Config`. In
`config.Load`, after `toml.Unmarshal`, normalize the value:

```go
if cfg.Jobs <= 0 {
    cfg.Jobs = 4
}
```

`gitty init` writes `Jobs: 4` into the freshly-created config so the field
is visible to anyone who `cat`s the file.

**Rationale**:

- A zero `Jobs` value means "not present in TOML" (Go's zero value for
  `int`). Coalescing zero-or-negative to the default is the simplest
  forward-compatible rule and matches FR-001b ("missing field treated as
  `jobs = 4`").
- No separate `*int` / `null.Int` type, no schema-version field, no
  migration step.

**Alternatives rejected**:

- `*int` pointer to distinguish "absent" from "explicit zero" — the spec
  treats `<=0` as invalid (FR-003), so collapsing the two cases is
  correct, and pointers are awkward in TOML round-tripping.
- A second TOML key like `version = 2` to indicate "this config has
  `jobs`" — adds machinery for no observable benefit.

## R-006 — Flag validation and clamping

**Decision**: Parse `--jobs` as `int`, default `0` (sentinel for "not
passed; use config"). Validate after parsing:

```go
switch {
case jobs < 0:
    return 2, fmt.Errorf("--jobs must be >= 1 (got %d)", jobs)
case jobs == 0:
    jobs = cfg.Jobs // already normalized to >=1 by config.Load
case jobs > 64:
    fmt.Fprintf(stderr, "--jobs %d clamped to 64\n", jobs) // FR-010
    jobs = 64
}
```

**Rationale**:

- `0` as "not passed" lets us distinguish "user typed `--jobs=0`" (rejected)
  from "user didn't pass the flag" (use config). Go's `flag` package
  doesn't differentiate by default; we get the differentiation for free
  via the default value.
- Clamp message matches FR-010's exact text.
- Negative values fail at flag parse time with exit code 2 per FR-003 and
  contracts/cli.md.

**Alternatives rejected**:

- Use `*int` and check `nil` for "not passed" — more code, no benefit.
- A custom `flag.Value` implementation that enforces the range — overkill;
  the inline switch is 5 lines and readable.

## R-007 — Repo prefix only when effective jobs > 1

**Decision**: The worker pool tracks an `effectiveJobs int` field. Per-repo
events are prefixed `[<namespace/path>] ` iff `effectiveJobs > 1`. Under
`effectiveJobs == 1`, the output is byte-for-byte the post-001-cleanup
serial format (no prefix).

**Rationale**:

- FR-005 says "With `--jobs>1`". Adding the prefix at `=1` would change
  byte-for-byte output for users who never wanted concurrency.
- The cost is one extra `if` per line write — negligible.
- A consequence: `grep` behavior differs between `--jobs=1` and `--jobs>1`.
  This is the intended UX: serial mode is "what you've always had,"
  concurrent mode adds grep-able prefixes precisely because lines may
  interleave.

**Alternatives rejected**:

- Always prefix — neat consistency but a gratuitous output change at
  `--jobs=1`. Rejected.
- Tag lines with a worker ID (`[w3] tenant/api ...`) — adds noise without
  helping the user; namespace path is the meaningful key.

## R-008 — Dry-run path bypasses the worker pool entirely

**Decision**: When `req.DryRun` is true, the repo loop in
`syncReposSection` does NOT enter the worker pool. It iterates projects in
the order returned by the GitLab API and writes plan lines to stdout
sequentially. The pool is only constructed and used on the real-run path.

**Rationale**:

- FR-008/SC-006 demand byte-identical dry-run output regardless of `--jobs`.
  Serial iteration is the simplest way to guarantee that.
- Dry-run output is fast (in-memory only), so no parallelism is gained
  from the pool. Avoiding the pool removes a potential reorder hazard.

**Alternatives rejected**:

- Build the worker pool unconditionally and have workers emit plan lines
  in a deterministic order — possible but requires reordering at flush
  time. Pointless complexity for a code path that doesn't benefit.

## R-009 — Test strategy for the worker pool

**Decision**: The new `workers_test.go` will use a fake `gitexec.Runner`
that records calls, optionally sleeps to simulate work, and optionally
returns a stderr buffer + error. Tests cover:

1. **Fan-out**: with 4 jobs and 8 projects, ≤4 in-flight at any moment.
   Verified by counting concurrent calls in the fake (`atomic.Int64`
   counter).
2. **Stderr capture on error**: a fake returning `(stderr="...", err=...)`
   results in gitty emitting an error line plus the captured stderr.
3. **Stderr suppression on success**: same fake with `err=nil` does NOT
   emit the captured stderr.
4. **Atomic gitty lines**: with 4 jobs and many short-line emissions, no
   gitty line is split mid-line in the captured stderr buffer.
5. **Repo prefix toggles at jobs>1**: with `effectiveJobs=1` no prefix is
   emitted; with `effectiveJobs=2` every gitty line is prefixed.
6. **Context cancellation**: cancel the context after a few workers have
   started; assert the dispatch loop stops and `Sync` returns
   `ctx.Err()`.

All six tests run without git on PATH (via the fake) and without network.

**Rationale**:

- These six cases cover the FR-005, FR-006, FR-007, FR-009 surface
  exhaustively in unit form.
- The fan-out counter is the canonical way to test bounded concurrency in
  Go.
- Context cancellation is exercised against the in-process fake (not a
  real signal) because we want a unit test, not an integration test.

## R-010 — Help text update for `--jobs`

**Decision**: Augment `gitty sync -h` with the new flag and its range, and
mention the `jobs` config key:

```
-jobs N
      Maximum concurrent clone/pull operations.
      Range: [1, 64]; overrides 'jobs' in .gitty/config.
      Default: value from .gitty/config (typically 4).
```

The README also gets a short bullet in the Features section and a one-line
update to the Sync Flags table.

**Rationale**:

- FR-011 mandates the help text.
- The Sync Flags table is part of contracts/cli.md and the README; keeping
  them in sync is one paragraph each.

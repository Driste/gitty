---

description: "Task list for the Async Repo Pulls feature"
---

# Tasks: Async Repo Pulls with Concurrency Limit

**Input**: Design documents from `/specs/002-async-repo-pulls/`
**Prerequisites**: [plan.md](./plan.md) ✓, [spec.md](./spec.md) ✓, [research.md](./research.md) ✓, [data-model.md](./data-model.md) ✓, [contracts/](./contracts/) ✓, [quickstart.md](./quickstart.md) ✓

**Tests**: Seed/integration tests are IN scope. The Runner contract change forces a sweep of every fake-runner test, and the worker pool needs dedicated tests for fan-out, atomicity, stderr capture, prefix toggle, and cancellation.

**Organization**: Three user stories from spec. Note: this feature requires a small but real **Runner contract change** (return `[]byte` stderr + accept `context.Context`) that's foundational to both US1 and US2. The change lands in Phase 2 along with the config field and the `--jobs` flag plumbing — the binary still behaves serially at end of Phase 2 because the worker pool itself isn't introduced until US1.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: US1 / US2 / US3, mapping to spec user stories
- File paths are relative to repo root.

## Path Conventions

Repository root: `/Users/tylercarper/Documents/dev/gitty/`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Verify the post-001 layout is current and the starting build is clean. No new files in this phase.

- [X] T001 Verify the current branch is `002-async-repo-pulls` and the working tree is clean.
- [X] T002 [P] Verify `go build ./...` is green from a fresh checkout (records the pre-feature build baseline).
- [X] T003 [P] Verify `go vet ./...` is green.
- [X] T004 [P] Verify `go test ./...` is green with a sanitized PATH (no `git` binary) — records the pre-feature test baseline so any new test failures are attributable to this feature.

**Checkpoint**: Starting state is green and confirmed.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Land the Runner contract change, the new config field, the new flag, and context threading. At the end of this phase the binary still behaves serially (no worker pool yet) but accepts `--jobs`, reads `jobs` from `.gitty/config`, and threads a cancellable context through the call chain. `git` stdout is suppressed from this phase onward (FR-006); `git` stderr is captured and forwarded only on failure.

⚠️ CRITICAL: No user-story phase can begin until Phase 2 is complete, `go build ./...` and `go vet ./...` pass, and the adapted tests are green.

### Phase 2a — Runner contract change

- [X] T010 Modify [internal/gitexec/gitexec.go](../../internal/gitexec/gitexec.go): change `Runner.Run` to `Run(ctx context.Context, dir string, args ...string) ([]byte, error)`. Replace `Real`'s `Stdout`/`Stderr` fields with internal `bytes.Buffer` capture. Wire `exec.CommandContext(ctx, "git", args...)`. Set `cmd.Stdout = io.Discard`. Preserve env-passthrough via the existing `Env` field (default `os.Environ()` when nil). Return `(stderr.Bytes(), err)`.
- [X] T011 Modify [internal/gitexec/gitexec_test.go](../../internal/gitexec/gitexec_test.go): update the `fakeRunner` to the new method signature `Run(ctx, dir, args...) ([]byte, error)` and assert the contract via a small interface check.

### Phase 2b — Adapt existing callers and tests to the new Runner signature

- [X] T020 Modify [internal/sync/sync.go](../../internal/sync/sync.go) so `Sync` accepts a `context.Context` as its first argument. Plumb it through `syncGroupsSection` and `syncReposSection` to `deps.Runner.Run(ctx, ...)`. Inside the existing serial loop, on Runner error: write a gitty error line followed by the captured git stderr buffer (single contiguous block). On success: discard the captured buffer.
- [X] T021 Modify [internal/sync/sync_test.go](../../internal/sync/sync_test.go): update the local `fakeRunner` to the new signature; update both existing tests (`TestSync_DoGroupsAndRepos`, `TestSync_DryRunStreamSplit`) to pass `context.Background()`.
- [X] T022 Modify [internal/cli/sync_cmd.go](../../internal/cli/sync_cmd.go): change `defaultRunnerCtor` to return a stateless `&gitexec.Real{}` (no Stdout/Stderr wiring). Thread `ctx` from `runSync`'s parameter list into `syncpkg.Sync(ctx, ...)`.
- [X] T023 Modify [internal/cli/cli.go](../../internal/cli/cli.go): add a `context.Context` parameter to `mainWithDeps`'s flow; `Main` builds it via `context.Background()` for now (signal handling lands in Phase 5/US3). Pass `ctx` through to `runSync`.
- [X] T024 Modify [internal/cli/cli_test.go](../../internal/cli/cli_test.go): update `fakeRunner` to the new signature; update all existing tests to pass `context.Background()` (or rely on the new signal-free default).

### Phase 2c — Config field and default

- [X] T030 Modify [internal/config/config.go](../../internal/config/config.go): add `Jobs int \`toml:"jobs"\`` to `Config`. Export `DefaultJobs = 4` constant. In `Load`, after `toml.Unmarshal`, coalesce `cfg.Jobs <= 0` to `DefaultJobs` (research R-005).
- [X] T031 Modify [internal/config/config_test.go](../../internal/config/config_test.go): extend `TestSaveLoadRoundTrip` to assert `Jobs` survives the round trip; add `TestLoadCoalescesMissingJobs` (write a TOML file without the `jobs` key, assert `Load` returns `Jobs == 4`); add a sub-test for negative-jobs coalescing to 4.
- [X] T032 Modify [internal/cli/init_cmd.go](../../internal/cli/init_cmd.go): include `Jobs: config.DefaultJobs` in the `Config` struct that `gitty init` saves, so a freshly-created `.gitty/config` shows `jobs = 4` to the user (SC-009).

### Phase 2d — Request.Jobs + `--jobs` flag

- [X] T040 Modify [internal/sync/sync.go](../../internal/sync/sync.go): add `Jobs int` to `Request`. The struct invariant (per data-model E2) is that `Jobs` arrives in `[1, 64]`; `Sync` does no further validation.
- [X] T041 Modify [internal/cli/sync_cmd.go](../../internal/cli/sync_cmd.go): add `-jobs` flag (`int`, default `0` sentinel). After parse: if value `< 0` return `(2, fmt.Errorf("--jobs must be >= 1 (got %d)", v))`. If value `== 0`, fall back to `cfg.Jobs`. If value `> 64`, emit one stderr line `--jobs <N> clamped to 64` and set to 64. Write the resolved value into `Request.Jobs`. Help text per [contracts/cli.md](contracts/cli.md) (FR-011).
- [X] T042 Modify [internal/cli/cli_test.go](../../internal/cli/cli_test.go): add three argv-fixture tests for `--jobs` per [contracts/cli.md](contracts/cli.md) "Test-fixture inputs": `--jobs 4` → `Request.Jobs == 4`; `--jobs -1` → exit `2` with the documented error text; `--jobs 100` → `Request.Jobs == 64` plus one stderr warning line.

### Phase 2e — Verify foundation

- [X] T050 Run `go build ./...` and `go vet ./...`. Both MUST be green.
- [X] T051 Run `go test ./...` with a PATH that excludes `git` (mirror the procedure from 001's SC-008). All tests MUST pass.
- [X] T052 Behavior smoke check: build the binary, run `gitty init` in a tmpdir, `cat .gitty/config` — confirm it contains `jobs = 4`. Run `gitty sync -h` — confirm `-jobs` appears in the help with the documented description.

**Checkpoint**: Binary builds, all tests pass, `--jobs` is accepted, `.gitty/config` carries `jobs`, but the sync loop is still serial. The three user-story phases can now proceed.

---

## Phase 3: User Story 1 — Sync a large group faster on a fast pipe (Priority: P1) 🎯 MVP

**Goal**: Introduce the bounded worker pool so multiple repos clone/pull concurrently. With Phase 2 done, this is purely additive: the new pool replaces the serial loop body inside `syncReposSection` while `Request.Jobs == 1` continues to produce a strictly serial run.

**Independent Test (offline subset)**: a unit test with a fake `gitexec.Runner` that records call timing can verify peak in-flight ≤ `Request.Jobs`. The wall-clock claim (SC-001) is verified against real GitLab in Phase 6 and deferred to the user.

### Implementation for User Story 1

- [X] T100 [US1] Create [internal/sync/workers.go](../../internal/sync/workers.go) with: `type pool struct { ctx; sem; wg; out *atomicWriter; runner; effectiveJobs }`; constructor `newPool(ctx, deps, jobs)`; method `Submit(job Job)` that acquires the semaphore, spawns a goroutine, calls `runOne(p, job)`; method `Wait() error` that blocks until all in-flight jobs return and propagates `ctx.Err()` if cancelled. Also define `type Job struct{ NamespacePath, LocalDir, CloneURL string; Exists bool }` per data-model E5.
- [X] T101 [US1] Add `atomicWriter` to [internal/sync/workers.go](../../internal/sync/workers.go) with `WriteLine(format, args...)` and `WriteBlock(header string, body []byte)` (mutex-guarded). All gitty stderr writes from the worker pool route through it.
- [X] T102 [US1] Add `runOne(p *pool, job Job)` to [internal/sync/workers.go](../../internal/sync/workers.go): emit a start line via `p.out.WriteLine`, call `p.runner.Run(p.ctx, ...)` with the appropriate `pull` or `clone` args, then either emit a success line (and discard captured stderr) or an error block (`p.out.WriteBlock("[<path>] error: ..."`, body=capturedStderr)). When `p.effectiveJobs > 1`, prefix every line with `[<job.NamespacePath>] `. For clone actions, `os.MkdirAll(filepath.Dir(job.LocalDir), 0755)` happens inside `runOne` before the git call.
- [X] T103 [US1] Modify [internal/sync/sync.go](../../internal/sync/sync.go) `syncReposSection`: replace the serial `for _, prj := range projects { ... }` body with: build a `[]Job` from `projects` (filtering empty-URL with the existing skip-line); if `req.DryRun`, iterate `Jobs` in order and emit plan lines to stdout (no pool); else construct `newPool(ctx, deps, req.Jobs)`, `Submit` each Job, then `pool.Wait()`.
- [X] T104 [US1] Modify [internal/sync/sync_test.go](../../internal/sync/sync_test.go) `TestSync_DoGroupsAndRepos` and `TestSync_DryRunStreamSplit` to populate `req.Jobs = 1`. Verify both still pass — behavior at `Jobs == 1` MUST remain byte-identical to today's serial mode (FR-002), apart from the FR-006 git-stdout suppression that already landed in Phase 2.

### Tests for User Story 1

- [X] T110 [P] [US1] Create [internal/sync/workers_test.go](../../internal/sync/workers_test.go) — `TestPool_FanOut`: with `jobs=4` and 12 fake-runner jobs (each sleeping ~50ms), assert the recorded peak concurrency is `>1` and `<=4`. Uses `atomic.Int64` to track in-flight count. No network, no `git` binary.
- [X] T111 [P] [US1] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestSync_Idempotency`: against a tmpdir + fake Runner, run `Sync` twice with the same input; the second run MUST produce only `pull` invocations (no clone, no mkdir of existing dirs). Locks in SC-003.

**Checkpoint**: With `--jobs > 1` the binary runs clones/pulls in parallel. Per-repo prefixing is in place; output remains correlatable by repo path. SC-002 and SC-003 are testable offline; SC-001 wall-clock requires real GitLab and is deferred to Phase 6.

---

## Phase 4: User Story 2 — See per-repo progress and outcomes clearly when jobs interleave (Priority: P1)

**Goal**: Lock in the per-repo grep-ability and atomic-write contract introduced in US1. With Phase 3 done, the behavior already exists; US2 delivers the tests that prove it and prevent regressions.

**Independent Test (offline)**: `go test ./internal/sync` exercises the prefix toggle, atomicity, stderr-capture-on-error, and stderr-suppress-on-success cases against the fake Runner.

### Tests for User Story 2

- [X] T200 [P] [US2] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestPool_PrefixToggle`: run twice — once with `effectiveJobs=1` (no prefix on per-repo lines), once with `effectiveJobs=2` (every per-repo line starts with `[<namespace/path>] `). Assert exact line shapes via a `bytes.Buffer` stderr capture. Locks in FR-005.
- [X] T201 [P] [US2] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestPool_AtomicLines`: with `jobs=8` and 64 fake-runner jobs each emitting one short line, capture stderr to a `bytes.Buffer` and assert every line in the buffer (a) is non-empty after `strings.Split("\n")`, (b) is fully prefixed `[...] ` (no splits mid-line), (c) appears exactly once across the 64 jobs. Locks in FR-006 atomicity.
- [X] T202 [P] [US2] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestPool_StderrCaptureOnError`: fake Runner returns `(stderr=[]byte("fatal: bad ref\n"), err=&exec.ExitError{...})` for one project; assert the captured stderr block appears in gitty's stderr immediately after the gitty error line for that repo, and that successful repos' output does NOT contain "fatal: bad ref". Locks in FR-006 capture-on-error.
- [X] T203 [P] [US2] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestPool_StderrSuppressOnSuccess`: fake Runner returns `(stderr=[]byte("Cloning into ...\n"), err=nil)`; assert "Cloning into" does NOT appear in gitty's stderr. Locks in FR-006 discard-on-success.
- [X] T204 [P] [US2] In [internal/sync/workers_test.go](../../internal/sync/workers_test.go) add `TestPool_DryRunBypassed`: with `req.DryRun=true` and `req.Jobs=4`, assert the fake Runner is NEVER called and stdout contains the plan lines in stable repo-order matching `req.Jobs=1`. Locks in FR-008 and SC-006.

### Verification for User Story 2

- [X] T210 [US2] Manual grep-ability check (deferred to user, can also run offline): build the binary; run `gitty sync ... --jobs=4 2> stderr.log` against any target (real or fake-via-tests); run `grep '\[<some/repo>\]' stderr.log` and confirm the output is exactly that repo's events. Record outcome in PR description (SC-004).

**Checkpoint**: All four contract guarantees from US2 are pinned by tests. Future regressions trip the suite.

---

## Phase 5: User Story 3 — Stop a runaway sync cleanly (Priority: P2)

**Goal**: Wire SIGINT to cancel in-flight work and exit non-zero. Without this, the worker pool keeps running after Ctrl-C until every clone completes.

**Independent Test (offline)**: a unit test cancels the context after a few workers start; assert `Sync` returns `ctx.Err()` and no new jobs were dispatched after cancellation. The 2-second wall-clock budget (SC-005) requires a real slow target and is deferred to the user.

### Implementation for User Story 3

- [X] T300 [US3] Modify [internal/cli/cli.go](../../internal/cli/cli.go) `Main` (and the test seam `mainWithDeps` if it needs the ctx): replace the placeholder `context.Background()` from T023 with `ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM); defer stop()`. Pass `ctx` into `runSync`.
- [X] T301 [US3] Modify [internal/sync/workers.go](../../internal/sync/workers.go) `pool.Submit`: check `p.ctx.Err()` BEFORE acquiring the semaphore; if cancelled, return early without queueing. Inside `runOne`, the captured-stderr block on failure is unchanged; the `ctx`-cancelled case is just a regular Runner error that gitexec already surfaced via `exec.CommandContext`.
- [X] T302 [US3] Modify [internal/sync/sync.go](../../internal/sync/sync.go) `Sync`: after `pool.Wait()`, if `ctx.Err() != nil` return it so `cli.Main` can map it to a non-zero exit code.
- [X] T303 [US3] Modify [internal/cli/sync_cmd.go](../../internal/cli/sync_cmd.go): when `syncpkg.Sync` returns an error and `errors.Is(err, context.Canceled)`, return exit code `130` (the POSIX convention for SIGINT) instead of `1`. Other errors keep mapping to `1`.

### Tests for User Story 3

- [X] T310 [US3] Create test `TestPool_ContextCancellation` in [internal/sync/workers_test.go](../../internal/sync/workers_test.go): build a pool with a fake Runner that sleeps 100ms; queue 10 jobs; after ~25ms call the cancel func; assert `pool.Wait()` returns within 200ms, returns a non-nil error containing `context.Canceled`, and the Runner recorded fewer than 10 invocations.
- [X] T311 [US3] In [internal/cli/cli_test.go](../../internal/cli/cli_test.go) add `TestMain_SyncCancelledReturnsNonZero`: drive `mainWithDeps` against a fake Runner that blocks, cancel the context externally, assert `mainWithDeps` returns a non-zero exit code distinct from `1`.

**Checkpoint**: Ctrl-C cleanly stops the sync. Test suite proves cancellation propagates from cli down through pool down to the in-flight Runner calls.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Documentation, smoke checks, and the deferred-to-user SC items.

- [X] T400 [P] Update [README.md](../../README.md): add a row to the Sync Flags table for `--jobs`; add a short bullet to the Features list ("Parallel sync: clone/pull multiple repos concurrently with a configurable concurrency limit"); add a one-paragraph mention in the Configuration section that `.gitty/config` now carries a `jobs` field.
- [X] T401 [P] Update [specs/001-arch-cleanup/quickstart.md](../001-arch-cleanup/quickstart.md) ONLY if needed — verify it does not contradict the new behavior (it should not; 001 is layout-only).
- [X] T402 [P] Run final `go build ./...`, `go vet ./...`, `go test ./...` (with `git` excluded from PATH). All three MUST be green.
- [X] T403 [P] Verify `git diff go.mod go.sum` is empty (SC-007, FR-012).
- [X] T404 [P] Confirm the PR description includes: (a) the FR-006 git-output-suppression behavior change, (b) the `--jobs` flag and default, (c) the `.gitty/config` `jobs` field with backward-compat note, (d) the FR-005 prefix rule, (e) the SIGINT exit-code semantics, (f) a callout that `--verbose` / line prefixes / ANSI / credential redaction remain deferred to a separate Principle II spec.
- [ ] T405 Deferred to user — verify SC-001: against a real GitLab group with ≥8 missing repos, time `gitty sync --path=<group> --jobs=1` and `gitty sync --path=<group> --jobs=4`. Confirm the `--jobs=4` run completes in ≤60% of the `--jobs=1` time.
- [ ] T406 Deferred to user — verify SC-005: start `gitty sync --jobs=4` against a slow target, send SIGINT after ~1s, confirm the process exits in ≤2s and `ps` shows no orphaned git children. Record outcome in PR description.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: T001 must run first; T002–T004 in parallel after T001 (read-only commands).
- **Phase 2 (Foundational)**: Depends on Phase 1. Hard internal ordering:
  - Phase 2a (T010–T011) before 2b — the Runner signature must change before callers can compile against it.
  - Phase 2b (T020–T024) before 2c, 2d — the callers need the new Runner contract green.
  - Phase 2c (T030–T032) and 2d (T040–T042) can run in parallel after 2b — they touch different files.
  - Phase 2e (T050–T052) after all of 2a–2d.
- **Phases 3, 4, 5 (User Stories)**: All depend on Phase 2 complete. US1 → US2 strictly (US2 tests the surfaces US1 introduces). US3 is independent of US2 and can run in parallel with US2.
- **Phase 6 (Polish)**: Depends on Phases 2–5 all complete.

### Within Each User Story

- US1: T100 → T101 → T102 → T103 are sequential (workers.go grows file-internally). T104 can land in parallel with T100–T102 (different file). T110/T111 after T100–T103.
- US2: T200–T204 all parallel (different test cases in the same file but independent). T210 is a separate manual verification.
- US3: T300 → T301/T302/T303 (T301–T303 touch different files and can run in parallel). T310/T311 after T300–T303.

### Parallel Opportunities

- Phase 1: T002–T004 (all read-only).
- Phase 2c/2d together (different files).
- Phase 4 US2 tests T200–T204 (different test functions in the same file).
- Phase 6 T400–T404 (different files / read-only checks).

---

## Parallel Example: Phase 4 (US2 tests)

```bash
Task: "T200 — TestPool_PrefixToggle in workers_test.go"
Task: "T201 — TestPool_AtomicLines in workers_test.go"
Task: "T202 — TestPool_StderrCaptureOnError in workers_test.go"
Task: "T203 — TestPool_StderrSuppressOnSuccess in workers_test.go"
Task: "T204 — TestPool_DryRunBypassed in workers_test.go"
```

All five test functions live in `internal/sync/workers_test.go` but are independent — different fixture data, different assertions, no shared state.

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Phase 1 Setup.
2. Phase 2 Foundational (Runner contract + Config field + `--jobs` flag, all serial-safe).
3. Phase 3 US1 (worker pool + prefixing).
4. **STOP and VALIDATE**: A user with `gitty sync --jobs=4` sees per-repo progress lines prefixed with namespace path; the run completes faster than `--jobs=1`. Unit tests T104, T110, T111 are green.
5. Open PR; ship as MVP without US2 lock-in tests or US3 SIGINT handling.

The MVP is functionally complete for the speed gain. Without US2/US3 you lose: the explicit regression tests for FR-005/FR-006, and graceful Ctrl-C.

### Incremental Delivery (recommended)

1. Phase 1 + Phase 2: contract change ships, binary still serial. Safe intermediate state.
2. Phase 3 (US1): worker pool ships with prefixing. End-users see the speed improvement.
3. Phase 4 (US2): tests lock the output contract. CI catches regressions of FR-005/FR-006.
4. Phase 5 (US3): SIGINT handling ships. Power users and CI users get graceful cancellation.
5. Phase 6: README + final verifications + deferred manual checks.

### Single-PR Delivery (also valid)

Land everything in one PR. Each commit must keep `go build`, `go vet`, `go test ./...` green. The Runner contract change is the most disruptive single commit; bundle T010–T011 with T020–T024 (Phase 2a + 2b) so the compile breakage doesn't span more than one commit.

---

## Notes

- The Runner contract change (T010) is the highest-blast-radius edit. All five fakeRunner implementations across `internal/{gitexec,sync,cli}/*_test.go` need updating in lockstep with T010 to keep `go test ./...` green.
- `flag.NewFlagSet`'s default-zero for an `int` flag is the resolution sentinel — keep it that way. Do not try to detect "user passed `--jobs=0`" specifically; `0` means "not passed" and falls through to the config value (research R-006).
- No `golang.org/x/sync/errgroup`. FR-012 forbids new dependencies; the standard `sync.WaitGroup` + buffered channel semaphore is the right tool (research R-001).
- `exec.CommandContext`'s default termination on cancel is SIGKILL. That's acceptable for git: any partial clone leaves a recoverable directory tree (Constitution Principle I idempotency).
- The `[<namespace/path>] ` prefix is only emitted when `effectiveJobs > 1` (research R-007). At `effectiveJobs == 1` the output is byte-identical to the post-001 serial format apart from the FR-006 git-stdout suppression.
- T405 and T406 require a real GitLab group + network; they are deferred to the user and the spec marks them as such.
- Verify tests pass after each phase before moving to the next.
- Avoid: new Go module dependencies (FR-012); any change to existing flag names/defaults (FR-008 from 001); any persistence of `--jobs` value back to `.gitty/config` (Constitution Principle III).

---

description: "Task list for the Architecture Cleanup feature"
---

# Tasks: Architecture Cleanup

**Input**: Design documents from `/specs/001-arch-cleanup/`
**Prerequisites**: [plan.md](./plan.md) ✓, [spec.md](./spec.md) ✓, [research.md](./research.md) ✓, [data-model.md](./data-model.md) ✓, [contracts/](./contracts/) ✓, [quickstart.md](./quickstart.md) ✓

**Tests**: Seed tests are IN scope per spec FR-011 (clarification Q3). One `*_test.go` per internal package, no network, no `git` binary required.

**Organization**: Three user stories from spec. Note: this feature is a refactor — by spec assumption, the package split itself (the bulk of the work) lives in **Phase 2 Foundational** because nothing else can ship until the binary still behaves identically in the new layout. The user-story phases then deliver the user-visible value of each story on top of that foundation.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: US1 / US2 / US3, mapping to spec user stories
- File paths are exact and absolute-friendly (relative to repo root)

## Path Conventions

Repository root: `/Users/tylercarper/Documents/dev/gitty/` — paths below are relative to repo root.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Create the directory skeleton the new packages will live in. No Go code in this phase — just directories, so the package-creation tasks in Phase 2 can be done in parallel.

- [X] T001 Create directory `internal/` at repo root
- [X] T002 [P] Create directory `internal/cli/`
- [X] T003 [P] Create directory `internal/config/`
- [X] T004 [P] Create directory `internal/gitlabapi/`
- [X] T005 [P] Create directory `internal/paths/`
- [X] T006 [P] Create directory `internal/gitexec/`
- [X] T007 [P] Create directory `internal/sync/`

**Checkpoint**: All six leaf directories exist. `go build ./...` is unaffected (no `.go` files added yet).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Move all existing functionality into the new packages with stream-split applied (FR-010), token resolution centralized (FR-004), pagination deduplicated (FR-006), and the GitLab/git surfaces narrowed behind interfaces (FR-003, FR-007). At the end of Phase 2 the binary behaves identically to today (FR-008) but lives in the new layout.

⚠️ CRITICAL: No user story phase can begin until Phase 2 is complete and `go build ./...` + `go vet ./...` pass.

The order below is the import-graph order (leaves first → orchestration → entrypoint), so each task sees its dependencies already in place.

### Phase 2a — Leaf packages (no internal dependencies)

- [X] T010 [P] Implement `internal/paths/paths.go` exporting `func Local(apiFullPath, configRoot string) string` with the exact behavior of today's `getLocalRelPath` in [sync.go](../../sync.go) (trim configRoot prefix; trim leading `/`; return apiFullPath unchanged when configRoot is empty).
- [X] T011 [P] Implement `internal/config/config.go`: define `Config` struct with `URL`/`HTTP`/`RootPath` TOML tags (move verbatim from [config.go](../../config.go)), constants `Dir = ".gitty"` and `File = "config"`, `Load(workspaceDir string) (*Config, error)`, and `Save(workspaceDir string, cfg *Config) error`. Per [contracts/workspace.md](contracts/workspace.md), `Load` MUST return the exact error strings listed there for missing-file, unreadable, malformed-TOML, and empty-URL cases.
- [X] T012 [P] Implement `internal/gitexec/gitexec.go`: define `Runner` interface (`Run(dir string, args ...string) error`) and concrete `Real` struct with `Stdout`, `Stderr` (`io.Writer`) and `Env` (`[]string`, defaults to `os.Environ()` when nil) fields. `Real.Run` MUST preserve today's env-passthrough behavior in [sync.go](../../sync.go) `runGit` (SSH agent socket, global gitconfig).
- [X] T013 [P] Implement `internal/gitlabapi/client.go`: define POGO types `Group{FullPath, Name string}` and `Project{PathWithNamespace, SSHURLToRepo, HTTPURLToRepo string}`, the `Client` interface with the four methods from [data-model.md E4](data-model.md), and a constructor `NewReal(baseURL, token string) (Client, error)` returning a `*Real` that wraps `gitlab.NewClient`.
- [X] T014 [P] Implement `internal/gitlabapi/paginate.go` exporting an unexported generic helper `func paginate[T any](fetch func(opts gitlab.ListOptions) ([]T, *gitlab.Response, error)) ([]T, error)` (per research R-003), with `PerPage: 100`. Use it inside `Real.ListSubGroups`, `Real.ListDescendantGroups`, and `Real.ListGroupProjects` so each is a single closure handing off to `paginate` (FR-006). `Real.GetGroup` does not paginate.

### Phase 2b — Orchestration (depends on 2a)

- [X] T020 [US-shared] Implement `internal/sync/token.go` exporting `func ResolveToken(explicit string, lookup func(string) string) (string, error)` with the order from [contracts/cli.md](contracts/cli.md) Token resolution section (`explicit` → `lookup("GITLAB_TOKEN")` → `lookup("CI_JOB_TOKEN")`); returns the exact error message from contracts/cli.md when all three are empty (FR-004).
- [X] T021 Implement `internal/sync/sync.go`: define `Action`, `Step`, `Plan`, and `Request` types per [data-model.md E5/E6](data-model.md). Implement `Build(req Request, c gitlabapi.Client, root string) (*Plan, error)` translating today's [sync.go](../../sync.go) `syncGroups` + `syncRepos` logic into Plan steps (no I/O, no git). Implement `(*Plan).Render(stdout io.Writer)` writing the existing dry-run text format to stdout. Implement `(*Plan).Apply(runner gitexec.Runner, errOut io.Writer) error` performing mkdirs, per-group config writes (via `internal/config`), and `runner.Run` invocations; per-step progress lines go to `errOut` (FR-010). Use `paths.Local` for every namespace→local-path conversion (FR-005). Defense-in-depth: skip projects whose chosen URL is empty, emitting one stderr line.
- [X] T022 Wire `Sync(req Request, c gitlabapi.Client, runner gitexec.Runner, stdout, errOut io.Writer) error` in `internal/sync/sync.go` as the single entry point: load `Config` via `internal/config`, call `Build`, then either `Render` (when `req.DryRun`) or `Apply`. Returns errors instead of calling `os.Exit` (so `cli.Main` can map them to exit codes).

### Phase 2c — CLI surface (depends on 2b)

- [X] T030 Implement `internal/cli/init_cmd.go` exporting `func runInit(args []string, stdout, stderr io.Writer) (int, error)`: parse the `-url` and `-http` flags exactly as today, call `internal/config.Save` on the cwd, write today's success lines to stdout, return 0 on success.
- [X] T031 Implement `internal/cli/sync_cmd.go` exporting `func runSync(args []string, stdout, stderr io.Writer, env func(string) string) (int, error)`: parse the six sync flags exactly as today (preserving the `if !doGroups && !doRepos { doRepos = true }` defaulting rule), call `sync.ResolveToken`, build a `gitlabapi.Real` client, build a `gitexec.Real` runner with `stdout`/`stderr` from args, build a `sync.Request`, call `sync.Sync`. Map errors to exit code 1; surface error text on `stderr`.
- [X] T032 Implement `internal/cli/cli.go` exporting `func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int` and `func usage(out io.Writer)`. Switch on `args[0]` (`init`/`sync`/empty/unknown) per [contracts/cli.md](contracts/cli.md) §"Subcommand: gitty (no args)". Empty args and unknown commands print usage to stdout and return 1 (matching today's exit behavior). Per FR-009, the usage text says `.gitty/config` instead of `gitty.toml`.

### Phase 2d — Entrypoint and old-file removal

- [X] T040 Replace [main.go](../../main.go) with a 5-line entrypoint: `package main`, imports `os` and `gitty/internal/cli`, body `os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))`.
- [X] T041 Delete [init.go](../../init.go) (logic moved to `internal/cli/init_cmd.go` + `internal/config`).
- [X] T042 Delete [sync.go](../../sync.go) (logic moved to `internal/sync/`, `internal/gitlabapi/`, `internal/gitexec/`, `internal/paths/`).
- [X] T043 Delete [config.go](../../config.go) (logic moved to `internal/config/config.go`).
- [X] T044 Run `go build ./...` from repo root and confirm a working `gitty` binary builds with no new errors. Run `go vet ./...` and confirm clean.
- [X] T045 Behavior-parity smoke check: build the binary, then for each argv row in [contracts/cli.md](contracts/cli.md) §"Test-fixture inputs" exercise the CLI manually (or via a shell script). For the success cases that don't need network, confirm the resulting `Request` is parsed correctly (printf-debug if needed; tests in Phase 4 will lock this in). For `init` and the no-args case, confirm stdout text is byte-identical to today **except** for the documented `gitty.toml` → `.gitty/config` replacement in usage text. Document the diff in the PR description.

**Checkpoint**: `go build ./...` and `go vet ./...` pass. The `gitty` binary built from the new layout produces identical CLI behavior to the pre-cleanup binary, modulo the FR-010 stream split (planning lines on stdout; progress/errors on stderr) and the FR-009 usage-text change. **All three user-story phases can now begin in any order.**

---

## Phase 3: User Story 1 — Locate the source of any user-visible behavior fast (Priority: P1) 🎯 MVP

**Goal**: A contributor unfamiliar with the codebase can identify the file responsible for any user-visible behavior in <5 minutes by reading the layout (SC-001).

**Independent Test**: Hand a new contributor the five behaviors listed in spec User Story 1 (token resolution, dry-run, group fetch, clone-vs-pull, config load) and ask them to point to the file. With Phase 2 done, navigation works; this phase delivers the *self-documenting* layer that makes the layout obvious without code-reading.

### Implementation for User Story 1

- [X] T100 [US1] Add a top-of-file package doc comment to each of the six new files (`internal/cli/cli.go`, `internal/config/config.go`, `internal/gitlabapi/client.go`, `internal/paths/paths.go`, `internal/gitexec/gitexec.go`, `internal/sync/sync.go`) — one short paragraph per FR-013 stating the package's responsibility in the words used by FR-001 (CLI surface, config persistence, GitLab data access, etc.) so `go doc` and editor hovers show it.
- [X] T101 [US1] Add an "Architecture" section to [README.md](../../README.md) (after the existing "Configuration" section, before "Usage") that contains the 30-second tour ASCII tree from [quickstart.md](quickstart.md) and a one-line link to the quickstart for full detail (FR-013).
- [X] T102 [US1] Verify SC-001 manually: open the layout cold and confirm each of {CLI parsing, config persistence, GitLab API, path building, git invocation, sync orchestration} is unambiguously named in the top-level layout (the package names already do this; this task is the conscious sign-off, not a code change). Record findings in the PR description.
- [X] T103 [US1] Verify SC-002: grep the codebase for duplicate token resolution (`GITLAB_TOKEN`), duplicate pagination loops (`opts.Page = resp.NextPage`), and duplicate path building (`strings.TrimPrefix(.*RootPath`) — confirm each pattern appears in exactly one file. Record the grep outputs in the PR description.

**Checkpoint**: User Story 1 fully delivered. A first-time reader can navigate the project from the layout alone.

---

## Phase 4: User Story 2 — Replace or test a single collaborator without rewriting the rest (Priority: P1)

**Goal**: Each responsibility unit is independently testable, with a seed test demonstrating the boundary works in isolation (SC-005, SC-008).

**Independent Test**: `go test ./...` passes from a clean checkout with no network access and no `git` binary on PATH (SC-008).

### Tests for User Story 2 (FR-011 — REQUIRED for this feature)

> Per spec clarification Q3, one seed `*_test.go` per internal package. Pure stdlib `testing`; no testify.

- [X] T200 [P] [US2] Create `internal/paths/paths_test.go` with a table test covering five inputs: `(apiFullPath="tenant/images/api", configRoot="tenant")` → `"images/api"`; `(apiFullPath="tenant/images/api", configRoot="")` → `"tenant/images/api"`; `(apiFullPath="tenant", configRoot="tenant")` → `""`; `(apiFullPath="tenant/images/api", configRoot="tenant/")` → `"images/api"` (trailing slash on root); `(apiFullPath="other/repo", configRoot="tenant")` → `"other/repo"` (non-prefix). Locks in FR-005 behavior parity.
- [X] T201 [P] [US2] Create `internal/config/config_test.go` with a round-trip test: in `t.TempDir()`, call `Save(dir, &Config{URL:"https://x", HTTP:true, RootPath:"a/b"})`, then `Load(dir)`, assert all three fields match. Add a second sub-test asserting `Load` on an empty tmpdir returns an error whose message contains `"no .gitty/config"` and `"gitty init"` (per [contracts/workspace.md](contracts/workspace.md)).
- [X] T202 [P] [US2] Create `internal/gitexec/gitexec_test.go` containing a `fakeRunner` struct that implements `Runner` and records its calls into a slice. Demonstrate that any code accepting a `Runner` interface can be exercised against the fake — no `git` binary needed. (This test exists primarily as the executable example future contributors copy when adding tests for sync code paths.)
- [X] T203 [P] [US2] Create `internal/gitlabapi/client_test.go` containing a `fakeClient` struct implementing the four-method `Client` interface, returning fixture `[]*Group` and `[]*Project` slices. Assert that consumers of `Client` (driven by a small test harness in this file) get the expected slice without HTTP. Note: this test does NOT exercise `Real`; the `Real` constructor is exercised only by `go vet` and the smoke check in T045.
- [X] T204 [P] [US2] Create `internal/sync/sync_test.go` with two tests: (1) `TestResolveToken` — table covering explicit-wins, GITLAB_TOKEN-fallback, CI_JOB_TOKEN-fallback, and all-empty-error cases; (2) `TestSync_Apply` — build a `Plan` against an in-test fake `Client` (~2 groups, ~2 projects), `Apply` with the fake `Runner` from `gitexec_test` (or a local copy) and `t.TempDir()` as cwd, assert: the expected directories exist, the expected `Runner.Run` invocations were recorded in order, and zero bytes were written to stdout (since this test runs `Apply`, not `Render`).
- [X] T205 [P] [US2] Create `internal/cli/cli_test.go` driving the eight argv-row test fixtures in [contracts/cli.md](contracts/cli.md) §"Test-fixture inputs". Use `bytes.Buffer` for stdout/stderr; assert the return code, the buffer content for usage cases, and (for the sync cases that need it) inject env-var lookup via a closure so the test runs without `GITLAB_TOKEN` set in the parent process. The two sync-success rows can be asserted by injecting a fake `Client`/`Runner` through a test seam in `internal/cli` (add a small unexported `mainWithDeps` helper that `Main` delegates to, taking `Client`/`Runner` constructors).

### Verification for User Story 2

- [X] T210 [US2] From a clean shell with `PATH=/usr/bin:/bin` (or any PATH that excludes `git`) and no network, run `go test ./...`. Confirm it passes (SC-008). Record the command and exit code in the PR description.
- [X] T211 [US2] Confirm `go.mod` and `go.sum` did not gain any module entries during this phase (FR-012, SC-004). `git diff go.mod go.sum` should be empty.

**Checkpoint**: User Story 2 fully delivered. Boundaries are not just structural but provably independent — every package has at least one test that exercises it through its public surface only.

---

## Phase 5: User Story 3 — Trust the documented config contract (Priority: P2)

**Goal**: README, code, and Constitution all reference the workspace config as `.gitty/config` (SC-006). No `gitty.toml` mentions remain anywhere except historical changelog/release-note files.

**Independent Test**: `grep -ri "gitty\.toml" .` from repo root returns matches only in changelog/release-note files (or zero matches if no such files exist yet).

### Pre-merge dependency (separate command)

- [X] T300 [US3] Run `/speckit-constitution` separately to amend Constitution Principle III, replacing every `gitty.toml` with `.gitty/config`. Bump version 1.0.0 → 1.0.1 (PATCH per the Constitution's versioning policy: "wording clarifications, typo fixes, non-semantic refinements"). Update the Sync Impact Report. **This task lands as a separate PR; merge it before merging the architecture cleanup PR.** See research R-010 for rationale.

### Implementation for User Story 3

- [X] T301 [US3] Update [README.md](../../README.md) §Features bullet "**Workspace Config**": rewrite the parenthetical and any inline references so the file is referred to as `.gitty/config` (with the directory being `.gitty/`) instead of `gitty.toml`.
- [X] T302 [US3] Update [README.md](../../README.md) §Configuration (`gitty init`): rewrite the line "This generates a `gitty.toml` file..." to "This generates a `.gitty/config` file in `<cwd>/.gitty/`...". Adjust any other `gitty.toml` mentions in this section.
- [X] T303 [US3] Update the example pipeline in [README.md](../../README.md) §"GitLab CI/CD Pipeline" if it references the config filename in any comment or echo.
- [X] T304 [US3] Confirm the usage text printed by `gitty` (via `internal/cli/cli.go::usage`) says `.gitty/config` (FR-009). Already addressed in T032 — this task is the verification.
- [X] T305 [US3] Run `grep -rn "gitty\.toml" /Users/tylercarper/Documents/dev/gitty/ --exclude-dir=.git --exclude-dir=specs` and confirm zero matches outside of historical changelog/release notes (none exist today, so result MUST be zero matches). The `specs/` exclusion is intentional — historical spec text may reference the old name. Record the grep output in the PR description.

**Checkpoint**: User Story 3 fully delivered. The doc/code/Constitution three-way disagreement is resolved.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Final verifications across all stories. SC-003 and SC-007 belong here because they require the cleanup to be fully assembled.

**Note (deferred to user, 2026-05-09):** T400 and T401 require a real GitLab group + network and were intentionally deferred during `/speckit-implement`. The user will run them against a disposable target; the unit tests in Phase 4 already lock in the stream-split behavior offline.

- [ ] T400 Verify SC-003: against an existing local sync target (or a small disposable GitLab group), run every example invocation from [README.md](../../README.md) §Examples with the cleaned-up binary, then with the pre-cleanup binary (built from `git stash` or a sibling checkout), and `diff -r` the resulting workspace trees. Difference MUST be zero. Record the command sequence and `diff` exit code in the PR description.
- [ ] T401 Verify SC-007: run `gitty sync --path=<small-group> --dry-run >stdout.log 2>stderr.log` against a disposable target. Confirm `stdout.log` contains the plan lines (`would create / would clone / would pull`) and `stderr.log` contains progress/banners (and zero plan lines). Run a real sync against an already-synchronized target (no-op) and confirm `stderr.log` contains only banner/count lines (idempotency holds per Constitution Principle I).
- [X] T402 [P] Final pass of `go build ./...`, `go vet ./...`, `go test ./...` from repo root. All three MUST be green.
- [X] T403 [P] Confirm the PR description includes: (a) the FR-009 doc-rename diff, (b) the FR-010 stream-split note explaining stdout/stderr behavior change, (c) the SC-003 byte-equality evidence, (d) the SC-008 offline-tests evidence, (e) a callout that `--verbose`, prefixes, and credential redaction are explicitly out of scope and tracked for a follow-up spec.
- [X] T404 [P] Confirm the Constitution Principle III amendment from T300 has merged (or is queued to merge first). Architecture cleanup PR MUST NOT merge before the Constitution amendment.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: T001 must run first; T002–T007 can be parallel after T001.
- **Phase 2 (Foundational)**: Depends on Phase 1. Hard internal ordering:
  - Phase 2a (T010–T014) — leaves, all parallel after Phase 1.
  - Phase 2b (T020–T022) — depends on Phase 2a.
  - Phase 2c (T030–T032) — depends on Phase 2b.
  - Phase 2d (T040–T045) — depends on Phase 2c.
- **Phases 3, 4, 5 (User Stories)**: All depend on Phase 2 completion. Once Phase 2 is done, the three user-story phases are independent and may proceed in parallel (or sequentially in priority order P1 → P1 → P2).
  - Recommendation: do Phase 4 (US2 tests) before merging anything, even though it's not strictly blocking, because the tests will catch FR-008 regressions in Phase 2's behavior-parity claim.
- **Phase 5 task T300** (Constitution amendment) MAY proceed in parallel with all other phases — it's a separate `/speckit-constitution` PR with no code dependency. It MUST land before the cleanup PR merges.
- **Phase 6 (Polish)**: Depends on Phases 2–5 all complete.

### Within Each User Story

- US1 tasks T100–T103 can each be done in any order after Phase 2; they are independent doc/verification touches.
- US2 tasks T200–T205 are all parallel-safe (different files); T210–T211 verify after they all land.
- US3 task T300 is independent (separate PR). T301–T303 are different sections of README.md so they should be sequenced (one editor pass), then T304 (verification) and T305 (grep audit).

### Parallel Opportunities

- All Phase 1 tasks except T001 can run in parallel (different directories).
- All Phase 2a leaf-package tasks (T010–T014) can run in parallel.
- All seed-test tasks (T200–T205) can run in parallel.
- T402 / T403 / T404 in Polish can run in parallel.

---

## Parallel Example: Phase 2a (Leaf Packages)

After Phase 1 completes, five contributors (or one contributor in five terminals) can do this in parallel:

```bash
# Each task touches a distinct file under internal/<pkg>/; no shared state.
Task: "T010 — implement internal/paths/paths.go"
Task: "T011 — implement internal/config/config.go"
Task: "T012 — implement internal/gitexec/gitexec.go"
Task: "T013 — implement internal/gitlabapi/client.go"
Task: "T014 — implement internal/gitlabapi/paginate.go"
```

Note: T013 and T014 both touch the `internal/gitlabapi` package, so while their files are distinct, they need to be reconciled before `go vet` is clean (`Real`'s methods in T013 reference `paginate` from T014). Run `go vet ./...` after both land before declaring Phase 2a complete.

## Parallel Example: Phase 4 (Seed Tests)

```bash
Task: "T200 — internal/paths/paths_test.go"
Task: "T201 — internal/config/config_test.go"
Task: "T202 — internal/gitexec/gitexec_test.go"
Task: "T203 — internal/gitlabapi/client_test.go"
Task: "T204 — internal/sync/sync_test.go"
Task: "T205 — internal/cli/cli_test.go"
```

All six tests live in different packages and have no shared fixtures.

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Complete Phase 1 (Setup).
2. Complete Phase 2 (Foundational) — this is where the architecture moves; the binary now behaves identically in the new layout.
3. Complete Phase 3 (US1 — README architecture section + per-package doc comments).
4. **STOP and VALIDATE**: A first-time contributor can navigate the codebase and find any user-visible behavior in <5 minutes (SC-001, SC-002).
5. Open PR with this scope; ship as the MVP.

### Incremental Delivery (recommended)

1. Phase 1 + Phase 2 — internal layout move with byte-identical CLI behavior; no test files, no doc updates yet.
2. Phase 4 (US2) — seed tests land. Lock in FR-008 / FR-010 behavior. (Doing US2 before US1 docs is a sequencing choice: tests catch any Phase 2 mistakes before the docs ship.)
3. Phase 3 (US1) — README architecture section + doc comments.
4. Phase 5 (US3) — README rename + Constitution amendment via T300.
5. Phase 6 — final verifications.

### Single-PR Delivery (also valid per spec)

The spec allows the entire cleanup to land as one PR. In that case execute every phase in the order above and run Phase 6 at the end. Every commit MUST keep `go build`, `go vet`, and `go test ./...` green (per spec assumptions).

---

## Notes

- [P] tasks operate on different files with no shared state.
- [Story] label maps tasks to spec user stories for traceability.
- The Constitution amendment (T300) is a separate `/speckit-constitution` invocation, not a code change in this branch — it MUST land before this PR merges.
- The `--verbose` flag, stable per-event-class prefixes, ANSI handling, and credential redaction (the rest of Constitution Principle II) are explicitly OUT of scope per spec FR-010. A follow-up spec captures them.
- Verify tests pass after each phase before moving to the next.
- Avoid: any new Go module dependency (FR-012), any change to flag names/defaults/exit codes (FR-008), any change to the on-disk directory layout (FR-008, Constitution Principle III).

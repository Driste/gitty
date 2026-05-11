# Phase 0 Research: Architecture Cleanup

**Date**: 2026-05-09  **Plan**: [plan.md](./plan.md)  **Spec**: [spec.md](./spec.md)

The spec entered Phase 0 with **zero open `[NEEDS CLARIFICATION]` markers** —
all three were resolved during `/speckit-specify` clarification (Q1: keep
`.gitty/config`; Q2: align stdout/stderr now; Q3: seed test per unit). The
research below covers the decisions a maintainer would otherwise have to make
mid-implementation: how exactly to slice the existing code, what shape the
narrow GitLab interface takes, how to fan out git execution, and how to write
seed tests without adding a test framework.

## R-001 — Package boundary granularity

**Decision**: Six `internal/` packages: `cli`, `config`, `gitlabapi`, `paths`,
`gitexec`, `sync`.

**Rationale**:

- The spec's FR-001 enumerates exactly six responsibilities. One package per
  responsibility makes the spec→code mapping a rename, not a translation.
- Each of the six is independently testable (FR-002): `paths` is pure string
  logic; `config` is pure I/O against a test tmpdir; `gitlabapi` exposes an
  interface so a fake satisfies it without HTTP; `gitexec` is a one-method
  type so a fake substitutes it; `cli` parses argv to a struct (no I/O); `sync`
  composes the others through interfaces.
- `internal/` (rather than top-level packages) prevents accidental external
  consumption — Gitty is a binary, not a library.

**Alternatives rejected**:

- *Multi-file `package main`*: cheaper boilerplate, but FR-002 testability
  becomes a developer-discipline guarantee instead of a compiler-enforced one.
  Rejected per Complexity Tracking row 3 in the plan.
- *Three packages (`cli`, `core`, `gitlabapi`)*: more cohesive but bundles
  paths/gitexec/sync into one `core`, which is exactly the entanglement the
  feature exists to remove. Rejected.
- *Per-subcommand packages (`internal/initcmd`, `internal/synccmd`)*: scales
  badly if subcommands grow; obscures shared concerns (token resolution,
  config load) by duplicating them. Rejected.

## R-002 — Shape of the narrow GitLab interface (FR-003)

**Decision**: A four-method `gitlabapi.Client` interface, returning POGO
("plain old Go object") types defined in this package — not the upstream
`*gitlab.Group` / `*gitlab.Project`.

```go
package gitlabapi

type Group struct {
    FullPath string
    Name     string
}

type Project struct {
    PathWithNamespace string
    SSHURLToRepo      string
    HTTPURLToRepo     string
}

type Client interface {
    GetGroup(path string) (*Group, error)
    ListSubGroups(parent string) ([]*Group, error)         // immediate children only
    ListDescendantGroups(parent string) ([]*Group, error)  // recursive
    ListGroupProjects(parent string, recursive bool) ([]*Project, error)
}
```

**Rationale**:

- Returning local types insulates `internal/sync` (and its tests) from the
  upstream `client-go` SDK. The fake in tests doesn't need to construct
  `*gitlab.Group` with all its unexported fields.
- Exactly four methods cover every GitLab call in today's `sync.go`. No
  speculative methods.
- All pagination collapses inside the concrete `Real` implementation
  (FR-006), so the interface stays flat.

**Alternatives rejected**:

- *Re-export upstream types directly*: zero conversion code but couples every
  consumer (and every test) to the SDK. Rejected — defeats FR-003's purpose.
- *Generic `Client.List[T any]`*: Go generics make this expressible but the
  callers in `internal/sync` aren't generic, so the savings are negative once
  you count the type assertions. Rejected.

## R-003 — Pagination dedup (FR-006)

**Decision**: One internal helper in `internal/gitlabapi/paginate.go`:

```go
func paginate[T any](
    fetch func(opts gitlab.ListOptions) ([]T, *gitlab.Response, error),
) ([]T, error)
```

Each of the three list methods (`ListSubGroups`, `ListDescendantGroups`,
`ListGroupProjects`) supplies a one-line `fetch` closure that calls the
matching SDK method.

**Rationale**:

- The current `sync.go` has three identical pagination loops. The helper
  reduces them to closures and makes the page-size constant (`PerPage: 100`)
  live in exactly one place.
- Generics keep the helper type-safe; no `interface{}` shuffling.

**Alternatives rejected**:

- *Reflection-based pager*: works for any type, but reflection is heavier
  than a 10-line generic function for this domain. Rejected.
- *Manual loop per endpoint, accepting duplication*: status quo. Rejected by
  FR-006.

## R-004 — Git execution wrapper (FR-007)

**Decision**: `internal/gitexec` exposes a `Runner` type with one method:

```go
package gitexec

type Runner interface {
    Run(dir string, args ...string) error
}

type Real struct {
    Stdout, Stderr io.Writer
    Env            []string  // defaults to os.Environ() when nil
}

func (r *Real) Run(dir string, args ...string) error { ... }
```

Today's `runGit` becomes `(&Real{Stdout: os.Stdout, Stderr: os.Stderr}).Run(...)`,
called from `internal/sync`. Tests use a fake `Runner` that records invocations.

**Rationale**:

- Single replaceable surface (FR-007) — every call site goes through `Run`.
- Env-passthrough behavior is preserved by defaulting `Env` to `os.Environ()`
  when nil, matching today's behavior (SSH agent socket, global gitconfig).
- Stdout/Stderr are wired via the struct so the `cli` package can pass through
  the real `os.Stdout`/`os.Stderr` while tests can pass a `bytes.Buffer`.

**Alternatives rejected**:

- *Function variable `var GitRun = func(...) error`*: stub-by-monkey-patch.
  Rejected — interfaces compose better with the `sync.Deps` struct.
- *Embed exec.Cmd directly in tests with `exec.LookPath`*: requires a real git
  binary on PATH; SC-008 explicitly forbids that. Rejected.

## R-005 — Output stream split (FR-010)

**Decision**: All packages that today print via `fmt.Printf` accept an
`io.Writer` for `out` (stdout) and `errOut` (stderr) and use them
explicitly. Specifically:

- `--dry-run` plan lines (`would create…`, `would clone…`, `would pull…`) → `out`.
- All progress lines (`Fetching subgroups for…`, `Found N groups…`,
  `Processing X…`, `Cloning X to Y`, `--- Syncing Groups ---` banners) → `errOut`.
- All errors (today: `log.Fatalf`) → `errOut`, with the process exit code
  surfaced through `cli.Main`'s int return value rather than calling
  `os.Exit` from inside library packages.

**Rationale**:

- Constitution Principle I requires diagnostics on stderr; planning output
  on stdout is the natural mate so users can pipe `--dry-run | wc -l` etc.
- Replacing `log.Fatalf` with returned errors makes `cli.Main` testable
  without intercepting `os.Exit`.

**Alternatives rejected**:

- *Use a single logger interface (slog or custom)*: bigger surface than
  needed and a deferred-Principle-II concern (per the spec, `--verbose` and
  prefixes are out of scope for this PR). Rejected — keep this PR small.

## R-006 — Token resolution location (FR-004)

**Decision**: `internal/sync.ResolveToken(explicit string, lookup func(string) string) (string, error)`.

The `lookup` parameter is `os.Getenv` in production and a closure over a map
in tests. Token resolution moves out of `runSync` (where it currently lives,
inline) into a single dedicated function.

**Rationale**:

- Pure function with one dependency (env lookup) injected — FR-004's "one
  function, unit-testable" requirement satisfied.
- The order (`explicit` → `GITLAB_TOKEN` → `CI_JOB_TOKEN`) is a constitution-
  pinned contract (Principle III); centralizing it makes that contract
  trivially auditable.

**Alternatives rejected**:

- *Move into `cli` package*: violates separation — token is a sync concern,
  not a flag-parsing concern. CLI just collects the explicit `--token`
  string. Rejected.
- *Move into `internal/gitlabapi`*: would mean the GitLab package reads env
  vars, which couples a pure data-access package to the process environment.
  Rejected.

## R-007 — `cli.Main` signature

**Decision**:

```go
package cli

func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int
```

`main.main` becomes a one-liner: `os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))`.

**Rationale**:

- This is the standard Go pattern for testable CLIs (used by `kubectl`,
  `cobra`-based tools, `gh`, etc.).
- All exit codes are returned, not called via `os.Exit` from deep call sites.
- Tests can drive end-to-end CLI behavior with `bytes.Buffer` for output and
  capture the return value.

**Alternatives rejected**:

- *Keep `flag.FlagSet` parsing inline in `main()`*: testable only by spawning
  the binary. Rejected.

## R-008 — Seed test scope per unit (FR-011)

**Decision**: One `*_test.go` per package, ~6 total:

| Package      | Seed test                                                                                                | Verifies                                          |
| ------------ | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------- |
| `paths`      | Table test: 5 inputs covering rooted, unrooted, trailing slash, empty config root, exact-match.          | FR-005, behavior parity                           |
| `config`     | Round-trip Save→Load in `t.TempDir()`; assert all fields equal.                                          | FR-002 (no git/network needed)                    |
| `gitexec`    | Hand-written fake `Runner` in test; sync's git-call code path can be exercised against it.               | FR-007 (interface-driven)                         |
| `gitlabapi`  | Hand-written fake `Client`; one fetch produces the expected `[]Project` slice.                           | FR-003                                            |
| `sync`       | `Sync(req, deps)` with fake `Client` + fake `Runner` + `t.TempDir()` produces the expected directory tree and the expected (recorded) git invocations. Also tests `ResolveToken` order. | FR-004, FR-007, end-to-end orchestration testable |
| `cli`        | `cli.Main` with crafted `args` and `bytes.Buffer` for stdout/stderr returns expected exit code and writes expected lines to the right stream. | FR-008, FR-010                                    |

**Rationale**:

- One test per package both exercises the boundary and serves as the
  copy-paste template for future contributors (FR-011).
- All tests use only the standard `testing` package — no testify, no mock
  framework — keeping FR-012 satisfied.

**Alternatives rejected**:

- *Use testify/assert*: nicer assertions, but a new dependency. Rejected.
- *Skip the `cli` and `sync` seed tests because they're "integration"*: but
  these are the two highest-value boundaries to demonstrate (the whole point
  of the cleanup). Rejected.

## R-009 — Migration safety for the `.gitty/config` rename

**Decision**: No on-disk migration is needed. The path `<workspace>/.gitty/config`
is unchanged — only the README and Constitution change to match. A user with an
existing workspace from the pre-cleanup binary continues to have a valid
workspace under the cleaned-up binary.

**Rationale**:

- The clarification Q1 result was "keep `.gitty/config` (amend docs)". The
  code already writes that path; no rename happens.
- The edge case in spec.md ("contributor on existing workspace gets a clear
  error if layout is unrecognized") is about *defensive messaging*, not
  migration: if a hypothetical future user has only a stray `gitty.toml` in
  their dir but no `.gitty/config`, the error should say "no `.gitty/config`
  found; run `gitty init`" rather than a bare "file not found." This is
  improved error wording inside `internal/config`'s `Load` — a one-line change.

**Alternatives rejected**:

- *Build a migration tool that detects `gitty.toml` and renames*: there are no
  `gitty.toml` files in the wild (the README documented one but the code never
  wrote one), so a migration tool would be solving a nonexistent problem.
  Rejected.

## R-010 — Constitution amendment plan

**Decision**: Land a separate `/speckit-constitution` PATCH bump (1.0.0 → 1.0.1)
that rewrites every `gitty.toml` mention in Principle III to `.gitty/config`,
**before** the architecture cleanup PR merges. The amendment PR is small
(string-replace + Sync Impact Report) and has no code dependency on the
cleanup, so it can land first or in parallel.

**Rationale**:

- Aligns with Constitution Governance §Versioning policy: "PATCH: Wording
  clarifications, typo fixes, non-semantic refinements."
- Decouples the wording fix from the code refactor so each PR has a clear
  blast radius.

**Alternatives rejected**:

- *Bundle the constitution change into the cleanup PR*: easier sequencing but
  mixes governance changes with code changes, complicating review. Rejected.
- *Defer the amendment forever and just live with inconsistency*: defeats the
  feature's User Story 3. Rejected.

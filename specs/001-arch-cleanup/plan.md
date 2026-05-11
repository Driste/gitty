# Implementation Plan: Architecture Cleanup

**Branch**: `001-arch-cleanup` | **Date**: 2026-05-09 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/001-arch-cleanup/spec.md`

## Summary

Reorganize Gitty's flat-file `package main` codebase into a small set of
narrowly-scoped, independently-testable units. Today every responsibility (CLI
parsing, config persistence, GitLab API access, namespace→path resolution, git
shell-out, and sync orchestration) is interleaved across `main.go`, `init.go`,
`sync.go`, and `config.go` — `runSync` alone owns five of those concerns.
After the cleanup, each concern lives in its own `internal/<name>` package, the
GitLab data surface is reduced to a small interface so a fake can be substituted
in tests, output is split correctly between stdout (plan/result) and stderr
(progress/errors), the documented config layout (`.gitty/config`) is reflected
consistently across code, README, and Constitution (via a separate
`/speckit-constitution` PATCH amendment), and a seed `*_test.go` file lands per
package proving each boundary works in isolation. No new external Go modules,
no new flags, no flag/exit-code/on-disk-layout changes.

## Technical Context

**Language/Version**: Go 1.24.4 (per `go.mod`)
**Primary Dependencies**: `github.com/pelletier/go-toml/v2` v2.3.0,
`gitlab.com/gitlab-org/api/client-go` v1.46.0 — both retained, none added.
**Storage**: Local filesystem only — TOML at `<workspace>/.gitty/config`,
plus the cloned-repo trees the user materializes via `gitty sync`.
**Testing**: Go standard `testing` package, run via `go test ./...`. No new
test-only dependencies (no testify, no mock framework — table tests + tiny
hand-written fakes per the FR-003 narrow interface).
**Target Platform**: macOS and Linux on `amd64` and `arm64` (per Constitution
§ Additional Constraints). Windows best-effort, untested.
**Project Type**: cli (single binary, no library/service consumers).
**Performance Goals**: Not a target of this refactor. Refactor MUST NOT regress
the existing wall-clock behavior of `gitty sync` (sequential pulls remain
sequential — async is a separate spec).
**Constraints**:

- No new external Go module dependencies (FR-012, Constitution Principle II).
- CLI flags, defaults, exit codes, and on-disk layout MUST be byte-identical
  before and after (FR-008, Constitution Principle I).
- Stdout/stderr separation MUST move toward Constitution Principle I:
  planning lines (`--dry-run` output) → stdout; progress/errors → stderr.
- Every intermediate commit MUST keep `go build ./...`, `go vet ./...`, and
  `go test ./...` green.

**Scale/Scope**: ~310 LOC of Go today across 4 files. Expected post-cleanup:
~350–400 LOC across ~10–12 files in 6 packages plus `main` (modest growth from
package boilerplate, offset by deduplicated pagination/path code).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle                                       | Applies? | This plan's posture                                                                                                                                                                                                                                            | Verdict                                                                                                                |
| ----------------------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| **I. CLI-First UX & Safe-by-Default Sync**      | Yes      | FR-008 forbids any flag/exit-code/layout change. FR-010 brings stdout/stderr into compliance for the streams currently emitted. Idempotency, dry-run parity, and non-destructive defaults are unchanged.                                                       | ✅ Passes — strictly preserves and partially strengthens compliance.                                                   |
| **II. Minimal Dependencies & Structured Observability** | Yes      | FR-012 forbids new Go modules. Pre-existing non-compliance with Principle II's *output contract* (no `--verbose`, no per-event-class prefixes, no credential redaction) is **not made worse** by this plan; full compliance is deferred to a follow-up spec. | ⚠ Partial compliance preserved — see Complexity Tracking entry "Deferred Principle II output contract." Not a *new* violation introduced by this plan. |
| **III. Config-Anchored Workspaces**             | Yes      | FR-009 keeps the on-disk layout (`.gitty/config`) and resolves the README/Constitution/code three-way disagreement by amending docs, not data. The amendment to Principle III is a pre-merge dependency tracked in Complexity Tracking.                  | ✅ Passes after the dependent constitution amendment lands.                                                            |

**Initial verdict (pre-design):** Pass with one tracked dependency (constitution
amendment) and one tracked carry-over (deferred Principle II output contract).
Re-checked post-Phase 1 below — still passes.

## Project Structure

### Documentation (this feature)

```text
specs/001-arch-cleanup/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── cli.md           # gitty CLI argv schema (subcommands, flags, exit codes)
│   └── workspace.md     # .gitty/config TOML schema and discovery rules
├── checklists/
│   └── requirements.md  # Spec quality checklist (created by /speckit-specify)
├── spec.md              # Feature specification (created by /speckit-specify)
└── tasks.md             # Phase 2 output (created by /speckit-tasks)
```

### Source Code (repository root)

```text
gitty/
├── main.go                 # Thin entrypoint: calls cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
├── go.mod                  # Unchanged dependency set
├── go.sum                  # Unchanged
├── README.md               # Updated: .gitty/config name, "Architecture" section pointing at internal/
├── LICENSE                 # Unchanged
├── CLAUDE.md               # Updated by Phase 1 to point at this plan
├── .specify/               # Spec-kit data; unchanged
└── internal/
    ├── cli/                # FR-001(a): flag parsing + subcommand dispatch
    │   ├── cli.go          #   Main(args, stdin, stdout, stderr) -> int
    │   ├── init_cmd.go     #   `gitty init` flag set + handler wiring
    │   ├── sync_cmd.go     #   `gitty sync` flag set + handler wiring
    │   └── cli_test.go     # Seed test: argv parsing produces the expected request struct
    ├── config/             # FR-001(b): workspace config persistence
    │   ├── config.go       #   Config struct, Load(dir), Save(dir, *Config), file path constants
    │   └── config_test.go  # Seed test: round-trip Save then Load preserves all fields
    ├── gitlabapi/          # FR-001(c): GitLab data access (concrete impl + interface)
    │   ├── client.go       #   Client interface (FR-003) + concrete Real struct wrapping go-gitlab
    │   ├── paginate.go     #   Single generic pagination helper used by all list endpoints (FR-006)
    │   └── client_test.go  # Seed test: a fake Client satisfying the interface drives a single fetch path end-to-end
    ├── paths/              # FR-001(d): namespace → local path resolution
    │   ├── paths.go        #   Local(apiFullPath, configRoot string) string
    │   └── paths_test.go   # Seed test: table covering rooted, unrooted, and trailing-slash inputs (current behavior preserved)
    ├── gitexec/            # FR-001(e): git binary invocation
    │   ├── gitexec.go      #   Runner type with Run(dir string, args ...string) error; preserves env passthrough
    │   └── gitexec_test.go # Seed test: Runner can be substituted by a fake (no real git binary needed)
    └── sync/               # FR-001(f): orchestration that composes the above
        ├── sync.go         #   Sync(req Request, deps Deps) error; deps wires Client/Runner/io.Writers
        ├── token.go        #   ResolveToken(explicit string, env Lookup) (string, error) — FR-004
        └── sync_test.go    # Seed test: Sync with fake Client + fake Runner + tmp dir produces expected directory tree
```

**Structure Decision**: Six `internal/` packages (`cli`, `config`, `gitlabapi`,
`paths`, `gitexec`, `sync`) plus a thin `main.go`. Subpackages were chosen over
multi-file `package main` for one reason: with subpackages the **import graph
enforces the boundary** — `internal/paths` cannot accidentally call into
`internal/gitlabapi`, and `internal/sync` can only see its `Deps` struct, not
concrete clients. With a single `main` package this discipline relies entirely
on developer attention. Given the explicit testability requirement (FR-002,
FR-003, FR-005, SC-005, SC-008), the import-graph guarantee pays for the small
amount of package-boilerplate cost.

## Complexity Tracking

| Item | Why it exists | Simpler alternative considered & rejected because |
| --- | --- | --- |
| **Pre-merge dependency: Constitution Principle III amendment** | FR-009 resolves a three-way doc/code/Constitution disagreement by changing the docs to match the code. Principle III currently says `gitty.toml`; it must say `.gitty/config` before this PR can merge without re-introducing the inconsistency this feature exists to fix. | (a) Rename the on-disk file to `gitty.toml` instead — rejected because user chose the `.gitty/config` option in /speckit-specify clarification Q1 (preserves directory for future per-workspace state). (b) Land the cleanup with Constitution still saying `gitty.toml` — rejected because it leaves the same inconsistency the feature is supposed to remove. |
| **Deferred Principle II output contract** | Spec FR-010 (clarification Q2) explicitly scopes output changes to *stream destination only*. The remaining Principle II requirements (`--verbose` flag, per-event-class prefixes, ANSI handling, credential redaction) are deferred to a follow-up spec. | Bringing all of Principle II's output contract into this PR was the alternative — rejected by the user in Q2 to keep blast radius small. Note: this plan does NOT regress observability; today's code already lacks these things, so deferral preserves status quo. |
| **Six packages instead of one `package main` with multiple files** | The spec demands testability of each unit in isolation (FR-002, FR-005, SC-005). Subpackages make the boundary a compiler-enforced fact instead of a convention. | Multi-file `package main` was the simpler alternative — rejected because it cannot prevent the kind of cross-cutting entanglement that exists in today's `runSync`. The boilerplate cost is small (one `package` line per file, a few exported names per package). |

## Re-evaluation After Phase 1

After producing `research.md`, `data-model.md`, `contracts/`, and
`quickstart.md`, the design has not introduced any new dependency, flag, or
behavior. The interface surface (`gitlabapi.Client` with 4 methods, `gitexec.Runner`
with 1 method) is the minimum required to make sync orchestration testable per
FR-003 and FR-007. All three Constitution principles remain in the same posture
as the initial check. **Final verdict: gates pass, plan is ready for `/speckit-tasks`.**

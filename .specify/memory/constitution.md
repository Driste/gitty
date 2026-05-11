<!--
SYNC IMPACT REPORT
==================
Version change: 1.0.0 → 1.0.1
Bump rationale: PATCH — wording correction in Principle III. The workspace
config file is `.gitty/config` (a TOML file inside a `.gitty/` directory),
matching the code that has shipped since the project's initial commit.
Earlier text named it `gitty.toml`, which never existed on disk. No
semantic changes; principle scope, governance, and quality gates are
unchanged.

Modified principles:
  - III. Config-Anchored Workspaces — every `gitty.toml` reference replaced
    with `.gitty/config`. No change in meaning.

Added sections:
  - (none)

Removed sections:
  - (none)

Templates requiring updates:
  - ✅ .specify/templates/plan-template.md  (no references to the file name)
  - ✅ .specify/templates/spec-template.md  (no references)
  - ✅ .specify/templates/tasks-template.md (no references)
  - ✅ .specify/templates/checklist-template.md (no references)
  - ✅ README.md (updated in feature 001-arch-cleanup to use .gitty/config)
  - ✅ CLAUDE.md (delegates to current plan; no constitution references)

Follow-up TODOs:
  - (none)

Prior history:
  - 1.0.0 (2026-03-31, ratified): First ratified constitution. Principles
    I/II/III established along with Additional Constraints, Development
    Workflow & Quality Gates, and Governance.
-->

# Gitty Constitution

## Core Principles

### I. CLI-First UX & Safe-by-Default Sync

The CLI is the only public interface. Commands MUST be predictable, composable, and
non-destructive by default.

- Stdin/args → stdout for primary output; diagnostics → stderr; non-zero exit on failure.
- Sync operations MUST be idempotent: re-running the same command on a synchronized
  workspace MUST NOT mutate state. Existing repos pull; missing repos clone; nothing
  else is modified.
- Every operation that creates, deletes, or mutates filesystem or remote state MUST
  support `--dry-run` and produce identical planning output to the real run.
- Destructive defaults are forbidden. Removal of local repos, force operations, or
  history-rewriting actions MUST require an explicit, named flag — never implicit.
- Flag names, defaults, and exit codes are part of the public contract; breaking
  changes require a MAJOR version bump (see Governance).

**Rationale:** Gitty operates against developer machines and CI runners where mistakes
are expensive (lost work, wiped clones, leaked tokens). Predictability and dry-run
parity are the primary trust contract.

### II. Minimal Dependencies & Structured Observability

Prefer the Go standard library. Every external dependency MUST be justified, and all
runtime behavior MUST be inspectable from the terminal without an external collector.

- Adding a Go module dependency requires a written justification in the PR description:
  what stdlib alternative was considered and why it was insufficient.
- Output destined for humans MUST be greppable: stable prefixes per event class
  (e.g., `clone`, `pull`, `skip`, `error`), one event per line, no decorative ANSI in
  non-TTY contexts.
- A `--verbose` flag MUST exist and MUST expose the underlying `git` invocation,
  resolved URL (with token redacted), and target path for every repo touched.
- Tokens, credentials, and full HTTP Authorization headers MUST never appear in
  any output stream, including verbose and dry-run modes.

**Rationale:** Gitty is a small CLI that often runs unattended in CI. Heavy dependency
trees increase supply-chain risk for a workflow tool, and operators need to reconstruct
what happened from terminal logs alone — there is no dashboard.

### III. Config-Anchored Workspaces

`.gitty/config` (a TOML file inside a `.gitty/` directory at the workspace root) is
the source of truth for a workspace. Flags override config for a single invocation
but MUST NOT silently mutate it.

- Every sync command MUST resolve its workspace root by locating `.gitty/config`;
  running outside an initialized workspace MUST fail with a clear remediation
  message pointing to `gitty init`.
- Flags such as `--url` and `--http` override the config for the current run only.
  Persisting a new value to `.gitty/config` requires an explicit `init`-class command.
- The on-disk directory layout MUST mirror the GitLab namespace path exactly. Renaming,
  flattening, or de-duplicating namespaces is forbidden — namespace collisions are
  prevented by preserving the full path.
- Token resolution order is fixed: explicit `--token` flag, then `GITLAB_TOKEN`, then
  `CI_JOB_TOKEN`. New sources require a constitution amendment.

**Rationale:** Workspace shape is the user's mental model. Drift between config, flags,
and on-disk layout is the failure mode that makes sync tools untrustworthy at scale.

## Additional Constraints

- **Language/runtime:** Go, version pinned in `go.mod`. Code MUST build with `go build`
  using only modules listed in `go.mod`/`go.sum`; no `go generate` step is required for
  a release build.
- **Target platforms:** macOS and Linux on `amd64` and `arm64`. Windows is best-effort.
- **External systems:** GitLab REST API only. GitHub, Bitbucket, or other forges are
  out of scope until a constitution amendment adds them.
- **Authentication:** SSH (default) or HTTPS with token. Token storage on disk is
  forbidden — tokens come from env vars or flag input only.
- **Network behavior:** Long-running operations (clone/pull) MUST be interruptible via
  SIGINT and MUST leave the workspace in a recoverable state (partial clone directory
  may exist; a subsequent run MUST recover or report it cleanly).

## Development Workflow & Quality Gates

- **Plan gate:** Every feature plan MUST include a Constitution Check section that
  enumerates which principles apply and how the feature complies. Violations require
  an entry in the plan's Complexity Tracking table with rejected alternatives.
- **Code review:** PRs MUST be reviewed against this constitution. Reviewers SHOULD
  reject changes that introduce destructive defaults, hidden config mutation, new
  dependencies without justification, or output that leaks credentials.
- **Pre-merge checks:** `go build ./...` and `go vet ./...` MUST pass. Adding tests is
  encouraged but not gated — when added, they MUST run via `go test ./...` with no
  extra setup.
- **Dry-run parity:** Any PR that changes sync behavior MUST demonstrate that
  `--dry-run` output matches the real run's planned actions (manual evidence in the
  PR description is sufficient).
- **Release discipline:** User-visible changes MUST be summarized in the commit body
  or PR description. Flag/exit-code changes MUST be called out explicitly.

## Governance

This constitution supersedes ad-hoc conventions. When a principle conflicts with a
proposed change, the principle wins unless the constitution is amended first.

**Amendments**

- Any change to principles, additional constraints, or quality gates is an amendment
  and MUST be made via PR that updates `.specify/memory/constitution.md` and bumps
  the version below.
- Amendment PRs MUST update the Sync Impact Report comment at the top of this file
  and MUST verify that `.specify/templates/plan-template.md`,
  `.specify/templates/spec-template.md`, and `.specify/templates/tasks-template.md`
  remain consistent.

**Versioning policy** (semantic):

- **MAJOR**: Backward-incompatible removal or redefinition of a principle, or a
  breaking change to the CLI contract (flag removal, exit-code change, default
  behavior reversal).
- **MINOR**: New principle, new constraint, or materially expanded guidance.
- **PATCH**: Wording clarifications, typo fixes, non-semantic refinements.

**Compliance review**

- Reviewers MUST verify constitution compliance during code review.
- The plan template's Constitution Check is the primary enforcement point at design
  time; PR review is the enforcement point at implementation time.

**Runtime guidance**

- `README.md` documents user-facing behavior and MUST stay consistent with Principles
  I and III. `CLAUDE.md` delegates to the active plan in `specs/` for feature-specific
  context.

**Version**: 1.0.1 | **Ratified**: 2026-03-31 | **Last Amended**: 2026-05-09

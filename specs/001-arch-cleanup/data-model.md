# Phase 1 Data Model: Architecture Cleanup

**Date**: 2026-05-09  **Plan**: [plan.md](./plan.md)  **Spec**: [spec.md](./spec.md)

This refactor introduces no new persistent state — the only on-disk artifact
remains `<workspace>/.gitty/config`. What it *does* introduce is a small set
of in-process value types that name the entities the spec refers to (Workspace
Config, GitLab Namespace, GitLab Project, Sync Plan) and replace today's
practice of passing raw upstream SDK structs across responsibility boundaries.

The model is intentionally narrow: every type below is justified by an FR or a
concrete consumer in the new package layout. Nothing speculative.

## E1 — Workspace Config (`internal/config.Config`)

Source of truth for a Gitty workspace. Loaded from `<workspace>/.gitty/config`,
saved by `gitty init`. The Go type is unchanged from today's `config.go`; only
its package home moves.

| Field      | Type     | TOML key     | Purpose                                                                 |
| ---------- | -------- | ------------ | ----------------------------------------------------------------------- |
| `URL`      | `string` | `url`        | GitLab base URL (e.g., `https://gitlab.com`).                           |
| `HTTP`     | `bool`   | `http`       | If true, clone via HTTPS; otherwise SSH.                                |
| `RootPath` | `string` | `root_path`  | GitLab namespace path this workspace is anchored to (empty for root).   |

**Validation rules:**

- `URL` MUST be non-empty when `Load` is called; an empty `URL` from disk is a
  corruption case and `Load` MUST return an error referencing the file path.
- `RootPath` MAY be empty (top-of-workspace case).
- No other fields are validated structurally — the GitLab API call surfaces
  any URL/path errors at use time.

**State transitions:** none. The file is rewritten in full on `init`.

**Public surface (FR-002):**

```go
package config

const (
    Dir  = ".gitty"   // directory under workspace root
    File = "config"   // file inside Dir; full path is <workspace>/.gitty/config
)

type Config struct {
    URL      string `toml:"url"`
    HTTP     bool   `toml:"http"`
    RootPath string `toml:"root_path"`
}

func Load(workspaceDir string) (*Config, error)        // reads <workspaceDir>/.gitty/config
func Save(workspaceDir string, cfg *Config) error      // writes the same path; mkdir -p .gitty
```

`Load` returns a wrapped error with a remediation hint if the file is missing
(`"no .gitty/config in <dir>; run 'gitty init' first"`), satisfying spec
edge case #1.

## E2 — GitLab Namespace (`internal/gitlabapi.Group`)

A group or subgroup as Gitty cares about it. POGO that strips upstream SDK
fields Gitty doesn't use, so `internal/sync` and its tests don't depend on
the SDK.

| Field      | Type     | Purpose                                                                            |
| ---------- | -------- | ---------------------------------------------------------------------------------- |
| `FullPath` | `string` | Full namespace path (`tenant/images/backend`). Used as the local directory key.    |
| `Name`     | `string` | Human-readable name. Used only for log lines.                                      |

**Relationships:**

- A `Group` has zero or more child `Group`s (via `ListSubGroups` /
  `ListDescendantGroups`).
- A `Group` has zero or more `Project`s (via `ListGroupProjects`).

**Validation rules:**

- `FullPath` MUST be non-empty when returned from any `Client` method. The
  concrete `Real` implementation MUST drop or error on any zero-FullPath
  result from the SDK rather than producing a bad local path downstream.

## E3 — GitLab Project (`internal/gitlabapi.Project`)

A repository under a namespace. POGO with only the three fields used by
sync orchestration.

| Field               | Type     | Purpose                                                              |
| ------------------- | -------- | -------------------------------------------------------------------- |
| `PathWithNamespace` | `string` | Full path including namespace (`tenant/images/api`). Local-path key. |
| `SSHURLToRepo`      | `string` | `git@host:tenant/images/api.git`. Used when `Config.HTTP == false`.  |
| `HTTPURLToRepo`     | `string` | `https://host/tenant/images/api.git`. Used when `Config.HTTP == true`. |

**Relationships:** belongs to a namespace identified by `PathWithNamespace`'s
prefix.

**Validation rules:**

- For each project the orchestration consumes, the chosen URL field
  (`SSH` or `HTTP` per `Config.HTTP`) MUST be non-empty. The orchestration MUST
  emit a `skip` event on stderr (and continue) for any project missing the
  required URL — this is a robustness improvement over today's silent attempt
  to clone an empty URL.

## E4 — GitLab Client interface (`internal/gitlabapi.Client`)

The four-method narrow surface defined in research R-002. Not data per se, but
listed here because it bounds the data the orchestration ever sees.

```go
type Client interface {
    GetGroup(path string) (*Group, error)
    ListSubGroups(parent string) ([]*Group, error)
    ListDescendantGroups(parent string) ([]*Group, error)
    ListGroupProjects(parent string, recursive bool) ([]*Project, error)
}
```

Two implementations:

- `Real`: wraps `gitlab.NewClient`; collapses pagination via the generic
  `paginate` helper (R-003).
- (test-only) hand-written fakes per test, satisfying the same interface.

## E5 — Sync Plan (`internal/sync.Plan`)

The set of decisions a single `gitty sync` invocation will carry out. Today
implicit in the imperative loop in `runSync`/`syncGroups`/`syncRepos`. Made
explicit so the same plan drives both the `--dry-run` rendering (writes to
stdout) and the apply path (calls `gitexec.Runner.Run`).

```go
package sync

type Action int

const (
    ActionCreateGroupDir Action = iota   // mkdir + write per-group .gitty/config
    ActionClone                          // git clone into a non-existent dir
    ActionPull                           // git pull in an existing dir
)

type Step struct {
    Action       Action
    LocalDir     string   // destination relative to cwd
    NamespacePath string  // GitLab full path (group or project)
    CloneURL     string   // populated for ActionClone only
}

type Plan struct {
    Steps []Step
}
```

**Validation rules:**

- A `Plan` MUST contain only `Step`s whose `LocalDir` is a sibling-or-descendant
  of `cwd` (no absolute paths, no `..`). Enforced when steps are appended in
  the planner — this is a defense-in-depth against a malformed config.
- For `ActionClone`, `CloneURL` MUST be non-empty.

**State transitions:**

1. **Planner** (`func Build(req Request, c gitlabapi.Client, paths PathFn) (*Plan, error)`):
   pure function, no I/O — produces a `Plan` from the request and fetched
   GitLab data.
2. **Renderer** (`func (p *Plan) Render(w io.Writer)`): writes the plan to
   `w` (stdout) in the existing dry-run text format. Used by `--dry-run`.
3. **Apply** (`func (p *Plan) Apply(runner gitexec.Runner, fs FS, errOut io.Writer) error`):
   executes each step. Per-step progress lines go to `errOut`. Today's `runGit`
   stdout/stderr passthrough is preserved by the `Runner` impl.

**Rationale for making this explicit (vs. inline loops as today):**

- SC-002 says behaviors must be implemented in exactly one place. With an
  explicit `Plan`, "what would happen" is one method (`Render`) and "what
  actually happens" is another (`Apply`); both consume the same `Plan`. There
  is no chance of drift between `--dry-run` text and the real run.
- It makes the seed `sync_test.go` straightforward: build a `Plan` against a
  fake `Client`, assert the steps; apply against a fake `Runner` + tmpdir,
  assert the directory tree and the recorded git invocations.

## E6 — Sync Request (`internal/sync.Request`)

The parsed CLI input that drives a sync invocation. Pure value type produced
by `internal/cli` and consumed by `internal/sync.Build`.

| Field        | Type     | Source                                                       |
| ------------ | -------- | ------------------------------------------------------------ |
| `GroupFlag`  | `string` | `--path` flag value                                          |
| `Token`      | `string` | Resolved token (already-applied env-var fallback per FR-004) |
| `DryRun`     | `bool`   | `--dry-run` flag                                             |
| `DoGroups`   | `bool`   | `--groups` flag (with the existing default-to-`--repos` logic applied in `cli`) |
| `DoRepos`    | `bool`   | `--repos` flag (same)                                        |
| `Nested`     | `bool`   | `--nested` flag                                              |

**Validation rules:**

- After cli-side defaulting, at least one of `DoGroups` / `DoRepos` MUST be
  true. This preserves today's `if !doGroups && !doRepos { doRepos = true }`
  behavior in the same place it lives now (just promoted from inline to a
  helper function in `internal/cli`).
- `Token` MUST be non-empty by the time `sync.Build` is called. Empty-token
  errors are returned from `cli.Main` with the existing message text.

## Mapping — spec entities to data model

| Spec entity         | Implemented as                                  |
| ------------------- | ----------------------------------------------- |
| **Workspace Config**| E1 (`internal/config.Config`)                   |
| **GitLab Namespace**| E2 (`internal/gitlabapi.Group`)                 |
| **GitLab Project**  | E3 (`internal/gitlabapi.Project`)               |
| **Sync Plan**       | E5 (`internal/sync.Plan`) + E6 (`Request`)      |

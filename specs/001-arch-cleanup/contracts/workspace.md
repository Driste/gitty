# Contract: Workspace on-disk layout

**Feature**: Architecture Cleanup
**Date**: 2026-05-09

This contract documents the on-disk layout `gitty` reads and writes. Per spec
FR-008 + FR-009, the cleanup MUST preserve every path in this contract.
README and Constitution wording is being brought into alignment with what is
documented here.

## Workspace anchor

A directory is a Gitty workspace iff it contains the file
`<workspace>/.gitty/config`.

| Path component        | Type | Created by  | Mode  |
| --------------------- | ---- | ----------- | ----- |
| `.gitty/`             | dir  | `gitty init` | `0755` |
| `.gitty/config`       | file | `gitty init` | `0644` |

There is no other file under `.gitty/` today. Future per-workspace state (caches,
last-sync timestamps, etc.) is allowed to land here without amending this
contract — the directory is reserved for Gitty's use.

## `.gitty/config` schema

TOML, three keys, all top-level:

```toml
url        = "https://gitlab.com"           # GitLab base URL
http       = false                          # true → clone via HTTPS, false → SSH
root_path  = ""                             # GitLab namespace this dir is anchored to ("" = workspace root)
```

Field semantics (matches `internal/config.Config` Go struct):

- `url` (string, REQUIRED): base URL of the GitLab instance. Used verbatim as
  `gitlab.WithBaseURL`. Empty string is invalid; `Load` MUST return an error
  if read.
- `http` (bool, OPTIONAL, default `false`): when `true`, projects are cloned
  via `HTTPURLToRepo`; when `false`, via `SSHURLToRepo`.
- `root_path` (string, OPTIONAL, default `""`): the GitLab namespace path the
  *containing directory* corresponds to. The workspace root has `""`. A
  per-group sub-config (created by `gitty sync --groups`) has the group's
  full path here, so commands run from inside the group directory know where
  on GitLab they are.

## Discovery rule

`gitty sync` looks for `<cwd>/.gitty/config` only — it does NOT walk parent
directories. This matches today's `LoadLocalConfig` behavior.

A user who `cd`s into a sub-group directory created by `gitty sync --groups`
will find a `.gitty/config` there with that group's `root_path`, so
`gitty sync --path=<sub-path>` resolves relative to that anchor.

## Layout produced by `gitty sync`

For a workspace anchored at namespace `root_path = "tenant"` and a sync of
`--path "tenant/images" --nested`:

```
<workspace>/
├── .gitty/
│   └── config                  # root_path = "tenant"
└── images/                     # mirror of tenant/images
    ├── .gitty/                 # only when sync was run with --groups
    │   └── config              # root_path = "tenant/images"
    ├── api/                    # cloned project
    │   └── …git contents…
    ├── web/                    # cloned project
    │   └── …git contents…
    └── infra/                  # mirror of tenant/images/infra
        ├── .gitty/             # only when --groups
        │   └── config          # root_path = "tenant/images/infra"
        └── deploy/             # cloned project
```

**Key invariant** (spec edge case + Constitution Principle III): the local
directory tree mirrors the GitLab namespace path 1:1 with the workspace's
`root_path` prefix stripped. No flattening, no renaming.

## Pre-cleanup vs. post-cleanup differences

| Aspect                           | Pre-cleanup            | Post-cleanup           |
| -------------------------------- | ---------------------- | ---------------------- |
| File path                        | `.gitty/config`        | `.gitty/config`        |
| File contents                    | TOML (3 keys above)    | TOML (3 keys above)    |
| File mode                        | `0644`                 | `0644`                 |
| Directory mode                   | `0755`                 | `0755`                 |
| README documents the file as…    | `gitty.toml` ❌        | `.gitty/config` ✅      |
| Constitution names it as…        | `gitty.toml` ❌        | `.gitty/config` ✅ (PATCH amendment) |
| `gitty` usage text says…         | `gitty.toml` ❌        | `.gitty/config` ✅      |

**No on-disk migration is required.** A user with a workspace from the
pre-cleanup binary continues to have a valid workspace under the cleaned-up
binary — the cleanup only changes documentation wording to match what the code
has always written.

## Errors `internal/config.Load` MUST return

| Condition                                            | Returned error message (substring required for SC-006-adjacent test) |
| ---------------------------------------------------- | -------------------------------------------------------------------- |
| `<workspaceDir>/.gitty/config` does not exist        | `no .gitty/config in <workspaceDir>; run 'gitty init' first`         |
| File exists but is unreadable (permissions, etc.)    | The underlying `os.ReadFile` error, wrapped with the file path.      |
| File exists but is not valid TOML                    | The underlying `toml.Unmarshal` error, wrapped with the file path.   |
| File parses but `URL` is empty                       | `invalid .gitty/config in <workspaceDir>: url is required`           |

These messages are part of the contract because they appear in the spec edge
cases ("clear, actionable error" if layout unrecognized).

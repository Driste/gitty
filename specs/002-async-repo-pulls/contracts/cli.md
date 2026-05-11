# Contract: Gitty CLI argv schema — Async Repo Pulls delta

**Feature**: Async Repo Pulls with Concurrency Limit
**Date**: 2026-05-11
**Base contract**: [../../001-arch-cleanup/contracts/cli.md](../../001-arch-cleanup/contracts/cli.md)

This document records ONLY the changes introduced by this feature. Every
other aspect of the CLI surface (subcommand names, other flag names/defaults,
exit-code map, stream destinations, workspace resolution rules, token
resolution order) is unchanged from the 001 contract.

## New flag on `gitty sync`

| Flag      | Type   | Default                          | Meaning                                                          |
| --------- | ------ | -------------------------------- | ---------------------------------------------------------------- |
| `-jobs`   | int    | `0` sentinel ⇒ use `.gitty/config`'s `jobs` (typically `4`) | Maximum concurrent clone/pull operations. Range `[1, 64]`. Overrides the workspace `jobs` config for this invocation only; does not persist back. |

**Help text** (FR-011) — what `gitty sync -h` prints for the new flag:

```
  -jobs int
        Max concurrent clone/pull operations (range [1, 64]).
        Overrides 'jobs' in .gitty/config for this run.
        Default: value from .gitty/config (typically 4).
```

## Resolution order for the effective jobs count

1. If `--jobs N` was supplied with `N >= 1`, use `N`.
2. If `--jobs N` was supplied with `N == 0`, fall back to step 3 (treats
   `--jobs=0` as "not passed"). *(Note: this is the default the flag
   parser provides when the user did not pass `--jobs`.)*
3. Else use `Config.Jobs` from the workspace's `.gitty/config`, normalized
   inside `config.Load` to `4` when the field is absent or non-positive.
4. Clamp the result to `[1, 64]`. Values `> 64` are clamped to `64` with
   exactly one stderr warning line (FR-010):
   ```
   --jobs <N> clamped to 64
   ```
   where `<N>` is the user-supplied value.

## Validation errors

| User input        | Stage      | Exit code | Where the error appears |
| ----------------- | ---------- | --------- | ----------------------- |
| `--jobs -1`       | post-parse | `2`       | `stderr`: `--jobs must be >= 1 (got -1)` |
| `--jobs abc`      | flag parse | `2`       | `stderr`: Go `flag` package's standard "invalid value" error |
| `--jobs 9999`     | post-parse | `0`       | `stderr`: `--jobs 9999 clamped to 64`; sync proceeds at jobs=64. |
| `--jobs 0`        | post-parse | (varies)  | Treated as "not passed"; falls through to config value. No warning. |

## Effect on existing flags and behavior

- `--dry-run`: unchanged behavior. Output is byte-identical regardless of
  `--jobs` (FR-008, SC-006). The worker pool is bypassed in dry-run mode.
- `--groups`: unchanged. Group materialization is serial regardless of
  `--jobs` (FR-004).
- All other flags (`--path`, `--token`, `--repos`, `--nested`): unchanged.
- Exit codes on partial repo failure: unchanged. Still `0` on partial
  failure (FR-007).
- Exit code on SIGINT: non-zero (new behavior — FR-009 makes the existing
  "process killed by signal" behavior explicit). The exit code is whatever
  `cli.Main` returns when `sync.Sync` returned a non-nil context-error;
  the contract is "non-zero, distinct from a clean run."

## Effect on stream destinations (FR-006)

| Output kind                                                | Stream  | Conditions                                  |
| ---------------------------------------------------------- | ------- | ------------------------------------------- |
| `--dry-run` planning lines                                 | stdout  | Unchanged. Always serial, deterministic order. |
| Section banners (`--- Syncing Groups ---`, etc.)           | stderr  | Unchanged.                                  |
| Per-step gitty progress (`Processing X...`, `Cloning X`)   | stderr  | Now prefixed `[<namespace/path>] ` when effective jobs > 1. |
| Errors emitted by gitty (clone failed, save-config failed) | stderr  | Same prefix rule. On a git failure, the captured git stderr is appended in the same critical section. |
| **`git`'s own stdout**                                     | (none)  | **CHANGE.** Suppressed (`io.Discard`) for all `--jobs` values, including `1`. |
| **`git`'s own stderr**                                     | stderr  | **CHANGE.** Captured per-call. Forwarded to gitty's stderr only when the git invocation exited non-zero; discarded on success. |

The git output change is the only observable behavior shift at `--jobs=1`
caused by this feature.

## Test-fixture inputs (additions to 001's table)

These additions exercise the new flag at the cli boundary:

| argv                                                                | Expected outcome                                                                |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `["sync", "-path", "g/h", "-token", "xyz", "-jobs", "4"]`           | `Request.Jobs == 4`; worker pool sized at 4 in `sync.Sync`.                     |
| `["sync", "-path", "g/h", "-token", "xyz", "-jobs", "1"]`           | `Request.Jobs == 1`; serial code path; no prefix on stderr lines.               |
| `["sync", "-path", "g/h", "-token", "xyz"]` (no `--jobs`)           | `Request.Jobs == cfg.Jobs` (typically 4 for a config produced by `gitty init`). |
| `["sync", "-path", "g/h", "-token", "xyz", "-jobs", "100"]`         | `Request.Jobs == 64`; one stderr line `--jobs 100 clamped to 64`.               |
| `["sync", "-path", "g/h", "-token", "xyz", "-jobs", "-1"]`          | Exit code `2`; stderr `--jobs must be >= 1 (got -1)`; no sync attempted.        |
| `["sync", "-path", "g/h", "-token", "xyz", "-dry-run", "-jobs", "8"]` | Exit code `0`; stdout plan lines identical to the same invocation with `-jobs 1`. |

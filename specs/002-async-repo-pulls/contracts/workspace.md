# Contract: Workspace on-disk layout — Async Repo Pulls delta

**Feature**: Async Repo Pulls with Concurrency Limit
**Date**: 2026-05-11
**Base contract**: [../../001-arch-cleanup/contracts/workspace.md](../../001-arch-cleanup/contracts/workspace.md)

This document records ONLY the changes introduced by this feature.

## `.gitty/config` schema — one new optional field

Adds `jobs` as a fourth top-level TOML key.

```toml
url        = "https://gitlab.com"           # unchanged
http       = false                          # unchanged
root_path  = ""                              # unchanged
jobs       = 4                              # NEW. Default concurrency for 'gitty sync'.
```

| Field      | Type     | Required | Default when absent | Purpose                              |
| ---------- | -------- | -------- | ------------------- | ------------------------------------ |
| `url`      | string   | yes      | —                   | (unchanged)                          |
| `http`     | bool     | no       | `false`             | (unchanged)                          |
| `root_path`| string   | no       | `""`                | (unchanged)                          |
| `jobs`     | int      | **no**   | `4`                 | **NEW.** Max concurrent clone/pulls. |

**`jobs` semantics:**

- A missing or non-positive value MUST be treated as `4` by
  `config.Load` (research R-005). The caller never sees `Jobs <= 0`.
- A value `> 64` is NOT clamped at load time. Clamping happens at the
  CLI boundary so the warning line is emitted on the invocation that
  attempted the out-of-range value (FR-010), not on every load.
- The `--jobs` flag overrides this value for a single invocation only;
  the override MUST NOT persist back to the file (Constitution Principle
  III).

## What `gitty init` writes

`gitty init` writes a `.gitty/config` with all four fields populated:

```toml
url = "https://gitlab.com"
http = false
root_path = ""
jobs = 4
```

So the user can edit `jobs` in one place without having to know the
default exists.

## Backward compatibility

A `.gitty/config` written by a pre-feature binary has only three fields
(no `jobs`). The new binary reads such a config successfully:
`toml.Unmarshal` leaves `Config.Jobs` at `0`, then `config.Load`
normalizes `0` to `4`. No migration tool, no warning line, no user
action required.

Forward compatibility: a `.gitty/config` written by a new binary that
contains `jobs = 4` is read by a pre-feature binary cleanly — the older
binary's `toml.Unmarshal` simply ignores the unknown key. No error.

## Errors `internal/config.Load` MUST return — UNCHANGED

The four error messages from 001's workspace.md (no .gitty/config, unreadable,
malformed TOML, empty URL) are unchanged. A bad `jobs` value (negative,
non-numeric in TOML) is **not** an additional error case: TOML type errors
are surfaced as part of the existing "parse" error; a negative integer in
the file is coalesced to `4` like a missing field.

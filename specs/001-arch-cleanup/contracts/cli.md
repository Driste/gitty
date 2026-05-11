# Contract: Gitty CLI argv schema

**Feature**: Architecture Cleanup
**Date**: 2026-05-09

This contract documents the *current* CLI surface of `gitty`. Per spec FR-008,
the cleanup MUST preserve every line of this contract byte-for-byte. Any
deviation discovered during implementation is a bug in the implementation, not
this contract.

This file is the reference reviewers consult when verifying the cleanup PR has
not regressed CLI behavior. It is also the input the `internal/cli` seed test
will assert against.

## Process exit conventions

| Exit code | Meaning                                                          | Today's source                                |
| --------- | ---------------------------------------------------------------- | --------------------------------------------- |
| `0`       | Success.                                                         | Implicit return from `main`.                  |
| `1`       | Usage error (unknown subcommand, missing subcommand).            | `os.Exit(1)` in `main.go`.                    |
| `1`       | Runtime error (config missing, token missing, GitLab call fail). | `log.Fatal*` (which exits with code 1).       |
| `2`       | Flag parse error (handled by `flag.ExitOnError`).                | Go's `flag` package default.                  |

Post-cleanup these MUST remain identical. `cli.Main` returns the int; `main.main`
calls `os.Exit(cli.Main(...))`.

## Subcommand: `gitty` (no args)

**Behavior**: Print usage to stdout, exit `1`.

**Output (stdout)**:

```
Usage: gitty <command> [flags]

Commands:
  init    Initialize a gitty.toml config file in the current directory
  sync    Sync (clone/pull) a GitLab group based on the gitty.toml config

Run 'gitty <command> -h' for specific flags.
```

> ⚠ The current usage text says `gitty.toml`; per FR-009 this MUST be updated
> to `.gitty/config` as part of this cleanup. The usage text is part of the CLI
> contract, but that one phrase is the documented FR-009 change site.

## Subcommand: `gitty init`

**Purpose**: Initialize a workspace anchor in the current directory.

| Flag    | Type   | Default              | Meaning                                                       |
| ------- | ------ | -------------------- | ------------------------------------------------------------- |
| `-url`  | string | `https://gitlab.com` | GitLab base URL.                                              |
| `-http` | bool   | `false`              | Use HTTPS for cloning (otherwise SSH).                        |

**Side effects on success**:

1. Creates `<cwd>/.gitty/` (mode `0755`) if it doesn't exist.
2. Writes `<cwd>/.gitty/config` (mode `0644`) containing the TOML serialization
   of `{URL, HTTP, RootPath: ""}`.

**Output (stdout)**:

```
Initialized gitty root at <cwd>
You can now run 'gitty sync --path=<path>' to pull down repositories.
```

**Exit code**: `0` on success. `1` (`log.Fatalf`) if the file cannot be written.

## Subcommand: `gitty sync`

**Purpose**: Pull repositories and/or build group directory structure under
the workspace.

| Flag        | Type   | Default | Meaning                                                                                |
| ----------- | ------ | ------- | -------------------------------------------------------------------------------------- |
| `-path`     | string | `""`    | GitLab group/subgroup path. Required *unless* the workspace is itself a managed group. |
| `-token`    | string | `""`    | GitLab access token. Falls back to `$GITLAB_TOKEN`, then `$CI_JOB_TOKEN`.              |
| `-dry-run`  | bool   | `false` | Print planned actions; do not touch disk or invoke `git`.                              |
| `-groups`   | bool   | `false` | Materialize the group directory structure (mkdir + per-group `.gitty/config`).         |
| `-repos`    | bool   | `false` | Clone or pull repositories.                                                            |
| `-nested`   | bool   | `false` | Recurse into descendant subgroups.                                                     |

**Default selection rule**: If neither `-groups` nor `-repos` is supplied,
`-repos` is implied (and only `-repos`).

**Token resolution order** (one place per FR-004):

1. `-token` flag value, if non-empty.
2. `$GITLAB_TOKEN`, if set.
3. `$CI_JOB_TOKEN`, if set.

If all three are empty, fail with:

```
Error: A token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required.
```

Exit code `1`.

**Workspace resolution**:

1. Read `<cwd>/.gitty/config`. If missing, fail with:
   ```
   Error: No .gitty/config found in this directory. Run 'gitty init' first.
   ```
   Exit code `1`.
2. Compute the *target group path*:
   - If config's `RootPath` is empty and `-path` is empty → fail with the
     "Target group path is empty" message, exit `1`.
   - Otherwise concatenate as `RootPath + "/" + Path` (skipping the slash if
     either side is empty).

**Side effects when `-groups` is in effect**:

For each fetched group `g` (immediate or descendant per `-nested`):

1. `mkdir -p <local-path>` where local-path = `g.FullPath` with the workspace's
   `RootPath` prefix stripped.
2. Write `<local-path>/.gitty/config` with `RootPath: g.FullPath` (URL/HTTP
   inherited from the workspace).

The workspace's own `Group` is included at the top of the list so the workspace
root also gets a config refresh.

**Side effects when `-repos` is in effect**:

For each fetched project `p`:

1. Compute `local-path` = `p.PathWithNamespace` with the workspace `RootPath`
   prefix stripped.
2. If `local-path` already exists: `cd <local-path> && git pull`.
3. Otherwise: `mkdir -p <parent of local-path> && git clone <url> <local-path>`,
   where `<url>` is `SSHURLToRepo` (or `HTTPURLToRepo` if `Config.HTTP`).

`-dry-run` MUST suppress every filesystem mutation and every `git` invocation,
emitting the corresponding planning line instead.

**Stream destinations** (FR-010, post-cleanup):

| Output kind                                              | Stream  |
| -------------------------------------------------------- | ------- |
| `--dry-run` planning lines (`[DRY RUN] Would …`)         | stdout  |
| Section banners (`--- Syncing Groups ---`, etc.)         | stderr  |
| Per-step progress (`Cloning X to Y`, `Processing X…`)    | stderr  |
| Counts and summaries (`Found N projects.`)               | stderr  |
| Errors (today: `log.Fatalf`; post: returned + printed)   | stderr  |
| `git`'s own stdout/stderr from `clone`/`pull`            | passthrough to inherited streams (unchanged) |

**Today**: every line above goes to stdout (the current code uses `fmt.Printf`
exclusively). The FR-010 change is the move of the bottom four rows from stdout
to stderr; the `--dry-run` row stays on stdout.

**Exit code**: `0` if every project and group operation succeeded (today
`runGit` only prints errors and continues, so partial failure still exits `0`).
The cleanup MUST preserve this — changing exit-on-partial-failure is a
behavior change, out of scope.

## Test-fixture inputs (for `internal/cli` seed test)

The seed test will exercise these argv slices and assert the resulting
`sync.Request` (or the printed-and-exit behavior for the usage cases):

| argv                                                              | Expected outcome                                                                 |
| ----------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `[]`                                                              | Usage printed to stdout buffer; return code `1`.                                 |
| `["unknown"]`                                                     | `Unknown command: unknown` + usage; return code `1`.                             |
| `["init"]`                                                        | `init` invoked with defaults `https://gitlab.com`, `HTTP=false`.                 |
| `["init", "-url", "https://gl.x", "-http"]`                       | `init` invoked with `URL=https://gl.x`, `HTTP=true`.                             |
| `["sync"]`                                                        | Returns the "no .gitty/config" error to stderr; return code `1`.                 |
| `["sync", "-path", "g/h", "-token", "xyz"]`                       | `Request{GroupFlag:"g/h", Token:"xyz", DoRepos:true}` reaches `sync.Build`.      |
| `["sync", "-path", "g/h", "-token", "xyz", "-groups"]`            | `Request.DoGroups=true, DoRepos=false`.                                          |
| `["sync", "-path", "g/h", "-token", "xyz", "-groups", "-repos"]`  | `Request.DoGroups=true, DoRepos=true`.                                           |
| `["sync", "-path", "g/h", "-token", "xyz", "-dry-run"]`           | `Request.DryRun=true`; no git or filesystem writes occur.                        |

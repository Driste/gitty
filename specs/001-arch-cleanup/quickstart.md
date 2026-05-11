# Quickstart: Working in the cleaned-up Gitty layout

**Audience**: Anyone (the maintainer, a future contributor, an AI agent) who
opens the repo after the architecture cleanup lands and needs to find their
way around in five minutes.

This is the file the README's "Architecture" section will link to (FR-013).

## The 30-second tour

```text
gitty/
├── main.go                 # 5 lines. Calls cli.Main.
└── internal/
    ├── cli/                # Parse argv → Request struct. Owns flag definitions and usage text.
    ├── config/             # Read/write <workspace>/.gitty/config.
    ├── gitlabapi/          # Talk to GitLab. Returns plain Group/Project structs.
    ├── paths/              # Pure: namespace path → local directory path.
    ├── gitexec/            # Run `git`. One method, one type.
    └── sync/               # Compose the above. Build a Plan, then Apply it (or Render it for --dry-run).
```

If you only remember one rule: **`internal/sync` is the only package allowed
to depend on more than one other internal package.** The others are leaves.

## "I'm trying to fix X — where do I start?"

| If the question is…                                                   | Open this file                              |
| --------------------------------------------------------------------- | ------------------------------------------- |
| Why does `gitty sync` accept this flag / reject that combination?      | [`internal/cli/sync_cmd.go`](../../internal/cli/sync_cmd.go) |
| Where is the token resolved from `--token` / env vars?                 | [`internal/sync/token.go`](../../internal/sync/token.go) (one function) |
| Why does this group end up at this local path?                         | [`internal/paths/paths.go`](../../internal/paths/paths.go) (one function) |
| The GitLab call returned the wrong shape — where do I add a field?    | [`internal/gitlabapi/client.go`](../../internal/gitlabapi/client.go) |
| `git pull` is being invoked weirdly                                    | [`internal/gitexec/gitexec.go`](../../internal/gitexec/gitexec.go) |
| Dry-run shows X but the real run does Y                                | [`internal/sync/sync.go`](../../internal/sync/sync.go) — `Plan.Render` and `Plan.Apply` consume the *same* plan, so the answer is in `Plan.Build` upstream of both |
| `.gitty/config` isn't being read / written correctly                   | [`internal/config/config.go`](../../internal/config/config.go) |

## "I'm adding a new subcommand"

1. Add a new file `internal/cli/<name>_cmd.go` exporting a function that
   takes `args []string, stdout, stderr io.Writer, env func(string) string`
   and returns `(int, error)`.
2. Wire it up in `cli.Main`'s switch (one new case).
3. If it touches GitLab: extend `gitlabapi.Client` with the methods you need
   (and a fake in tests).
4. If it touches git: call `gitexec.Runner.Run`.
5. Don't reach into other `internal/` packages from outside `internal/sync`.

## "I'm adding a test"

Each `internal/<pkg>/` already has a seed `*_test.go` showing the pattern for
that package. Copy it.

The full suite runs offline (no GitLab network, no `git` binary required):

```bash
go test ./...
```

If your test needs to run `git` or hit GitLab, it goes in a build-tagged
`*_integration_test.go` file (none exist yet — start one if you need it).

## "I'm changing CLI behavior"

Read [`specs/001-arch-cleanup/contracts/cli.md`](contracts/cli.md) first.
It documents every flag, every default, and every exit code. Any change you
make should also update that contract document — and likely warrants its
own `/speckit-specify` invocation if it's user-facing.

## Stream conventions

- **stdout**: machine-readable / pipe-friendly output. Today only `--dry-run`
  plan lines go here.
- **stderr**: progress, banners, errors.
- **`git`'s own output**: passthrough — Gitty doesn't capture or rewrite it.

Everything that prints in `internal/sync` takes an `io.Writer` for `out`
(stdout) and another for `errOut` (stderr). The `cli` package wires the real
`os.Stdout` / `os.Stderr` in production and `bytes.Buffer` in tests.

## Build / verify locally

```bash
go build ./...        # Must succeed at every commit.
go vet ./...          # Must succeed at every commit.
go test ./...         # Must succeed at every commit (no network, no git binary).
go build -o gitty .   # Produce the binary. CLI behavior must match contracts/cli.md.
```

## What this layout deliberately does NOT have

- No `--verbose` flag, no per-event-class line prefixes, no credential
  redaction, no ANSI handling. These are Constitution Principle II requirements
  and live in a follow-up spec (deferred per spec.md FR-010 scope statement).
- No async sync, no branch-freshness display, no add/remove diff. These are
  feature ideas in the README TODO list, not part of this cleanup.
- No migration tooling for `gitty.toml` → `.gitty/config`. There is no
  migration: the on-disk path was always `.gitty/config`; only the docs change.

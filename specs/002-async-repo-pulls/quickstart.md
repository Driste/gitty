# Quickstart: Running gitty sync in parallel

**Audience**: Anyone using `gitty sync` against a workspace with more than a
handful of repos, or anyone touching the worker-pool code in
`internal/sync`.

This file sits alongside (not replacing) [`specs/001-arch-cleanup/quickstart.md`](../001-arch-cleanup/quickstart.md),
which documents the overall package layout.

## For users

### Setting a default for a workspace

`.gitty/config` now has a `jobs` field that controls how many repos clone/pull
concurrently. After `gitty init` on a fresh directory, you'll see something
like:

```toml
url = "https://gitlab.com"
http = false
root_path = ""
jobs = 4
```

To change the default for this workspace, edit the file. No `--jobs` flag
needed on subsequent commands.

### Overriding for one invocation

```bash
gitty sync --path=tenant/images --jobs=8     # one-shot bump to 8
gitty sync --path=tenant/images --jobs=1     # one-shot serial run (rollback)
```

The `--jobs` flag overrides `.gitty/config`'s `jobs` for this invocation
only — it does NOT persist back to the file.

### Output under concurrency

When the effective jobs value is `> 1`, every gitty per-repo line on stderr
is prefixed with the repo's namespace path. That makes the output
grep-friendly even when several repos clone in parallel:

```bash
gitty sync --path=tenant --nested --jobs=4 2> sync.stderr
grep '\[tenant/images/api\]' sync.stderr  # everything for that one repo
```

Git's own progress output is suppressed across all `--jobs` values
(including `--jobs=1`). If a `git clone` or `git pull` fails, the stderr
git produced is captured and printed right after gitty's `[<repo>] error`
line so you can see why.

### Dry-run is unaffected

`gitty sync --dry-run` produces the same output regardless of `--jobs`:
serial, deterministic, repo-order. The worker pool is bypassed in dry-run
mode.

### Ctrl-C

Pressing Ctrl-C during a parallel sync stops dispatching new repos,
terminates in-flight git children, and exits with a non-zero code within a
couple seconds. The workspace is left in a re-runnable state: complete
repos stay, partial clones are detected on the next run.

### Bounds and edge cases

- Valid range: `[1, 64]`. The lower bound (1) is the rollback path; the
  upper bound (64) prevents fork-bombing your SSH agent.
- `--jobs=0` is treated as "didn't pass the flag" and falls back to the
  config value.
- `--jobs=-1` (or any negative) is a flag error: exit 2.
- `--jobs=9999` is clamped to 64 with one stderr warning line.

## For contributors

### Where the new code lives

```text
internal/sync/
├── sync.go         # MODIFIED: the existing serial loop in syncReposSection
│                   #           now dispatches Jobs into pool.Submit(...).
└── workers.go      # NEW: type pool, type atomicWriter, runOne(job).

internal/gitexec/
└── gitexec.go      # CHANGED: Runner.Run signature now returns ([]byte, error)
                    #          and takes a context.Context. Real is stateless
                    #          beyond Env; stdout is always io.Discard.

internal/config/
└── config.go       # CHANGED: Config gains Jobs int; Load coalesces <=0 to 4.

internal/cli/
└── sync_cmd.go     # CHANGED: --jobs flag added; clamps to [1,64]; passes
                    #          resolved value into sync.Request.Jobs.
```

### Where to look for X

| Question                                              | File                              |
| ----------------------------------------------------- | --------------------------------- |
| How does the `--jobs` flag resolve to a number?       | `internal/cli/sync_cmd.go`        |
| Why does my workspace default to 4 jobs?              | `internal/config/config.go` (`DefaultJobs`) |
| How is concurrency bounded?                           | `internal/sync/workers.go` (`pool.sem`) |
| Why is git's progress hidden?                         | `internal/gitexec/gitexec.go` (`cmd.Stdout = io.Discard`) |
| Where does the captured git stderr get printed on failure? | `internal/sync/workers.go` (`atomicWriter.WriteBlock`) |
| Why do my stderr lines have `[tenant/foo]` prefixes? | `internal/sync/workers.go` (`pool.effectiveJobs > 1` branch) |
| Why does Ctrl-C exit so fast?                         | `internal/cli/sync_cmd.go` (`signal.NotifyContext`) |

### Writing tests for code that uses Runner

The Runner contract changed — fake runners now return `([]byte, error)`:

```go
type fakeRunner struct {
    mu       sync.Mutex
    calls    int
    inFlight atomic.Int64
    peak     atomic.Int64
    delay    time.Duration
    stderr   []byte
    err      error
}

func (f *fakeRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
    n := f.inFlight.Add(1)
    defer f.inFlight.Add(-1)
    for {
        peak := f.peak.Load()
        if n <= peak || f.peak.CompareAndSwap(peak, n) {
            break
        }
    }
    if f.delay > 0 {
        select {
        case <-time.After(f.delay):
        case <-ctx.Done():
            return nil, ctx.Err()
        }
    }
    f.mu.Lock()
    f.calls++
    f.mu.Unlock()
    return f.stderr, f.err
}
```

Use it for fan-out tests (`peak` ≤ `jobs`), failure-mode tests
(`stderr=[]byte("fatal: ..."), err=&exec.ExitError{}`), and cancellation
tests (the `select` on `ctx.Done()`).

### Don't reach for golang.org/x/sync

`errgroup.SetLimit` would be a clean API, but it's a new module dependency
and FR-012 forbids it. Use the standard `sync.WaitGroup` + buffered channel
semaphore pattern. There's an example in `workers.go`.

## Build / verify locally

```bash
go build ./...                       # Must succeed at every commit.
go vet ./...                         # Must succeed at every commit.
go test ./...                        # Must succeed with no network, no git binary on PATH.
go build -o gitty .                  # Produce the binary.
./gitty sync -h | grep -- '-jobs'    # Verify --jobs is in help text (FR-011).
```

## What this layout deliberately does NOT have

- No `--verbose` flag, no per-event-class line prefixes (the `[<repo>]`
  prefix is repo-specific, not event-class-specific), no ANSI handling,
  no credential redaction. These remain Constitution Principle II
  follow-ups, separate from this feature.
- No async fetch of groups/projects from the GitLab API. Listing remains
  serial because the API uses pagination, not parallel reads.
- No exit-code change on partial repo failure. Still `0`.
- No retry/backoff on a failing repo. One attempt, log, continue.

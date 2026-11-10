# Tasks: Cross-Repository Context

**Input**: `specs/001-cross-repo-context/plan.md`, `spec.md`
**Branch**: `001-cross-repo-context`

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel with its neighbours (different files, no
  dependency on an unfinished task)
- **[US1]…[US4]**: the user story the task serves
- Every task names the file it touches

## Path Conventions

CLI: single `main` package at the repository root; one file per concern;
tests beside the code as `*_test.go`; e2e in `e2e_test.go` against the fake
GitLab. Plugin: `cmd/gitty-mcp/` (its own `main`), sharing
`internal/contract/` with the CLI. Task IDs are stable; phases are ordered by
their headings, so Phase 0 carries the highest numbers.

---

## Phase 0: Plugin seam (no story — enables US1)

**Goal**: `gitty <cmd>` runs `gitty-<cmd>`; the JSON contract has a home; the
release ships two binaries. Nothing MCP-specific yet.

- [x] T038 `plugin.go`: `findPlugin(name, exe) (path, ok)` — `gitty-<name>`
      beside the binary (and beside its symlink target) first, then
      `exec.LookPath`, which also covers `.exe` on Windows and refuses the
      current directory; `execPlugin` in `plugin_unix.go` replaces the
      process (`syscall.Exec`), so stdio, signals and the exit code are the
      plugin's own with nothing in between; `plugin_windows.go` runs a child
      and passes its exit code on.
- [x] T039 `plugin.go` / `main.go` / `config.go`: `pluginEnv()` — locate
      the workspace without changing directory (`locateWorkspace`; "not in a
      workspace" → empty values, any other failure → a note on stderr) and
      set `GITTY_BIN`, `GITTY_WORKSPACE` (the outermost config of the
      workspace, `workspaceRoot`), `GITTY_GROUP`, `GITTY_INSTANCE`, replacing
      inherited values; the `default:` arm in `main.go` runs the plugin; the
      not-found message names `gitty-<cmd>` and the places looked, with an
      install hint instead of usage when `cmd == "mcp"`; exit 2.
- [x] T040 [P] `plugin_test.go`: a stub `gitty-echo` script on a temp `PATH`
      (and one beside a copied binary, which must win) — arguments, stdin,
      the four env vars, the working directory, exit code 3 passed through,
      not-found message and exit 2, a built-in never shadowed, the current
      directory never searched, an unrunnable plugin reported; unit tests
      for lookup order, `installedPlugins`, `pluginEnv`, `workspaceRoot`.
- [x] T041 [P] `internal/contract/contract.go`: `Schema = 1` for what the
      commands print and `IndexVersion = 1` for the file; `Index`,
      `Project`, `Local`, `Link`, `Unresolved`, `Briefing`, `Subject`,
      `Member`, `Related`, `FetchReport`, `FetchResult` with JSON tags; the
      link kinds as constants; `Supported` for version checks. (The root
      package uses these types directly in T002/T017/T044.)
- [ ] T042 [P] `.github/workflows/release.yml`: build
      `gitty-mcp_${VERSION}_${os}_${arch}` from `./cmd/gitty-mcp` in the same
      loop; checksum and upload `gitty-mcp_*` too; smoke: `gitty-mcp
      --version` reports the tag. CI's `go vet ./...` / `go test ./...`
      already cover the new packages. (Done with T026: there is nothing to
      build until `cmd/gitty-mcp` exists.)
- [x] T043 [P] `README.md`: "Plugins" — the lookup rule, the env vars, what
      a plugin is started with. (Installing `gitty-mcp` is documented with
      the plugin itself, T031.)

**Checkpoint**: `gitty mcp` without the plugin exits 2 with the hint; with a
stub `gitty-mcp` on `PATH` it runs it with the env set.

---

## Phase 1: Foundation — index, resolution, scanning (US3)

**Goal**: a correct, incremental, inspectable index of projects and evidenced
links, built from CI configuration, manifests and the API.

### Fake GitLab

- [ ] T001 [US3] Extend `apiProject` in `e2e_test.go` with `description`,
      `topics`, `forked_from_project.path_with_namespace`,
      `container_registry_image_prefix`; have `f.project()` fill sensible
      defaults; serve them from the project listing and `GET /projects/:id`.

### Index

- [ ] T002 [P] [US3] `index.go`: the index model is `contract.Index` and
      friends (T041), used directly — no second set of types; `indexVersion
      = contract.Schema`; `indexPath(workspaceDir)`.
- [ ] T003 [P] [US3] `index.go`: `loadIndex(dir) (*Index, error)` — missing
      file → empty index; version mismatch → empty index with a note;
      `saveIndex(dir, *Index)` — temp file + rename; never stores a token
      (test asserts the file never contains the credential used).
- [ ] T004 [US3] `index.go`: staleness — `headOf(dir)` via `git rev-parse
      HEAD`; `(*Index).stale(project, head) bool`; drop links `from` a project
      being rescanned before re-adding.
- [ ] T005 [P] [US3] `index_test.go`: round-trip, version mismatch, atomic
      write survives a simulated failure (temp left, original intact), stale
      detection.

### Resolution

- [ ] T006 [P] [US3] `resolve.go`: `resolver` built from the index's project
      paths + instance host + registry prefixes; `resolve(candidate) (path,
      ok)` handling: `https://host/g/p[.git][/-/…]`, `host/g/p[/sub]` (Go
      module paths), `git@host:g/p.git`, `ssh://git@host/g/p`, registry
      prefixes → project, bare `g/p` paths; longest-prefix match at `/`
      boundaries; `looksLikeInstanceRef(candidate)` for the unresolved list.
- [ ] T007 [P] [US3] `resolve_test.go`: table over every form; `acme/lib-tools`
      vs `acme/lib`; `host/acme/lib/v2` → `acme/lib`; foreign hosts → no.

### Scanner

- [ ] T008 [US3] `scan.go`: `scanCheckout(ctx, git, dir, instance, resolver)
      (links, unresolved, docs, summary, err)` — `git ls-files -z`, name
      filter, ≤ 1 MiB per file, extractors in order; README first-paragraph
      summary; root `*.md` doc list.
- [ ] T009 [P] [US3] `scan.go`: CI extractor — `.gitlab-ci.yml`,
      `.gitlab/ci/**/*.yml`, `*.gitlab-ci.yml`; tracks the current `include:`
      / `trigger:` block by indentation; `project:` → `ci-include` /
      `ci-trigger`; `component:` → `ci-component` (strip `@version` and the
      component name).
- [ ] T010 [P] [US3] `scan.go`: manifest extractor — file list per plan;
      tokens containing the instance host, or Go module paths starting with
      it; strip `.git`, `/-/…`, version suffixes; → `depends`.
- [ ] T011 [P] [US3] `scan_test.go`: fixture trees under `testdata/scan/`
      (a CI file with include/trigger/component; go.mod; package.json with a
      git URL; requirements with `git+https`; a Dockerfile — expected to yield
      nothing in phase 1); exact evidence lines asserted.
- [ ] T012 [US3] `index.go`: `buildIndex(ctx, s *syncer, target, nested)`
      — list projects with full metadata (concurrent pager; add a
      `ProjectsDetailed` to `gitlabSource`), fork edges from the API, scan
      changed checkouts honouring `exclude`/archived, merge, save.

### `gitty index`, `gitty related`

- [ ] T013 [US3] `indexcmd.go`: `runIndex(ctx, opts)` — events `index
      <path> new|changed|unchanged`, `link <from> <kind> <to> <evidence>`,
      `unresolved <from> <ref> <evidence>`, `summary projects=N scanned=N
      links=N unresolved=N`; `--dry-run` prints `plan index <path>` per
      changed project and writes nothing; `--nested`, `--archived`,
      `--exclude`, `--jobs`, `--verbose`, `--anon`, `--token`.
- [ ] T014 [P] [US3] `related.go`: `runRelated(ctx, opts)` — `related <p>
      <kind> <to> <evidence>`, `related-by <p> <kind> <from> <evidence>`,
      summary; `--direction`; builds/refreshes the index first when stale.
- [ ] T015 [US3] `commands.go`: add `index` and `related` to the command
      table with flags (completion follows); `main.go` dispatch; `agent.go`
      schema entries; `eventLineRe` in tests gains the new prefixes.
- [ ] T016 [US3] `e2e_test.go`: `TestE2EIndexAndRelated` — three repos with
      a CI include and a go.mod dependency; index events and file; second run
      all `unchanged`; `related` both directions; `--dry-run` writes nothing;
      excluded project absent.

**Checkpoint**: `gitty index && gitty related acme/app` answers correctly on
the fake instance and on a real one.

---

## Phase 2: Context — both surfaces, with fetching (US1, US2) 🎯 Prototype

**Goal**: an agent in a checkout gets, in one call, the path to every related
project and the reason it matters, with absent ones fetched into place.

### Briefing and fetching

- [ ] T017 [P] [US1] `briefing.go`: `briefing` model (subject, uses, usedBy,
      unresolved, readNext, fetchFailures) and `buildBriefing(idx, subject,
      direction, caps)`; group subjects; `relative_path` from the caller's
      directory; `docs` ordering (README last).
- [ ] T018 [P] [US1] `fetch.go`: `fetchProjects(ctx, s *syncer, paths)
      (results)` — resolves each path to a project (index, else API), skips
      checked-out ones, runs the existing `bringUp` into the namespace path,
      returns per-project outcome; honours `exclude`; never touches a project
      that is not named.
- [ ] T044 [US1] `fetch.go`: `runFetch(ctx, opts)` — `gitty fetch
      <project>...`: events `clone`/`pull`/`skip`/`error` as sync, summary;
      `--format=json` prints `[]contract.FetchResult` (`path`, `dir`,
      `fetched`, `present`, `error`); `--dry-run` prints `plan clone <path>`
      and writes nothing; `--jobs`, `--token`, `--anon`, `--exclude`.
- [ ] T045 [US1] `commands.go` / `agent.go`: register `fetch`;
      `e2e_test.go`: `TestE2EFetch` — two names, one present (pulled), one
      absent (cloned), a sibling untouched; JSON round-trips; `--dry-run`
      writes nothing.
- [ ] T019 [US1] `index.go`: `ensureFresh(ctx, s, subject)` — build the index
      if missing/mismatched, rescan the subject if its HEAD moved, rescan a
      just-fetched project so its links appear.
- [ ] T020 [P] [US1] `briefing_test.go`: uses/used-by split, caps, group
      subject, unresolved listing, relative paths.

### `gitty context`

- [ ] T021 [US2] `context.go`: `runContext(ctx, opts)` — resolve the subject
      like `ls` resolves its argument (checkout → its project; group →
      group); `--fetch`; `--format=markdown|json`; Markdown renderer with the
      stable headings from the plan; JSON renderer shares the MCP result type.
- [ ] T022 [US2] `commands.go` / `agent.go`: register `context`.
- [ ] T023 [US2] `e2e_test.go`: `TestE2EContext` — Markdown from inside a
      checkout and from a group; `--fetch` clones the absent target and the
      line shows `(fetched)`; without it the line names `gitty sync
      --related`; `--format=json` round-trips.

### `gitty-mcp` (the plugin, `cmd/gitty-mcp/`)

- [ ] T024 [US1] `cmd/gitty-mcp/rpc.go`: JSON-RPC 2.0 over stdio —
      newline-delimited request reader, `id`/`method`/`params`, responses and
      error objects (`-32700`, `-32600`, `-32601`, `-32602`, `-32603`);
      `initialize` (protocolVersion echo, `serverInfo{name:"gitty-mcp",
      version}`, `capabilities.tools`), `notifications/initialized`, `ping`,
      `tools/list`, `tools/call`; logs to stderr only.
- [ ] T025 [US1] `cmd/gitty-mcp/tools.go`: tool registry with JSON-schema
      inputs, split into `indexTools` and `workspaceTools` behind one
      dispatcher. `indexTools`: `gitty_context` → run `GITTY_BIN context
      --format=json [--fetch] [--direction] [project]` in the workspace and
      return the `contract.Briefing`; `gitty_related` → the same, links only;
      `gitty_projects` → decode `<GITTY_WORKSPACE>/.gitty/index.json`, run
      `GITTY_BIN index` first when missing, cache by mtime; `gitty_fetch` →
      `GITTY_BIN fetch --format=json`. `workspaceTools`: `gitty_search`
      (`git grep -n -I --max-count` per checkout from the index, `max` cap)
      and `gitty_read` (confinement: reject `..`, absolute, and
      `filepath.EvalSymlinks` inside the checkout; line range). A reply whose
      `schema` is unsupported is a tool error naming both versions.
- [ ] T026 [US1] `cmd/gitty-mcp/main.go`: flags `--no-fetch` (fetch default
      for `gitty_context`), `--jobs`, `--version`; environment from `gitty`
      (`GITTY_BIN`, `GITTY_WORKSPACE`, `GITTY_GROUP`, `GITTY_INSTANCE`);
      without `GITTY_WORKSPACE` refuse to start ("run it as `gitty mcp` from
      inside a workspace"); graceful exit on EOF / SIGTERM; `gitty`'s stderr
      relayed to stderr.
- [ ] T027 [US1] `agent.go`: `agent schema` gains a `plugins` list of the
      `gitty-*` executables found (beside the binary, on `PATH`); for
      `gitty-mcp` it includes the tool list from `gitty-mcp --describe`.
- [ ] T028 [P] [US1] `cmd/gitty-mcp/*_test.go`: drive the server over
      `io.Pipe` with a stub `gitty` on a temp `PATH` that prints canned
      `context --format=json` / `fetch --format=json` and records its
      arguments — initialize, tools/list names six tools, `gitty_context`
      passes `--fetch` by default and `--no-fetch` suppresses it, the result
      carries `path`, `kind`, `evidence`, `fetched`; `gitty_read` refuses
      `../x`, absolute paths and an escaping symlink; `gitty_search` is
      bounded; malformed JSON → `-32700`; unknown method → `-32601`;
      unsupported `schema` → tool error.
- [ ] T029 [US1] `e2e_test.go`: build `gitty-mcp` beside `gitty-e2e` in
      `TestMain`; `TestE2EMCPContextFetches` — real binaries, real git, fake
      GitLab: `gitty mcp` from A (plugin found beside the binary) answers
      `gitty_context`, clones B into its namespace path and returns that
      absolute path; second call `fetched: false`; `--no-fetch` lists B as
      absent and clones nothing; `gitty mcp` with the plugin removed exits 2
      with the hint.
- [ ] T030 [US1] `scripts/mcp-smoke.sh` + CI step: build both binaries,
      start `gitty mcp` in the integration workspace and exchange
      initialize/list/call over stdio with a shell client (no extra tooling).

### Docs

- [ ] T031 [P] [US1] `README.md`: "Context for agents" section — what links
      are, installing `gitty-mcp` and registering `gitty mcp` as the server
      (one generic stdio-server registration example), the tools, fetch
      semantics and `--no-fetch`, `gitty context`, `gitty fetch`,
      `gitty index`/`related`; constitution-relevant notes (nothing written
      in repos, index location, token never stored, the plugin never holds
      a token).
- [ ] T032 [P] [US1] `context.md`: agent-facing documentation of the tools
      and the briefing shape.

**Checkpoint**: SC-001 and SC-005 pass; this is the prototype.

---

## Phase 3: Shell fetch and remaining sources (US4)

- [ ] T033 [US4] `sync.go`: `--related[=uses|used-by|both]` — select link
      targets/sources of the current project not checked out and feed the
      existing pool; `--dry-run` parity; events as today.
- [ ] T034 [P] [US4] `scan.go`: image extractor (`FROM`, `image:` against
      registry prefixes) → `image`.
- [ ] T035 [P] [US4] `scan.go`: `.gitmodules` extractor → `submodule`.
- [ ] T036 [P] [US4] `scan.go`: documentation mentions extractor →
      `mentions` (root `*.md`, `docs/**/*.md`).
- [ ] T037 [US4] tests for T033–T036; README updates.

---

## Dependencies

- T038 → T039 → T040; T041 before T002, T017, T044; T042, T043 any time
  after T038.
- T001 before T012, T016, T023, T029.
- T002 → T003 → T004; T006 before T008; T008 before T009/T010 (they plug into
  it); T012 needs T004, T006, T008; T013/T014 need T012; T015 after T013/T014.
- T017, T018 need Phase 1; T019 needs T012, T018; T044 needs T018, T041;
  T045 after T044; T021 needs T017–T019; T024 → T025 → T026; T025 needs
  T021 and T044 (it runs them); T028 needs T026; T029 needs T039, T026,
  T045; T027 after T026.
- Phase 3 needs Phase 2's `fetch.go` only for T033.

## Parallel Example

```
T038 ‖ T041 ‖ T042 ‖ T043   # Phase 0: dispatch, contract types, release, docs
T002 ‖ T006 ‖ T001      # types, resolver, fake API fields
T003 ‖ T005 ‖ T007      # load/save, index tests, resolver tests
T009 ‖ T010 ‖ T011      # the two extractors and their fixtures
```

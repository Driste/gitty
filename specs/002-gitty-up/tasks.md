# Tasks: gitty-up

**Input**: `specs/002-gitty-up/plan.md`, `spec.md`

## Phase A — what a repository says

- [ ] T101 `internal/contract`: `Facts`, `Provided`, hub wire types
      (`HubWant`, `HubWanted`, `HubReceipt`, `HubInfo`), `HubSchema`,
      `KindDeploys`; `Local.Provides`; `ScannerVersion` 4.
- [ ] T102 `scan.go`: provides — CI components and templates, security
      policies, Helm charts, Terraform modules, root Dockerfile.
- [ ] T103 `scan.go`: deployment configuration (values files,
      `kustomization.yaml`, `helmfile.yaml`, agent `config.yaml`) → `deploys`.
- [ ] T104 tests for T102, T103.

## Phase B — the hub

- [ ] T110 `gitlab.go`: listing with paging and visible, bounded retry;
      `whoIs`, `standingOn`.
- [ ] T111 `store.go`: tree, facts, notes log; atomic writes; replay.
- [ ] T112 `standing.go`: use, upkeep, labels, order.
- [ ] T113 `notes.go`: slots, replacement rule, caps, evidence state.
- [ ] T114 `render.go`: tree with a byte budget, browse, search, outline.
- [ ] T115 `mcp.go`: 2026-07-28 over HTTP, and the 2025 handshake.
- [ ] T116 `api.go`: `/v1/info`, `/v1/want`, `/v1/facts`.
- [ ] T117 `main.go`: flags, refresh loop, shutdown, `--no-auth` on loopback.
- [ ] T118 unit tests for T110–T117.

## Phase C — gitty shares

- [ ] T120 `config.go`, `init`: the `hub` key.
- [ ] T121 `share.go`: `gitty share` — events, summary, `--dry-run`; clean
      default-branch checkouts only.
- [ ] T122 share after `sync`, `fetch`, `sync --related`; `--no-share`.
- [ ] T123 e2e: two workspaces, one hub, used-by seen from both.

## Phase D — retire what it replaces

- [ ] T130 remove `cmd/gitty-mcp`, `gitty context`, `gitty related`,
      `gitty index`; keep what `fetch` and `sync --related` use.
- [ ] T131 README; release and CI workflows build and smoke-test `gitty-up`.
- [ ] T132 mark 001's spec and plan as superseded where they are.

## Phase E — against a real instance

- [ ] T140 `gitty up` on gitlab.com groups, anonymous; share from a real
      workspace; walk it over HTTP.

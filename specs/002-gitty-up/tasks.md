# Tasks: gitty-up

**Input**: `specs/002-gitty-up/plan.md`, `spec.md`

## Phase A — what a repository says

- [x] T101 `internal/contract`: `Facts`, `Provided`, hub wire types
      (`HubWant`, `HubWanted`, `HubReceipt`, `HubInfo`), `HubSchema`,
      `KindDeploys`; `Local.Provides`; `ScannerVersion` 4.
- [x] T102 `scan.go`: provides — CI components and templates, security
      policies, Helm charts, Terraform modules, root Dockerfile.
- [x] T103 `scan.go`: deployment configuration (values files,
      `kustomization.yaml`, `helmfile.yaml`, agent `config.yaml`) → `deploys`.
- [x] T104 tests for T102, T103.

## Phase B — the hub

- [x] T110 `gitlab.go`: listing with paging and visible, bounded retry;
      `whoIs`, `standingOn`.
- [x] T111 `store.go`: tree, facts, notes log; atomic writes; replay.
- [x] T112 `standing.go`: use, upkeep, labels, order.
- [x] T113 `notes.go`: slots, replacement rule, caps, evidence state.
- [x] T114 `render.go`: tree with a byte budget, browse, search, outline.
- [x] T115 `mcp.go`: 2026-07-28 over HTTP, and the 2025 handshake.
- [x] T116 `api.go`: `/v1/info`, `/v1/want`, `/v1/facts`.
- [x] T117 `main.go`: flags, refresh loop, shutdown, `--no-auth` on loopback.
- [x] T118 unit tests for T110–T117.

## Phase C — gitty shares

- [x] T120 `config.go`, `init`: the `hub` key.
- [x] T121 `share.go`: `gitty share` — events, summary, `--dry-run`; clean
      default-branch checkouts only.
- [x] T122 share after `sync`, `fetch`, `sync --related`; `--no-share`.
- [x] T123 e2e: two workspaces, one hub, used-by seen from both.

## Phase D — retire what it replaces

- [x] T130 remove `cmd/gitty-mcp`, `gitty context`, `gitty related`,
      `gitty index`; keep what `fetch` and `sync --related` use.
- [x] T131 README; release and CI workflows build and smoke-test `gitty-up`.
- [x] T132 mark 001's spec and plan as superseded where they are.

## Phase E — against a real instance

- [x] T140 `gitty up` on gitlab.com groups, anonymous; share from a real
      workspace; walk it over HTTP.

## As built

- The hub's GitLab calls are a handful of GETs over `net/http`; it does not
  link the GitLab client.
- Sharing after a sync or fetch is reported on stderr and is not part of
  their event stream: a dry run cannot know what a real run would share, so
  it could not have parity there. `gitty share` is where the events are.
- A dry run of `gitty share` asks the hub what it has. That sends project
  paths and commit ids; it sends no facts.
- T130 also made the workspace index re-list the instance by itself once a
  day, since `gitty index` was the only thing that did.
- T140: `scripts/hub-smoke.sh`, run in CI: lists a real group, shares a real
  checkout, walks the tree in both protocol eras and records a note.
  Measured against gitlab.com, anonymous: a group of 86 projects is listed
  in 2 s; `gitlab-org` and `components` together, 3,878 projects in 850
  groups, in 18 s once pages were fetched side by side (86 s one at a time).
  The whole group tree of that listing is one answer of 31 KB, about 7,800
  tokens. Sharing a workspace of six checkouts takes 0.6 s.
- Not built: reading policy scopes, OAuth for MCP clients, private
  projects, verification on the hub's side that a commit it is sent is the
  default branch's (gitty checks this before sending; the hub trusts an
  authenticated sender).

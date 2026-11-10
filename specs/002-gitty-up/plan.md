# Implementation Plan: gitty-up

**Branch**: `001-cross-repo-context` | **Date**: 2026-11-10 | **Spec**: [spec.md](spec.md)

## Summary

`gitty-up` is a small HTTP service, one per GitLab instance, that holds what
is known about the instance's projects and groups and serves it to agents as
a tree they descend by reasoning. It learns in three ways and trusts them in
this order: what a repository says (submitted by `gitty` from a checkout it
has just read), what GitLab's listing says, and what an agent noted. It never
reads a repository itself.

`gitty` gains one action, `gitty share`, and loses the surfaces `gitty-up`
replaces: `gitty-mcp`, `gitty context`, `gitty related`, `gitty index`.

## Technical Context

**Language**: Go 1.24. **Dependencies**: none new. `gitty-up` uses only the
standard library — its GitLab calls are a handful of GETs over `net/http`,
so it does not link the GitLab client.
**Storage**: one directory of plain files (below).
**Protocol**: MCP over Streamable HTTP, revision 2026-07-28 (stateless, no
handshake, `server/discover`), and the 2025 handshake revisions on the same
endpoint for clients that have not moved.
**Testing**: unit tests in `cmd/gitty-up` against an `httptest` GitLab; e2e
in the root package with both real binaries, real git and the fake GitLab.

## Constitution Check

| Principle | How this complies |
|---|---|
| **CLI-first, safe by default** | `gitty share` is a command with events, a summary line and `--dry-run` parity. Sharing after a sync is reported on stderr and never changes sync's stdout or exit code. Nothing is deleted anywhere; a replaced note stays in the log. |
| **Minimal deps, structured output** | No new module. `gitty share` prints `share`, `skip`, `error`, `summary`. The hub logs to stderr. Tokens are never stored or logged, by either binary. |
| **Config-anchored workspaces** | The hub's address is a key in `.gitty/config` (`hub`). With no key nothing is sent anywhere. |
| **Nothing inside a repository** | Unchanged. |

## Design

### What the hub holds

```text
<data>/
  tree.json        groups and projects from the listing, public and internal only
  facts/<id>.json  one project's repository, as gitty read it at one commit
  notes.jsonl      every note ever recorded, one per line, never rewritten
```

Everything is keyed by GitLab's numeric ids, so a project that moves keeps
its facts and notes. All three are read into memory at start; `notes.jsonl`
is replayed to find the current note in each slot.

### The listing

On start and every `--refresh`, for each root group (`--group`, or every
top-level group the hub's token sees): the group, its descendant groups, and
its projects in the trimmed form, once live and once archived. That form
already carries description, visibility, last activity, stars, forks and
default branch. Projects and groups that are neither public nor internal are
dropped on the way in. A throttled request waits for as long as the instance
asks, says so on stderr, and gives up after a bounded number of tries.

### Facts: the repository's own account

`gitty` builds them from what feature 001's scanner already produces, plus
two things added here:

- **What the project provides.** CI components and templates (`templates/`,
  with a component's inputs read from its `spec:` header), security policies
  (`.gitlab/security-policies/policy.yml`), Helm charts, Terraform modules,
  a root Dockerfile.
- **`deploys` links.** An image of the instance's registry, or a URL of the
  instance, found in deployment configuration — Helm values,
  `kustomization.yaml`, `helmfile.yaml`, a GitLab agent's `config.yaml` — is
  a link of a new kind, so that an application's outline can say what
  deploys it.

Facts carry the commit, its date, and the blob id of each file that was
read, so that a note citing a file can later be checked against it.

`gitty share` asks the hub which of its checkouts it wants (`POST /v1/want`
with project and commit), then sends those (`PUT /v1/facts`). A checkout is
offered only if it is clean and at the tip of its default branch as the
remote has it. Facts replace older facts of the same project when their
commit is not older by date. Links to projects the hub does not cover are
dropped on arrival.

### Notes: what agents leave

Four slots per node. `account` and `usage` hold one entry each, `relation`
one per other project, `caveat` up to five. A new entry replaces the current
one when its author is the same, or stands at least as high on the project
in GitLab's own terms (a maintainer's account is not overwritten by a
passer-by; the refusal says to add a caveat instead). Text is bounded,
flattened to one line, and stored with author, time and evidence. Evidence
that names a file records that file's blob id from the facts of the moment;
an outline compares it with the facts of now.

### Standing

Use is the number of distinct projects whose facts link to a project, other
than by mention. Upkeep is: archived; what the project says of itself
(deprecated, experimental); days since last activity against `--quiet-after`.
Archived, deprecated and quiet projects are listed after the rest under a
heading that says which; experimental ones are labelled and sorted below
their peers. Order within a section: use, then last activity, then path.

### The tools

| Tool | Answers |
|---|---|
| `gitty_tree` | Groups, nested, a line each: counts and the group's account. Fitted to a byte budget by shortening accounts, then depth; says what it cut. |
| `gitty_browse` | One group: its account and notes, its subgroups, its projects in standing order, paged. |
| `gitty_search` | Projects and groups holding every word of a query in their path, account, topics, provided things or notes. |
| `gitty_outline` | One node. Project: its own words with their source, standing, what it provides, uses, is used by, is deployed by, relations and notes with their state, and the command that fetches it. Group: its account, what its projects most use, its most used projects. |
| `gitty_note` | Records or retires an entry in a slot. |

Answers are text, written to be read by a model: a stable line grammar, a
closing line of what to do next. Notes are always quoted, attributed and
dated, in a section of their own.

### MCP over HTTP

One endpoint, `POST /mcp`. A request that carries the per-request metadata
of 2026-07-28 is validated as that revision requires — `MCP-Protocol-Version`,
`Mcp-Method` and `Mcp-Name` headers checked against the body, the required
`_meta` keys, `resultType` on every result, `ttlMs` and `cacheScope` on the
tool list — and unknown methods are 404, unsupported versions 400 with the
supported list. A request without it is taken as a 2025 client: `initialize`
is answered, no session is issued, and the same tools are served. GET and
DELETE are 405. A present `Origin` that is not the server's own or an
allowed one is 403.

Every request carries `Authorization: Bearer <a token of the instance>`.
The hub asks the instance who that is (`GET /user`), remembers the answer for
five minutes under a hash of the token, and never keeps the token. For a
note it also asks, with the caller's token, whether they can see the node and
how they stand on it. `--no-auth` skips all of this and is refused on
anything but a loopback address.

## Source layout

```text
cmd/gitty-up/
  main.go      flags, start, shutdown
  gitlab.go    the few GitLab calls: listing, who is this, can they see that
  store.go     tree, facts, notes: load, save, replay
  standing.go  use, upkeep, order
  render.go    tree, browse, search, outline as text
  notes.go     slots and their rules
  mcp.go       JSON-RPC over HTTP, both protocol eras
  api.go       /v1/want, /v1/facts, /v1/info for gitty
internal/contract/contract.go   + Facts, Provided, the hub's wire types, KindDeploys
scan.go        + provides, deployment configuration
share.go       gitty share
```

Removed: `cmd/gitty-mcp/`, `context.go`, `briefing.go`, `indexcmd.go`, the
`related` command, `scripts/mcp-smoke.sh` (replaced by a hub smoke test).

## Phases

| Phase | Delivers |
|---|---|
| **A** | Scanner: provides and `deploys`; contract types |
| **B** | `gitty-up`: listing, store, auth, standing, tools, MCP, facts API |
| **C** | `gitty share`, the `hub` key, share after sync and fetch |
| **D** | Retire `gitty-mcp` and the local context commands; docs; release |
| **E** | Live check against gitlab.com |

## Not in this plan

- Reading policy scopes to say which policies apply to a project. Until
  then that relation is something an agent notes.
- OAuth for MCP clients. A static bearer header is what hosts are configured
  with today; the specification's own framework can be added beside it.
- Private projects, more than one hub process, a second transport.

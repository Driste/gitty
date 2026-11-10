# Feature Specification: gitty-up — the instance's shared context

**Feature Branch**: `001-cross-repo-context` (continues there; see Open Decisions)
**Created**: 2026-11-10
**Status**: Draft — awaiting the answers under "Open Decisions"
**Input**: User description: "Another binary that can run as an MCP
service/daemon … if the user asks 'let's add a pipeline for building an
image', the server will say there's a component project here for doing that,
and a policy project here for enforcing some configuration … connecting the
related gitops or IaC projects with their application code … organizational
context around how things are done and where the projects are. Just two
binaries: the main one for gitty actions and another called gitty-up (`gitty
up` runs it). A hierarchical tree index the LLM itself can reason through,
like PageIndex, lightweight. Use the agents that are doing the development to
create the context around the projects and groups instead of a fleshed-out
process that does it automatically. A project that isn't used much ranks
lower; one that isn't maintained isn't something to suggest. The
authoritative source is the repositories themselves."

## Problem

Feature 001 answers "what does this project lean on" from the checkouts on
one machine. That is the small half of the question. The large half is
organizational: *is there already a project here that does this, how do
teams here do it, and what else belongs to the thing I am changing* — the
component to include, the policy that will apply, the gitops repository that
deploys this service. None of that is on the developer's disk, and most of it
is not written in any one file. It is in people's heads, and lately in the
context of every agent that has had to work it out once and then forgot.

The agents doing the development are the ones who work it out. Each of them
reads a neighbouring project, learns what it is for and how it is used, and
throws that away when the session ends. gitty-up is where they put it down
instead: one shared, small, evidence-anchored memory of the instance, shaped
as the tree the instance already is, that the next agent walks by reasoning.

## Clarifications

### Session 2026-11-10

- Q: A third binary beside `gitty` and `gitty-mcp`? → A: **No. Two
  binaries.** `gitty` acts (sync, fetch, status). `gitty-up` knows. `gitty
  up` runs it, through the plugin rule that already exists. `gitty-mcp` is
  retired when `gitty-up` can do its job.
- Q: What happens to `gitty agent schema` and `context.md`? → A: **Removed**
  (done). They told an agent how to drive gitty; that is not this.
- Q: How does an agent find things — vector search, keyword search? → A: **A
  hierarchical tree the model reasons through**, after PageIndex: groups as
  folders, projects as documents, a project's outline as its structure. No
  embeddings, no model inside the server.
- Q: Who builds the context — a crawler that reads every repository? → A:
  **No. The agents that are doing the development.** Context accumulates
  where work happens. There is no process that reads the whole instance.
- Q: What ranks a suggestion? → A: **Use and upkeep.** A project few others
  use ranks lower. A project that is not maintained is not suggested.
- Q: When an agent's account and a repository disagree? → A: **The
  repository is the authority.**

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Find what the organization already has (Priority: P1)

An agent is asked to add a pipeline that builds a container image. Before
writing one, it asks gitty-up. It reads the group tree — the whole instance
in one answer, a line per group — sees a group of CI components, lists it,
and finds a build component with a one-line account of what it does, how
many projects use it and whether it is looked after. It opens the outline:
the inputs, one real project that uses it with the file and line, and what
earlier agents noted about using it. It writes the `include:` and moves on.

**Why this priority**: It is the question the feature exists for, and it
needs no link from the agent's own project — which is why feature 001 could
not answer it.

**Independent Test**: With a hub holding the tree of an instance, three
projects' contributed facts and one note, a client that knows only the
instance's root reaches the right component in at most four calls — tree,
browse, outline, and optionally search — and every statement in the answers
carries where it came from.

**Acceptance Scenarios**:

1. **Given** a hub that has listed the instance, **When** an agent asks for
   the tree, **Then** it gets every group, nested, a line each with its
   project count and its account of itself, within a size an agent can read
   in one call, and is told when anything was cut.
2. **Given** a group, **When** the agent browses it, **Then** each project is
   one line: name, what it is, how many projects use it, and any label that
   bears on choosing it (archived, deprecated, experimental, quiet).
3. **Given** projects that are archived, say of themselves that they are
   deprecated, or have seen no activity for the configured period, **When**
   they are listed or searched, **Then** they come after the others, under a
   heading that says why, and are never the first suggestion.
4. **Given** a keyword query, **When** the agent searches, **Then** projects
   whose path, account, provided things or notes hold every word are
   returned, ranked by use and upkeep.
5. **Given** a project nobody has contributed anything about, **When** its
   outline is asked for, **Then** the answer says what little is known (the
   listing), says plainly that nothing else is recorded, and says how to add
   to it.

---

### User Story 2 - An agent leaves what it learned (Priority: P1)

Working in project A, an agent has had to read project B to use it. It now
knows what B is for and the one thing that was not obvious about using it.
It records a note on B — a short account, the caveat, and the file in B that
shows it. The next agent to ask about B gets that note, labelled with who
wrote it, when, and whether the file it cites still says so.

**Why this priority**: Without it the hub is a listing. This is where the
context comes from.

**Independent Test**: An agent records a summary and a caveat on a project
and a relation from one project to another; a second client's outline of
each shows them, attributed; after the cited file changes upstream the
caveat is shown as possibly out of date.

**Acceptance Scenarios**:

1. **Given** a caller identified by a token of the instance who can see
   project B, **When** it records a note on B, **Then** the note is stored
   with its author, time, and the evidence it cites, and appears in B's
   outline.
2. **Given** a project with an account from an earlier agent, **When**
   another agent records a better one, **Then** the new one takes its place
   and the old one is kept in the history.
3. **Given** a note that says "A is deployed by C" where no file in A names
   C, **When** it is recorded as a relation, **Then** C appears in A's
   outline and A in C's, marked as an agent's account with its evidence.
4. **Given** a note whose cited file has changed since the commit it cites,
   **When** the project's outline is asked for, **Then** the note is marked
   as possibly out of date and ranks below those that still stand.
5. **Given** a caller who cannot see the project, or with no valid token,
   **When** it tries to record a note, **Then** it is refused and nothing is
   stored.
6. **Given** a group, **When** an agent records how things are done in it,
   **Then** the group's line in the tree and its own outline carry that
   account.

---

### User Story 3 - The repository speaks for itself (Priority: P2)

A developer's agent runs `gitty sync` in its workspace. For each project it
has just cloned or pulled, gitty reads the checkout as feature 001 taught it
to — what the project includes, depends on, builds from, and what its README
says — and hands that to the hub with the commit it was read at. The hub now
knows, from the repository itself, that this project uses that component;
the component's "used by" goes up by one, with a file and line. Nobody ran a
crawler. The instance's picture fills in where people work.

**Why this priority**: It is what makes "used by" worth having and gives
ranking its evidence, without the hub ever reading a repository itself. It
comes after US2 because the hub is useful with notes alone.

**Independent Test**: Two workspaces on two machines each sync a different
project that includes the same template; after both have run, the
template's outline lists both under "used by", each with file and line and
the commit it was read at.

**Acceptance Scenarios**:

1. **Given** a workspace whose config names a hub, **When** gitty clones or
   fast-forwards a project, **Then** it submits what the checkout says and
   prints one event line for it; with no hub configured nothing is sent.
2. **Given** facts for a project at an older commit, **When** facts for a
   newer commit of its default branch arrive, **Then** they replace the old
   ones, links that are gone are gone, and notes are kept.
3. **Given** a checkout that is not at a commit of the default branch, or
   has uncommitted changes, **When** gitty would submit, **Then** it does
   not: a branch in progress is not the project's account of itself.
4. **Given** `--dry-run`, **When** gitty would submit, **Then** it prints
   what it would send and sends nothing.
5. **Given** a repository's own words (its description, its README's first
   paragraph) and an agent's account of the same project, **When** the
   outline is composed, **Then** the repository's words come first and the
   agent's are shown as an agent's.

---

### User Story 4 - The whole stack of a service (Priority: P3)

An agent changing a service asks for its outline and sees, beside what the
service uses, what is built on it and around it: the gitops repository whose
values file pins its image, the Terraform that provisions what it runs on,
the policy project that adds jobs to its pipeline — each from a file and
line where a repository says so, or from an agent's note where none does.

**Why this priority**: It is US1–US3 put together and read from the other
side; it needs no machinery of its own beyond more kinds of evidence.

**Independent Test**: With facts contributed from a service, a gitops
repository that references its image and a policy project scoped to its
group, the service's outline names all three under the right headings.

---

### Edge Cases

- A note is text written by one session and read by another: it is the
  shape of a prompt injection. Notes are returned as attributed claims in a
  field of their own, bounded in length, plain text, never as instructions;
  the server's own guidance to agents says to treat them as a colleague's
  remark and to check the cited file before acting on it.
- An agent writes something wrong. The history is kept, any later note
  replaces it, and anything the repository contradicts loses to the
  repository.
- Many agents write the same thing. A project has one account, not a pile:
  a new one replaces the old. Caveats and usage notes are few per project;
  the oldest unconfirmed one falls off when the cap is reached.
- A project is active but says of itself that it is deprecated. (Seen on
  gitlab.com: a component marked DEPRECATED in its description had activity
  the same day.) Its own words outrank its activity.
- A project is new and unused. It ranks low but is listed; "used by 0" is
  shown as what is known, not as a verdict.
- A project moves or is deleted. The listing notices; its notes follow a
  move and are retired with a deletion.
- Facts arrive for a project the hub has not listed (private, or outside
  what it covers). They are refused; nothing about it is stored.
- The hub is down. `gitty` works exactly as it does without one; a failed
  submission is a note on stderr, never a failed sync.
- Two agents record a note on the same project in the same second. Both are
  in the history; the later one is current.
- The hub's disk is lost. The tree is rebuilt from the instance, facts
  return as people work; notes are what a backup is for, and the store is
  one directory of plain files.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The project MUST ship exactly two binaries: `gitty` and
  `gitty-up`. `gitty up` MUST run `gitty-up` through the existing plugin
  rule. `gitty-mcp` MUST be removed once `gitty-up` serves agents.
- **FR-002**: `gitty-up` MUST run as a long-lived service speaking MCP over
  HTTP, using only the standard library and the modules gitty already has.
- **FR-003**: The hub MUST hold the tree of the instance — groups and
  projects with the metadata a listing gives (description, visibility,
  archived, last activity, stars, forks) — refreshed by listing on a
  schedule. Listing is the only thing it fetches from the instance on its
  own account. It MUST NOT clone or read repositories on a schedule.
- **FR-004**: The hub MUST offer reading tools that let a model descend the
  tree: the group tree to a chosen depth; a group's projects; keyword
  search; a project's (or group's) outline. Every answer MUST be bounded in
  size, say when it was cut and how to get the rest, and end with what to
  do next.
- **FR-005**: The hub MUST offer writing tools by which an agent records,
  on any project or group it can see: an account of what it is; how it is
  used here; a caveat; a relation to another project with the reason.
  Entries are bounded in length and may cite evidence as project, file,
  line and commit.
- **FR-006**: Every stored entry MUST carry who wrote it (the GitLab user
  behind the caller's token), when, and its evidence. Outlines MUST show
  those, and MUST distinguish three sources and order them so: what a
  repository says, what GitLab says, what an agent noted.
- **FR-007**: `gitty`, when its workspace config names a hub, MUST submit
  what it reads from a checkout — links with evidence, the README's account,
  what the project provides — after it clones or fast-forwards it, with the
  commit it was read at, and only for a clean checkout at a commit of the
  default branch. It MUST honour `--dry-run`, print one event per
  submission, and treat a hub that does not answer as a note, not an error.
- **FR-008**: Facts for a project MUST be replaced as a whole by facts for a
  newer commit. "Used by" for a project MUST be the projects whose current
  facts link to it, each with file, line and commit.
- **FR-009**: Ranking in listings and search MUST combine use (how many
  projects' facts link to it) and upkeep (archived; what the project says
  of itself; time since last activity, against a configurable period).
  Projects that fail on upkeep MUST be listed last under a stated reason and
  MUST NOT be offered first. Nothing known is hidden.
- **FR-010**: A note whose cited file differs from the cited commit in the
  latest facts MUST be marked as possibly out of date.
- **FR-011**: Every request MUST carry a token of the instance. The hub MUST
  refuse a request whose token the instance does not accept, MUST refuse a
  write about a project the caller cannot see, and MUST NOT store or log a
  token.
- **FR-012**: By default the hub MUST cover only projects whose visibility
  is public or internal, so that no answer tells a caller of the instance
  anything they could not have browsed.
- **FR-013**: The store MUST be one directory of plain, versioned files,
  written atomically, holding no credential; the history of notes MUST be
  kept.
- **FR-014**: Nothing in this feature writes inside a repository, and
  nothing is specific to one agent vendor.
- **FR-015**: Once the hub answers them, `gitty context`, `gitty related`
  and `gitty index` MUST be removed from `gitty`. The scanner stays, as what
  `gitty` submits from. `gitty fetch` and `gitty sync --related` stay.

### Key Entities

- **Node**: a group or a project of the instance, by namespace path; what
  the listing says of it.
- **Facts**: what one project's repository says at one commit — its links
  (kind, target, evidence, what it points at), its README's first
  paragraph, what it provides. Whole, replaceable, from the repository.
- **Note**: what an agent recorded on a node — a slot (account, usage,
  caveat, relation), text, the other project for a relation, evidence,
  author, time, and whether its evidence still stands.
- **Outline**: a node as an agent reads it — its own words, what it
  provides, what it uses, what uses it, what is around it, the notes on it,
  each line with its source.
- **Standing**: a project's use and upkeep, and the labels that follow.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The whole group tree of an instance with 4,000 projects in 650
  groups is one answer of under 10,000 tokens. (Measured in a sketch on
  gitlab.com: 3,878 projects, 639 groups, about 7,800 tokens.)
- **SC-002**: From the root, an agent that has never seen the instance
  reaches the right project for a stated need in at most four calls, in a
  fixture where one exists.
- **SC-003**: Every statement in an outline is attributable: a file and line
  at a commit, a field of the listing, or a named author and a time. Zero
  statements without a source.
- **SC-004**: A note recorded by one client is in the next outline another
  client asks for.
- **SC-005**: A hub with nothing contributed yet starts answering within one
  listing of the instance, and a `gitty` with no hub configured passes every
  existing test.
- **SC-006**: No new Go module dependency; nothing written inside a
  repository; no token in the store or the logs.

## Assumptions

- The agents that use the hub have a shell and a workspace, or neither. With
  a shell they run `gitty fetch` to get a project the hub named; the hub
  does not serve file contents.
- GitLab's permissions are the trust gradient: a note by someone who can
  push to the project is worth more than one by someone who can only see it,
  and the instance can say which is which for any token.
- The listing gitty already makes carries last activity, stars, forks and
  visibility. (Checked on gitlab.com.) Last activity alone is not upkeep.
- An agent told plainly what is worth recording, and shown what is missing,
  will record it. If that turns out false the hub still has the tree and the
  facts, and US2 needs rethinking rather than the rest.
- One hub per instance, one process, one directory. Availability beyond
  "restart it" is not this feature.

## Open Decisions

Defaults are what will be built unless answered otherwise.

1. **Does `gitty` submit facts on its own when it syncs?** The request says
   agents create the context, and that the repositories are the authority.
   Submitting what `gitty` already reads from a checkout is how the
   repository's own account reaches the hub without a crawler. It is
   mechanical, though, and it does send file names and line numbers of
   references to the hub. *Default: yes, whenever a hub is configured;
   `--dry-run` shows it; a flag turns it off.*
2. **Where notes live.** In the hub only, or may a repository carry its own
   account in a file the hub prefers? *Default: the hub only. The
   repository's README and description already outrank notes, and feature
   001 settled that gitty writes nothing inside repositories.*
3. **How long before a quiet project counts as unmaintained.** *Default: 12
   months without activity, configurable; archived and self-declared
   deprecated at once.*
4. **Private projects.** *Default: not covered. Per-caller filtering is a
   later feature.*
5. **Branch.** *Default: continue on `001-cross-repo-context`, since this
   replaces that feature's server.*
6. **The local commands of feature 001** (FR-015). *Default: `gitty
   context`, `gitty related` and `gitty index` go once the hub answers the
   same questions; `gitty fetch` and `gitty sync --related` stay, reading
   the local index as they do now.*

## Out of Scope

- Reading every repository of the instance from the hub, on a schedule or
  otherwise. An on-demand read of one project when an agent asks about it
  is a possible later addition; a crawler is not.
- A model inside the server: embeddings, generated summaries, relevance
  scoring.
- Serving file contents, cloning, or anything else that is `gitty`'s.
- Writing notes back into repositories. An agent's note that deserves to be
  the repository's own words gets there the way anything does: someone opens
  a merge request.
- Per-caller visibility of private projects, several hubs, replication.

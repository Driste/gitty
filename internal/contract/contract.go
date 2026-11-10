// Package contract holds the JSON shapes gitty writes for other programs to
// read: the workspace index file, and what `gitty context --format=json` and
// `gitty fetch --format=json` print.
//
// They are public interfaces. gitty's own plugins (gitty-mcp among them) are
// built on nothing else — a plugin drives the gitty CLI and decodes these
// types — so anything a plugin can do, a script can do too. The package has
// no behaviour on purpose: what is shared between the CLI and a plugin is
// vocabulary, never an implementation.
//
// Compatibility: within one version number, fields are only ever added. A
// reader must ignore fields it does not know and must refuse a version it
// does not know rather than guess at its shape (see Supported).
package contract

import (
	"fmt"
	"time"
)

// Schema is the version of the documents gitty prints on stdout: Briefing
// and FetchReport. It changes only when an existing field changes meaning or
// goes away.
const Schema = 1

// IndexVersion is the version of the index file. An index with any other
// version is rebuilt by gitty, and refused by everything else.
const IndexVersion = 1

// ScannerVersion identifies the rules links are extracted with. It is not a
// format version: an index whose scanner is older reads fine, and gitty
// rescans its checkouts so that links found by newer rules appear.
const ScannerVersion = 1

// Supported reports whether a document of the given version can be read by
// code built against this package, and if not, an error naming both sides —
// what a reader should surface instead of decoding further.
func Supported(what string, got, want int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%s has version %d, but this program reads version %d: gitty and its plugin come from different releases", what, got, want)
}

// The kinds of link between two projects. The set is fixed: a consumer can
// switch on it.
const (
	KindDepends     = "depends"      // a package manifest or script names the project
	KindCIInclude   = "ci-include"   // .gitlab-ci.yml includes configuration from it
	KindCITrigger   = "ci-trigger"   // .gitlab-ci.yml triggers its pipeline
	KindCIComponent = "ci-component" // .gitlab-ci.yml uses a CI/CD component it publishes
	KindImage       = "image"        // a container image from its registry is used
	KindSubmodule   = "submodule"    // it is a git submodule
	KindFork        = "fork"         // the project was forked from it
	KindMentions    = "mentions"     // documentation refers to it
)

// Index is the file <workspace>/.gitty/index.json: every project of the
// workspace's part of the instance, and every reference found between them.
// `gitty index` writes it; `gitty context` and `gitty related` keep it fresh.
//
// Nothing in it depends on where the workspace is on disk — directories are
// relative to the workspace root — and it never holds a credential.
type Index struct {
	Version  int    `json:"version"`
	Scanner  int    `json:"scanner"`  // see ScannerVersion
	Instance string `json:"instance"` // the instance URL from the workspace config
	// Root is the namespace path the workspace's root directory stands for
	// ("" when the workspace is anchored at the instance's top level). A
	// project's directory is its path with this prefix removed.
	Root string `json:"root"`
	// Hosts are the host names under which the instance's repositories are
	// referred to: the configured one, and any other the API advertises.
	Hosts []string `json:"hosts,omitempty"`
	// Registry is the host (and port, and path, if any) of the instance's
	// container registry: an image named <registry>/<project path>[/...]
	// belongs to that project. Empty when the instance has none, or when no
	// checkout has been looked up yet.
	Registry string `json:"registry,omitempty"`
	// Exclude is the exclude list in effect when the index was last brought
	// up to date. No link in the index starts or ends at a project it
	// covers; gitty rescans when the list in effect differs.
	Exclude     []string  `json:"exclude,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	// ListedAt is when project metadata was last read from the API; scanning
	// checkouts for links does not need the network and does not move it.
	ListedAt time.Time `json:"listed_at"`

	Projects   map[string]*Project `json:"projects"` // keyed by namespace path
	Links      []Link              `json:"links"`
	Unresolved []Unresolved        `json:"unresolved,omitempty"`
}

// Project is one project of the instance as the index knows it.
type Project struct {
	// ID is GitLab's project id. Zero means the project is not known
	// upstream: a checkout made by hand, or one whose project is gone.
	ID            int64    `json:"id,omitempty"`
	Description   string   `json:"description,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	Archived      bool     `json:"archived,omitempty"`
	WebURL        string   `json:"web_url,omitempty"`
	// ForkedFrom is the namespace path of the project this one was forked
	// from. It is known for projects that are checked out: a listing does
	// not carry it, and gitty looks it up per checkout.
	ForkedFrom string `json:"forked_from,omitempty"`
	// Local is present when the project is checked out in the workspace.
	Local *Local `json:"local,omitempty"`
}

// Local describes a project's checkout.
type Local struct {
	Dir  string `json:"dir"`  // relative to the workspace root, forward slashes
	Head string `json:"head"` // the commit the checkout was at when last scanned
	// Docs are the documentation files at the checkout's root worth reading
	// first, relative to Dir: agent instructions first, the README last.
	Docs    []string `json:"docs,omitempty"`
	Summary string   `json:"summary,omitempty"` // the README's opening paragraph
}

// Link is one evidenced reference from a project to another. From uses To.
type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	// Evidence is where the reference was found: "file:line" inside From's
	// checkout, or "api:<field>" for what only the API knows (a fork).
	Evidence string `json:"evidence"`
}

// Unresolved is a reference that looks like a project of the instance but
// matches none the index knows — moved, deleted, private, or outside the
// workspace's part of the namespace. It is reported, never guessed at.
type Unresolved struct {
	From     string `json:"from"`
	Ref      string `json:"ref"`
	Kind     string `json:"kind"` // the kind of link it would have been
	Evidence string `json:"evidence"`
}

// Briefing is what `gitty context --format=json` prints: one project (or
// group) and the projects it is linked to, each with where it is on this
// machine and why it matters.
type Briefing struct {
	Schema    int     `json:"schema"`
	Instance  string  `json:"instance"`
	Workspace string  `json:"workspace"` // absolute path of the workspace root
	Subject   Subject `json:"subject"`

	// Uses are the projects the subject refers to; UsedBy the ones that
	// refer to it. Either list may be capped: the totals say how many
	// there are in all.
	Uses        []Related `json:"uses"`
	UsedBy      []Related `json:"used_by"`
	UsesTotal   int       `json:"uses_total"`
	UsedByTotal int       `json:"used_by_total"`

	// Within holds the links among a group subject's own projects.
	Within []Link `json:"within,omitempty"`

	// Coverage says what the answer rests on. Links are found by reading
	// checkouts, so a project that is not checked out is known to exist (and
	// can be something others use) but is not known to use anything: UsedBy
	// is complete only as far as the workspace is.
	Coverage Coverage `json:"coverage"`

	Unresolved []Unresolved `json:"unresolved,omitempty"`
	// Notes are things the reader should know about this answer: a list
	// that was capped, an index that could not be refreshed.
	Notes     []string  `json:"notes,omitempty"`
	IndexedAt time.Time `json:"indexed_at"`
}

// Coverage is how much of the workspace's part of the instance has been
// read for links.
type Coverage struct {
	Projects   int `json:"projects"`    // projects the index knows of
	CheckedOut int `json:"checked_out"` // of those, checked out here and read
}

// Subject is what a briefing is about: a project, or a group of them.
type Subject struct {
	Kind string `json:"kind"` // "project" or "group"
	// Project is the subject's namespace path when it is a project; Group
	// when it is a group.
	Project string `json:"project,omitempty"`
	Group   string `json:"group,omitempty"`

	// Present says whether it exists on this machine; Path is where
	// (absolute) and RelativePath the same from where the command ran.
	Present      bool   `json:"present"`
	Path         string `json:"path,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`

	// For a project.
	Description   string   `json:"description,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	Archived      bool     `json:"archived,omitempty"`
	WebURL        string   `json:"web_url,omitempty"`
	Docs          []string `json:"docs,omitempty"` // relative to Path
	Summary       string   `json:"summary,omitempty"`
	// LocalOnly marks a checkout whose project is not known upstream.
	LocalOnly bool `json:"local_only,omitempty"`

	// For a group: its projects.
	Projects []Member `json:"projects,omitempty"`
}

// Member is one project of a group subject.
type Member struct {
	Project      string `json:"project"`
	Description  string `json:"description,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	Present      bool   `json:"present"`
	Path         string `json:"path,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`
}

// Related is one project linked to the subject.
type Related struct {
	Project     string `json:"project"` // namespace path
	Description string `json:"description,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	WebURL      string `json:"web_url,omitempty"`

	// Present says whether the project is checked out on this machine; when
	// it is, Path is where (absolute) and RelativePath the same from where
	// the command ran. Fetched is true when this very call cloned it.
	Present      bool   `json:"present"`
	Fetched      bool   `json:"fetched"`
	Path         string `json:"path,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`

	// When it is not present: the command that would fetch it and, if
	// fetching was attempted and failed, why.
	FetchCommand string `json:"fetch_command,omitempty"`
	FetchError   string `json:"fetch_error,omitempty"`

	// Links are the references that relate the two, each with its evidence.
	// In Uses the evidence is in the subject's files; in UsedBy it is in
	// this project's.
	Links []Link `json:"links"`

	Docs    []string `json:"docs,omitempty"` // relative to Path
	Summary string   `json:"summary,omitempty"`
}

// FetchReport is what `gitty fetch --format=json` prints.
type FetchReport struct {
	Schema  int           `json:"schema"`
	Results []FetchResult `json:"results"`
}

// FetchResult is the outcome for one project named to `gitty fetch`.
type FetchResult struct {
	Project string `json:"project"`        // namespace path, as asked for
	Path    string `json:"path,omitempty"` // absolute path of the checkout, when there is one
	Present bool   `json:"present"`        // checked out when the command finished
	Fetched bool   `json:"fetched"`        // cloned by this command
	Error   string `json:"error,omitempty"`
}

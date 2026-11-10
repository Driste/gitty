// Package contract holds the JSON shapes gitty writes for other programs to
// read: the workspace index file, what `gitty fetch --format=json` prints,
// and what gitty sends a hub (gitty-up) and reads back from it.
//
// They are public interfaces. gitty-up is built on nothing else from gitty,
// so anything it is told, a script could tell it too. The package has no
// behaviour on purpose: what the two binaries share is vocabulary, never an
// implementation.
//
// Compatibility: within one version number, fields are only ever added. A
// reader must ignore fields it does not know and must refuse a version it
// does not know rather than guess at its shape (see Supported).
package contract

import (
	"fmt"
	"time"
)

// Schema is the version of the documents gitty prints on stdout:
// FetchReport. It changes only when an existing field changes meaning or
// goes away.
const Schema = 1

// IndexVersion is the version of the index file. An index with any other
// version is rebuilt by gitty, and refused by everything else.
const IndexVersion = 1

// ScannerVersion identifies the rules links are extracted with. It is not a
// format version: an index whose scanner is older reads fine, and gitty
// rescans its checkouts so that links found by newer rules appear.
const ScannerVersion = 4

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
	// KindDeploys is the one kind that runs the other way round from use:
	// From is deployment configuration (Helm values, a kustomization, a
	// GitLab agent's config) and To is the project it deploys.
	KindDeploys = "deploys"
)

// Index is the file <workspace>/.gitty/index.json: every project of the
// workspace's part of the instance, and every reference found between them.
// gitty keeps it by itself: `gitty sync --related` and `gitty share` bring it
// up to date before they read it.
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
	// Namespaces are the namespace paths whose projects were listed: for
	// anything beneath them, Projects is the whole truth as of ListedAt. A
	// project outside them is in Projects only because something referred
	// to it and the instance was asked.
	Namespaces []string `json:"namespaces,omitempty"`
	// Moved maps a path that projects are still referred to by onto the path
	// the project has now. GitLab keeps answering for the old one; this is
	// how a reference to it is followed without asking again.
	Moved map[string]string `json:"moved,omitempty"`
	// NotFound are references to paths outside Namespaces that the instance
	// was asked about and has no project for. They are not asked about again
	// until the instance is next listed.
	NotFound []string `json:"not_found,omitempty"`
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
	// Provides is what the project offers others, as far as its files say.
	Provides []Provided `json:"provides,omitempty"`
}

// The kinds of thing a project can be seen to provide.
const (
	ProvidesCIComponent = "ci-component"     // templates/<name>.yml with a spec: header
	ProvidesCITemplate  = "ci-template"      // templates/<name>.yml without one
	ProvidesPolicy      = "policy"           // a security policy in .gitlab/security-policies/policy.yml
	ProvidesHelmChart   = "helm-chart"       // Chart.yaml
	ProvidesTerraform   = "terraform-module" // *.tf at the root or under modules/
	ProvidesImage       = "image"            // a Dockerfile at the root
)

// Provided is one thing a project offers: a CI component, a pipeline
// template, a policy, a chart, a module, an image.
type Provided struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	File string `json:"file"` // where it is defined, relative to the project
	// About is what the file says the thing is, when it says so: a chart's
	// description, a policy's type.
	About string `json:"about,omitempty"`
	// Inputs are the parameters it takes: a component's inputs, a module's
	// variables.
	Inputs []string `json:"inputs,omitempty"`
}

// Link is one evidenced reference from a project to another. From uses To.
type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	// Evidence is where the reference was found: "file:line" inside From's
	// checkout, or "api:<field>" for what only the API knows (a fork).
	Evidence string `json:"evidence"`
	// Target says what in To the reference is to, when the reference says:
	// the file a pipeline includes, the component it uses, the path a URL
	// points at. Several are separated by ", ".
	Target string `json:"target,omitempty"`
}

// Unresolved is a reference that looks like a project of the instance but
// matches none the index knows — moved, deleted, private, or outside the
// workspace's part of the namespace. It is reported, never guessed at.
type Unresolved struct {
	From     string `json:"from"`
	Ref      string `json:"ref"`
	Kind     string `json:"kind"` // the kind of link it would have been
	Evidence string `json:"evidence"`
	Target   string `json:"target,omitempty"` // as Link.Target
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

// --- gitty to gitty-up ---

// HubSchema is the version of what gitty sends a hub and reads from it.
const HubSchema = 1

// Facts is what one project's repository says at one commit: what gitty
// read from a clean checkout of its default branch, sent to a hub so that
// the instance's picture is made of what repositories say and not of what
// anyone remembers.
type Facts struct {
	Schema      int        `json:"schema"`
	Scanner     int        `json:"scanner"`
	Project     string     `json:"project"` // namespace path
	ID          int64      `json:"id"`      // GitLab's project id
	Commit      string     `json:"commit"`
	CommittedAt time.Time  `json:"committed_at"`
	Summary     string     `json:"summary,omitempty"` // the README's opening paragraph
	Docs        []string   `json:"docs,omitempty"`
	Provides    []Provided `json:"provides,omitempty"`
	Links       []Link     `json:"links,omitempty"`
	// Files are the blob ids of tracked files, abbreviated: enough to tell
	// later whether a file someone cited has changed. Large repositories
	// send only the files that were read.
	Files map[string]string `json:"files,omitempty"`
}

// HubHead names a checkout gitty could send facts for.
type HubHead struct {
	Project string `json:"project"`
	ID      int64  `json:"id"`
	Commit  string `json:"commit"`
}

// HubWant asks a hub which of these it does not have yet.
type HubWant struct {
	Schema int       `json:"schema"`
	Have   []HubHead `json:"have"`
}

// HubWanted is the answer: the projects to send, and why each of the others
// is not wanted.
type HubWanted struct {
	Schema  int               `json:"schema"`
	Want    []string          `json:"want"`
	Skipped map[string]string `json:"skipped,omitempty"`
}

// HubReceipt is what a hub says about facts it was sent.
type HubReceipt struct {
	Schema  int    `json:"schema"`
	Project string `json:"project"`
	Stored  bool   `json:"stored"`
	Reason  string `json:"reason,omitempty"` // why not, when not
	Links   int    `json:"links"`            // links kept: those to projects the hub covers
}

// HubInfo is what a hub says about itself.
type HubInfo struct {
	Schema   int       `json:"schema"`
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Instance string    `json:"instance"`
	Projects int       `json:"projects"`
	Groups   int       `json:"groups"`
	ListedAt time.Time `json:"listed_at"`
}

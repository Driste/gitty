package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gitty/internal/contract"
)

// The tools.
//
// They come in two kinds, kept apart on purpose.
//
// Index-backed tools — gitty_context, gitty_related, gitty_projects — answer
// from what gitty knows about the instance and the workspace. This plugin
// computes none of it: it runs gitty and passes the answer on. Any process
// holding the index could serve them, which is the seam a shared, remote
// server would be cut along.
//
// Workspace-backed tools — gitty_fetch, gitty_search, gitty_read — act on the
// checkouts on this machine, and are local by nature.

const serverName = "gitty-mcp"

// server is the state shared by every call.
type server struct {
	gittyBin  string // the gitty to run for everything this plugin does not do itself
	workspace string // the workspace root, as gitty found it
	dir       string // where the server was started: the default project is the one here
	noFetch   bool   // read-only: nothing is ever cloned
	anon      bool   // gitty is told to reach the instance without a token
	jobs      int    // passed to gitty when > 0
	errOut    io.Writer

	// fetchMu lets one call at a time clone. Two agents asking for the same
	// absent project at once should result in one clone and one "already
	// there", not two git processes in one directory.
	fetchMu sync.Mutex

	idxMu      sync.Mutex
	idx        *contract.Index
	idxModTime time.Time
	idxSize    int64
}

// tool is one MCP tool: how it is described to the client, and what it does.
type tool struct {
	name        string
	title       string
	description string
	schema      map[string]any
	readOnly    bool // makes no change anywhere
	run         func(ctx context.Context, s *server, args json.RawMessage) (*toolResult, error)
}

type toolResult struct {
	Content           []toolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// structured returns a result carrying v both as data and, for clients that
// only read text, as the same JSON.
func structured(v any) (*toolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the result: %w", err)
	}
	return &toolResult{Content: []toolContent{{Type: "text", Text: string(data)}}, StructuredContent: v}, nil
}

func textResult(text string) *toolResult {
	return &toolResult{Content: []toolContent{{Type: "text", Text: text}}}
}

// call runs a tool. Whatever goes wrong while it runs — bad arguments, a
// gitty that fails, a file that may not be read — is an answer the model
// should see and can act on, so it is returned as a result marked as an
// error, never as a protocol failure.
func (s *server) call(ctx context.Context, t *tool, args json.RawMessage) *toolResult {
	res, err := t.run(ctx, s, args)
	if err != nil {
		return &toolResult{Content: []toolContent{{Type: "text", Text: err.Error()}}, IsError: true}
	}
	return res
}

func (s *server) tool(name string) *tool {
	for _, t := range s.tools() {
		if t.name == name {
			return t
		}
	}
	return nil
}

// tools are the tools this server offers. A read-only server does not offer
// gitty_fetch at all: what cannot be done is better not advertised.
func (s *server) tools() []*tool {
	out := make([]*tool, 0, len(allTools))
	for _, t := range allTools {
		if t.name == "gitty_fetch" && s.noFetch {
			continue
		}
		out = append(out, t)
	}
	return out
}

// toolList renders the tools for tools/list.
func (s *server) toolList() []map[string]any {
	var out []map[string]any
	for _, t := range s.tools() {
		description := t.description
		if t.name == "gitty_context" && s.noFetch {
			description += " This server is read-only: it never clones, and projects that are not checked out are reported as absent."
		}
		// The only thing any tool ever changes is which projects are cloned,
		// and a read-only server does not do that either.
		readOnly := t.readOnly || s.noFetch
		out = append(out, map[string]any{
			"name":        t.name,
			"title":       t.title,
			"description": description,
			"inputSchema": t.schema,
			"annotations": map[string]any{
				"title":           t.title,
				"readOnlyHint":    readOnly,
				"destructiveHint": false,
				"idempotentHint":  true,
				"openWorldHint":   !readOnly,
			},
		})
	}
	return out
}

func (s *server) instructions() string {
	text := "gitty links the projects of one GitLab instance. Call gitty_context to learn which other projects the one you are working in uses and which use it: each comes with the absolute path of its checkout on this machine, the kind of link, and the file and line that proves it. "
	if s.noFetch {
		text += "This server is read-only and never clones. "
	} else {
		text += "By default gitty_context clones the projects it uses that are not checked out yet, so their paths exist when it returns. "
	}
	text += "Pass `project` as a namespace path (group/project) or as the absolute path of a directory inside the workspace, such as your own working directory. \"Used by\" only covers projects that are checked out; `coverage` says how many that is. Use gitty_search and gitty_read to look inside related projects when you cannot open their paths yourself."
	return text
}

// --- arguments ---

// decodeArgs reads a tool's arguments strictly: a misspelt name is an error
// the model can correct, not something to ignore.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid arguments: %v", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

// checkProjectArg rejects a project argument that could be mistaken for
// anything but a name: gitty is run with it on its command line.
func checkProjectArg(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\x00\n\r") || len(p) > 1024 {
		return fmt.Errorf("invalid project %q: give a namespace path such as group/project, or the absolute path of a directory", p)
	}
	return nil
}

func checkDirection(d string) error {
	switch d {
	case "", "uses", "used-by", "both":
		return nil
	}
	return fmt.Errorf("invalid direction %q: choose uses, used-by or both", d)
}

// --- running gitty ---

// gitty runs the gitty that launched this plugin and returns what it wrote
// to stdout, its exit code, and the tail of what it wrote to stderr. Its
// stderr is also passed through to this server's own, where the host keeps
// it; gitty redacts credentials from everything it prints.
func (s *server) gitty(ctx context.Context, args ...string) (stdout []byte, code int, diag string, err error) {
	// Whether to go without a token is this server's setting, not a tool
	// argument: it goes on every command, right after the command's name.
	if s.anon && len(args) > 0 {
		args = append([]string{args[0], "--anon"}, args[1:]...)
	}
	cmd := exec.CommandContext(ctx, s.gittyBin, args...)
	cmd.Dir = s.dir
	var out bytes.Buffer
	tail := &tailBuffer{max: 4 << 10}
	cmd.Stdout = &out
	cmd.Stderr = io.MultiWriter(tail, s.errOut)
	// On cancellation gitty gets an interrupt, which it turns into a clean
	// stop: a clone cut short is left resumable.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	runErr := cmd.Run()
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return out.Bytes(), 0, tail.String(), nil
	case ctx.Err() != nil:
		return nil, -1, tail.String(), fmt.Errorf("cancelled")
	case errors.As(runErr, &exit):
		return out.Bytes(), exit.ExitCode(), tail.String(), nil
	}
	return nil, -1, tail.String(), fmt.Errorf("running %s: %v", s.gittyBin, runErr)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// gittyFailure turns a gitty run that produced nothing usable into the error
// a tool reports: what gitty itself said, last line first in importance.
func gittyFailure(what string, code int, diag string) error {
	lines := strings.Split(strings.TrimSpace(diag), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	said := strings.TrimSpace(strings.Join(lines, "\n"))
	if said == "" {
		return fmt.Errorf("%s failed (gitty exited %d)", what, code)
	}
	return fmt.Errorf("%s failed (gitty exited %d):\n%s", what, code, said)
}

// --- index-backed tools ---

// briefing runs `gitty context --format=json` and decodes the result.
// Exit 1 with a briefing on stdout is a briefing: it means a fetch failed,
// and the briefing says which.
func (s *server) briefing(ctx context.Context, project, direction string, fetch bool) (*contract.Briefing, error) {
	if err := checkProjectArg(project); err != nil {
		return nil, err
	}
	if err := checkDirection(direction); err != nil {
		return nil, err
	}
	args := []string{"context", "--format=json"}
	if direction != "" {
		args = append(args, "--direction="+direction)
	}
	if s.jobs > 0 {
		args = append(args, fmt.Sprintf("--jobs=%d", s.jobs))
	}
	if fetch {
		args = append(args, "--fetch")
		s.fetchMu.Lock()
		defer s.fetchMu.Unlock()
	}
	if project != "" {
		args = append(args, "--", project)
	}
	out, code, diag, err := s.gitty(ctx, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != 1 || len(bytes.TrimSpace(out)) == 0 {
		return nil, gittyFailure("gitty context", code, diag)
	}
	var b contract.Briefing
	if err := json.Unmarshal(out, &b); err != nil {
		return nil, fmt.Errorf("gitty context did not print a briefing (%v)", err)
	}
	if err := contract.Supported("the briefing", b.Schema, contract.Schema); err != nil {
		return nil, err
	}
	return &b, nil
}

var toolContext = &tool{
	name:  "gitty_context",
	title: "Context of a project",
	description: "Which other projects of the GitLab instance a project uses, and which use it. For each: its description, " +
		"`path` (the absolute path of its checkout on this machine), the `links` that relate the two — each with a `kind` " +
		"(depends, ci-include, ci-trigger, ci-component, image, submodule, fork, mentions) and `evidence` (file:line in the project " +
		"that holds the reference) — and the documentation files worth reading in it. Call it before changing a project, to find the " +
		"code it depends on and the code that depends on it. Projects it uses that are not checked out are cloned first (`fetched` " +
		"says which), so their paths exist when the call returns; projects already checked out are never pulled or changed. " +
		"`used_by` only covers projects that are checked out: `coverage` says how much of the namespace that is.",
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type":        "string",
				"description": "The project (or group) to brief: a namespace path such as `acme/app`, or the absolute path of a directory inside the workspace — your working directory, for instance. Omitted: the project the server was started in.",
			},
			"fetch": map[string]any{
				"type":        "boolean",
				"description": "Clone the projects it uses that are not checked out. Defaults to true unless the server is read-only. Pass false for a quick answer that touches nothing.",
			},
			"direction": map[string]any{
				"type":        "string",
				"enum":        []string{"uses", "used-by", "both"},
				"description": "Which side to return. The totals of both sides are always given.",
			},
		},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Project   string `json:"project"`
			Fetch     *bool  `json:"fetch"`
			Direction string `json:"direction"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		fetch := !s.noFetch && (args.Fetch == nil || *args.Fetch)
		b, err := s.briefing(ctx, args.Project, args.Direction, fetch)
		if err != nil {
			return nil, err
		}
		if s.noFetch && args.Fetch != nil && *args.Fetch {
			b.Notes = append(b.Notes, "this server is read-only (--no-fetch): nothing was cloned")
		}
		return structured(b)
	},
}

// relatedView is a briefing reduced to its links.
type relatedView struct {
	Subject     relatedSubject        `json:"subject"`
	Uses        []relatedEntry        `json:"uses"`
	UsedBy      []relatedEntry        `json:"used_by"`
	UsesTotal   int                   `json:"uses_total"`
	UsedByTotal int                   `json:"used_by_total"`
	Within      []contract.Link       `json:"within,omitempty"`
	Unresolved  []contract.Unresolved `json:"unresolved,omitempty"`
	Coverage    contract.Coverage     `json:"coverage"`
}

type relatedSubject struct {
	Kind    string `json:"kind"`
	Project string `json:"project,omitempty"`
	Group   string `json:"group,omitempty"`
}

type relatedEntry struct {
	Project string          `json:"project"`
	Present bool            `json:"present"`
	Path    string          `json:"path,omitempty"`
	Links   []contract.Link `json:"links"`
}

var toolRelated = &tool{
	name:  "gitty_related",
	title: "Links of a project",
	description: "The links of a project and nothing else: the projects it uses and the ones that use it, each with the kind of link and " +
		"its evidence (file:line). Never clones. Use it for a quick look at the shape of things, for instance to see everything that " +
		"would be affected by a change to a library; use gitty_context when you want descriptions, documentation and checkouts.",
	readOnly: true,
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type":        "string",
				"description": "A namespace path such as `acme/lib`, or the absolute path of a directory inside the workspace. Omitted: the project the server was started in.",
			},
			"direction": map[string]any{
				"type":        "string",
				"enum":        []string{"uses", "used-by", "both"},
				"description": "Which side to return.",
			},
		},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Project   string `json:"project"`
			Direction string `json:"direction"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		b, err := s.briefing(ctx, args.Project, args.Direction, false)
		if err != nil {
			return nil, err
		}
		view := relatedView{
			Subject:     relatedSubject{Kind: b.Subject.Kind, Project: b.Subject.Project, Group: b.Subject.Group},
			Uses:        []relatedEntry{},
			UsedBy:      []relatedEntry{},
			UsesTotal:   b.UsesTotal,
			UsedByTotal: b.UsedByTotal,
			Within:      b.Within,
			Unresolved:  b.Unresolved,
			Coverage:    b.Coverage,
		}
		for _, r := range b.Uses {
			view.Uses = append(view.Uses, relatedEntry{Project: r.Project, Present: r.Present, Path: r.Path, Links: r.Links})
		}
		for _, r := range b.UsedBy {
			view.UsedBy = append(view.UsedBy, relatedEntry{Project: r.Project, Present: r.Present, Path: r.Path, Links: r.Links})
		}
		return structured(view)
	},
}

// loadIndex returns the workspace's index, read from the file gitty keeps.
// The decoded index is kept between calls and read again when the file has
// changed. When there is no index yet, gitty is asked to build one.
func (s *server) loadIndex(ctx context.Context) (*contract.Index, error) {
	path := filepath.Join(s.workspace, ".gitty", "index.json")
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		_, code, diag, runErr := s.gitty(ctx, "index")
		if runErr != nil {
			return nil, runErr
		}
		if code != 0 {
			return nil, gittyFailure("building the workspace index (gitty index)", code, diag)
		}
		fi, err = os.Stat(path)
	}
	if err != nil {
		return nil, fmt.Errorf("the workspace index cannot be read: %v", err)
	}

	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.idx != nil && fi.ModTime().Equal(s.idxModTime) && fi.Size() == s.idxSize {
		return s.idx, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the workspace index cannot be read: %v", err)
	}
	var idx contract.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("the workspace index is not readable (%v); 'gitty index' rebuilds it", err)
	}
	if err := contract.Supported("the workspace index", idx.Version, contract.IndexVersion); err != nil {
		return nil, err
	}
	s.idx, s.idxModTime, s.idxSize = &idx, fi.ModTime(), fi.Size()
	return s.idx, nil
}

// checkout is one project that is checked out, as the tools address it.
type checkout struct {
	project string
	dir     string // absolute
}

// checkouts lists the checked-out projects of the index, by name.
func (s *server) checkouts(idx *contract.Index) []checkout {
	var out []checkout
	for p, meta := range idx.Projects {
		if meta.Local != nil {
			out = append(out, checkout{project: p, dir: filepath.Join(s.workspace, filepath.FromSlash(meta.Local.Dir))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].project < out[j].project })
	return out
}

// findCheckout returns the checkout of a named project.
func (s *server) findCheckout(idx *contract.Index, project string) (checkout, error) {
	name := strings.Trim(strings.TrimSpace(project), "/")
	if name == "" {
		return checkout{}, fmt.Errorf("a project is required: its namespace path, such as group/project")
	}
	for p, meta := range idx.Projects {
		if !strings.EqualFold(p, name) {
			continue
		}
		if meta.Local == nil {
			hint := "gitty_fetch brings it here"
			if s.noFetch {
				hint = "this server is read-only and cannot fetch it"
			}
			return checkout{}, fmt.Errorf("%s is not checked out; %s", p, hint)
		}
		return checkout{project: p, dir: filepath.Join(s.workspace, filepath.FromSlash(meta.Local.Dir))}, nil
	}
	return checkout{}, fmt.Errorf("no project %q in this workspace's index; gitty_projects lists what there is", project)
}

type projectEntry struct {
	Project     string   `json:"project"`
	Description string   `json:"description,omitempty"`
	Topics      []string `json:"topics,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
	Present     bool     `json:"present"`
	Path        string   `json:"path,omitempty"`
}

var toolProjects = &tool{
	name:  "gitty_projects",
	title: "Find projects",
	description: "Find projects of the instance by name: every project in the workspace's part of the namespace is known, whether or not " +
		"it is checked out. Returns each match with its description and, when it is checked out, the absolute `path` of its checkout. " +
		"Use it to answer \"is there a project for X, and where is it\".",
	readOnly: true,
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Words to look for in the project's path, description and topics; all of them must match, in any order, regardless of case. Omitted: every project.",
			},
			"checked_out": map[string]any{
				"type":        "boolean",
				"description": "Only projects that are checked out on this machine.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "How many to return at most (default 50, at most 500). `total` says how many matched.",
			},
		},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Query      string `json:"query"`
			CheckedOut bool   `json:"checked_out"`
			Limit      int    `json:"limit"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		limit := clamp(args.Limit, 50, 1, 500)
		idx, err := s.loadIndex(ctx)
		if err != nil {
			return nil, err
		}
		words := strings.Fields(strings.ToLower(args.Query))
		var names []string
		for p, meta := range idx.Projects {
			if args.CheckedOut && meta.Local == nil {
				continue
			}
			haystack := strings.ToLower(p + "\n" + meta.Description + "\n" + strings.Join(meta.Topics, " "))
			match := true
			for _, w := range words {
				if !strings.Contains(haystack, w) {
					match = false
					break
				}
			}
			if match {
				names = append(names, p)
			}
		}
		// Checked-out projects first: they are the ones that can be opened.
		sort.Slice(names, func(i, j int) bool {
			a, b := idx.Projects[names[i]].Local != nil, idx.Projects[names[j]].Local != nil
			if a != b {
				return a
			}
			return names[i] < names[j]
		})
		result := struct {
			Total     int            `json:"total"`
			Truncated bool           `json:"truncated,omitempty"`
			Projects  []projectEntry `json:"projects"`
		}{Total: len(names), Projects: []projectEntry{}}
		for _, p := range names {
			if len(result.Projects) == limit {
				result.Truncated = true
				break
			}
			meta := idx.Projects[p]
			e := projectEntry{Project: p, Description: meta.Description, Topics: meta.Topics, Archived: meta.Archived}
			if meta.Local != nil {
				e.Present = true
				e.Path = filepath.Join(s.workspace, filepath.FromSlash(meta.Local.Dir))
			}
			result.Projects = append(result.Projects, e)
		}
		return structured(result)
	},
}

// --- workspace-backed tools ---

var toolFetch = &tool{
	name:  "gitty_fetch",
	title: "Fetch projects",
	description: "Make sure the named projects are checked out on this machine, cloning the ones that are not into the workspace, each at " +
		"the place its namespace path gives it. Returns the absolute `path` of each. Only the projects named are touched; one that is " +
		"already checked out is left exactly as it is. gitty_context does this by itself for what a project uses — call this for " +
		"anything else you need to read, such as a project found with gitty_projects.",
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"projects": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    25,
				"description": "Full namespace paths, such as `acme/platform/lib`.",
			},
		},
		"required":             []string{"projects"},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Projects []string `json:"projects"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		if s.noFetch {
			return nil, fmt.Errorf("this server is read-only (--no-fetch) and does not clone")
		}
		if len(args.Projects) == 0 || len(args.Projects) > 25 {
			return nil, fmt.Errorf("name between 1 and 25 projects, each by its full namespace path")
		}
		cmd := []string{"fetch", "--format=json"}
		if s.jobs > 0 {
			cmd = append(cmd, fmt.Sprintf("--jobs=%d", s.jobs))
		}
		cmd = append(cmd, "--")
		for _, p := range args.Projects {
			if err := checkProjectArg(p); err != nil || p == "" {
				return nil, fmt.Errorf("invalid project %q: give a full namespace path such as group/project", p)
			}
			cmd = append(cmd, p)
		}
		s.fetchMu.Lock()
		defer s.fetchMu.Unlock()
		out, code, diag, err := s.gitty(ctx, cmd...)
		if err != nil {
			return nil, err
		}
		if code != 0 && code != 1 || len(bytes.TrimSpace(out)) == 0 {
			return nil, gittyFailure("gitty fetch", code, diag)
		}
		var report contract.FetchReport
		if err := json.Unmarshal(out, &report); err != nil {
			return nil, fmt.Errorf("gitty fetch did not print a report (%v)", err)
		}
		if err := contract.Supported("the fetch report", report.Schema, contract.Schema); err != nil {
			return nil, err
		}
		return structured(report)
	},
}

// Limits on what one search or read returns. Both exist so that an agent
// gets an answer sized for a conversation; neither is a security boundary.
const (
	defaultSearchMatches = 100
	maxSearchMatches     = 1000
	maxMatchRunes        = 300
	defaultReadLines     = 400
	maxReadLines         = 2000
	maxReadFileBytes     = 4 << 20
)

type searchMatch struct {
	Project string `json:"project"`
	File    string `json:"file"` // relative to the project's checkout
	Line    int    `json:"line"`
	Text    string `json:"text"`
}

var toolSearch = &tool{
	name:  "gitty_search",
	title: "Search checkouts",
	description: "Search the tracked files of the checked-out projects for a pattern (git grep). Returns project, file, line and the " +
		"matching text. Use it to find where something defined in one project is used in the others — every caller of a function you " +
		"are about to change, every pipeline that includes a template. Searches every checkout unless `projects` narrows it; only " +
		"projects that are checked out can be searched.",
	readOnly: true,
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "What to look for: an extended regular expression, or a literal string with `fixed`.",
			},
			"fixed": map[string]any{
				"type":        "boolean",
				"description": "Treat the pattern as a literal string.",
			},
			"ignore_case": map[string]any{"type": "boolean", "description": "Match regardless of case."},
			"projects": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Namespace paths of the projects to search. Omitted: every checked-out project.",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Only files matching this pattern, such as `*.go` or `deploy/*.yml`.",
			},
			"max": map[string]any{
				"type":        "integer",
				"description": "How many matches to return at most (default 100, at most 1000).",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Pattern    string   `json:"pattern"`
			Fixed      bool     `json:"fixed"`
			IgnoreCase bool     `json:"ignore_case"`
			Projects   []string `json:"projects"`
			Glob       string   `json:"glob"`
			Max        int      `json:"max"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		if args.Pattern == "" || strings.ContainsAny(args.Pattern, "\x00\n") {
			return nil, fmt.Errorf("a pattern is required, on one line")
		}
		if strings.ContainsRune(args.Glob, 0) {
			return nil, fmt.Errorf("invalid glob")
		}
		limit := clamp(args.Max, defaultSearchMatches, 1, maxSearchMatches)
		idx, err := s.loadIndex(ctx)
		if err != nil {
			return nil, err
		}
		var where []checkout
		if len(args.Projects) == 0 {
			where = s.checkouts(idx)
		} else {
			for _, p := range args.Projects {
				c, err := s.findCheckout(idx, p)
				if err != nil {
					return nil, err
				}
				where = append(where, c)
			}
		}

		result := struct {
			Matches   []searchMatch `json:"matches"`
			Searched  int           `json:"searched"` // checkouts looked in
			Truncated bool          `json:"truncated,omitempty"`
		}{Matches: []searchMatch{}}
		for _, c := range where {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("cancelled")
			}
			matches, more, err := grep(ctx, c, args.Pattern, args.Fixed, args.IgnoreCase, args.Glob, limit-len(result.Matches))
			if err != nil {
				return nil, err
			}
			result.Searched++
			result.Matches = append(result.Matches, matches...)
			if more || len(result.Matches) >= limit {
				result.Truncated = more || c != where[len(where)-1]
				break
			}
		}
		return structured(result)
	},
}

// grep searches one checkout's tracked files and returns up to limit
// matches, and whether there were more.
func grep(ctx context.Context, c checkout, pattern string, fixed, ignoreCase bool, glob string, limit int) ([]searchMatch, bool, error) {
	// -I: no binary files. -z: a NUL ends the file name, so a name holding a
	// colon cannot be mistaken for the separator. The pattern follows -e, so
	// one that starts with a dash is still a pattern.
	args := []string{"grep", "-n", "-I", "-z", "--no-color", "--full-name"}
	if fixed {
		args = append(args, "-F")
	} else {
		args = append(args, "-E")
	}
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern)
	if glob != "" {
		args = append(args, "--", glob)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = c.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("running git: %v", err)
	}

	var matches []searchMatch
	more := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		// file NUL line NUL text
		parts := strings.SplitN(sc.Text(), "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		if len(matches) == limit {
			more = true
			break
		}
		line := 0
		fmt.Sscanf(parts[1], "%d", &line)
		matches = append(matches, searchMatch{Project: c.project, File: parts[0], Line: line, Text: clip(parts[2], maxMatchRunes)})
	}
	if more {
		cancel() // enough: stop git rather than read the rest
		cmd.Wait()
		return matches, true, nil
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return matches, false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return matches, false, nil // git grep's way of saying "nothing found"
	}
	said := strings.TrimSpace(stderr.String())
	if said == "" {
		said = err.Error()
	}
	return nil, false, fmt.Errorf("searching %s failed: %s", c.project, said)
}

var toolRead = &tool{
	name:  "gitty_read",
	title: "Read a file of a project",
	description: "Read a tracked file from a checked-out project, whole or by line range. Use it when you cannot open a related " +
		"project's path yourself. Only files git tracks in that checkout can be read: nothing outside the checkout, nothing " +
		"untracked, nothing under .git.",
	readOnly: true,
	schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "description": "The project's namespace path, such as `acme/lib`."},
			"path":    map[string]any{"type": "string", "description": "The file, relative to the project's root, such as `pkg/types/user.go`."},
			"start":   map[string]any{"type": "integer", "description": "First line to return, counting from 1 (default 1)."},
			"end":     map[string]any{"type": "integer", "description": "Last line to return (default: 400 lines from `start`; at most 2000 per call)."},
		},
		"required":             []string{"project", "path"},
		"additionalProperties": false,
	},
	run: func(ctx context.Context, s *server, raw json.RawMessage) (*toolResult, error) {
		var args struct {
			Project string `json:"project"`
			Path    string `json:"path"`
			Start   int    `json:"start"`
			End     int    `json:"end"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		idx, err := s.loadIndex(ctx)
		if err != nil {
			return nil, err
		}
		c, err := s.findCheckout(idx, args.Project)
		if err != nil {
			return nil, err
		}
		file, rel, err := confine(ctx, c, args.Path)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(file)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s in %s is not a file", rel, c.project)
		}
		if fi.Size() > maxReadFileBytes {
			return nil, fmt.Errorf("%s in %s is %d bytes, more than this tool reads (%d)", rel, c.project, fi.Size(), maxReadFileBytes)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading %s in %s: %v", rel, c.project, err)
		}
		head := data
		if len(head) > 8000 {
			head = head[:8000]
		}
		if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(head[:validPrefix(head)]) {
			return nil, fmt.Errorf("%s in %s is not a text file", rel, c.project)
		}

		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		total := len(lines)
		if len(data) == 0 {
			total = 0
		}
		start := args.Start
		if start < 1 {
			start = 1
		}
		end := args.End
		if end < start {
			end = start + defaultReadLines - 1
		}
		if end-start+1 > maxReadLines {
			end = start + maxReadLines - 1
		}
		if end > total {
			end = total
		}
		if start > total && total > 0 {
			return nil, fmt.Errorf("%s in %s has %d lines; line %d is past its end", rel, c.project, total, start)
		}
		var sb strings.Builder
		switch {
		case total == 0:
			fmt.Fprintf(&sb, "%s:%s is empty\n", c.project, rel)
		case start == 1 && end == total:
			fmt.Fprintf(&sb, "%s:%s (%d lines)\n", c.project, rel, total)
		default:
			fmt.Fprintf(&sb, "%s:%s lines %d-%d of %d\n", c.project, rel, start, end, total)
		}
		if total > 0 {
			sb.WriteString(strings.Join(lines[start-1:end], "\n"))
			sb.WriteString("\n")
		}
		return textResult(sb.String()), nil
	},
}

// confine turns a path an agent asked for into the file it may read, or
// refuses. The rule is: a file git tracks in this checkout, reached without
// leaving it.
//
//   - The path is relative, and no part of it is "..".
//   - It is not under .git: that is the repository's machinery, and its
//     config can hold credentials.
//   - git tracks it. That keeps untracked files — a .env, a key someone left
//     lying in the directory — out of reach, and settles "is it a file of
//     this project" by asking the one authority on the matter.
//   - Resolved through any symlinks, it is still inside the checkout: a
//     tracked link pointing at /etc/passwd is tracked, and is not read.
func confine(ctx context.Context, c checkout, asked string) (file, rel string, err error) {
	refuse := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("cannot read %q from %s: %s", asked, c.project, why)
	}
	if asked == "" || strings.ContainsRune(asked, 0) {
		return refuse("a path relative to the project's root is required")
	}
	slashed := strings.ReplaceAll(asked, `\`, "/")
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(asked) || filepath.VolumeName(asked) != "" {
		return refuse("the path must be relative to the project's root")
	}
	var parts []string
	for _, seg := range strings.Split(slashed, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return refuse("the path may not contain \"..\"")
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return refuse("a file is required, not the project's root")
	}
	if strings.EqualFold(parts[0], ".git") {
		return refuse("nothing under .git can be read")
	}
	rel = strings.Join(parts, "/")

	// Ask git, taking the path literally: no pathspec magic, no globbing.
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--error-unmatch", "--", rel)
	cmd.Dir = c.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LITERAL_PATHSPECS=1")
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != rel {
		return refuse("it is not a file git tracks in this project")
	}

	root, err := filepath.EvalSymlinks(c.dir)
	if err != nil {
		return refuse("the checkout cannot be resolved")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(c.dir, filepath.FromSlash(rel)))
	if err != nil {
		return refuse("it cannot be resolved")
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return refuse("it leads outside the project")
	}
	return resolved, rel, nil
}

// validPrefix returns the length of b without a trailing partial rune, so a
// sample cut in the middle of a character is not mistaken for bad text.
func validPrefix(b []byte) int {
	for i := 0; i < utf8.UTFMax && i < len(b); i++ {
		if utf8.RuneStart(b[len(b)-1-i]) {
			if !utf8.FullRune(b[len(b)-1-i:]) {
				return len(b) - 1 - i
			}
			break
		}
	}
	return len(b)
}

// clip shortens a line of text to max runes.
func clip(s string, max int) string {
	s = strings.TrimRight(s, "\r")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// clamp returns v, or def when v is unset, kept within [lo, hi].
func clamp(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

// allTools is every tool, in the order they are listed.
var allTools = []*tool{toolContext, toolRelated, toolProjects, toolFetch, toolSearch, toolRead}

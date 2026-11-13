package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gitty/internal/contract"
)

// Sharing: telling a hub what the checkouts here say.
//
// A hub (gitty-up) is the instance's shared picture of its own projects. It
// reads no repository itself: what it knows of one was sent by a gitty that
// had it checked out. This is that sending. For each checkout the hub does
// not already know at its commit, gitty sends what it read there — what the
// project provides, what it links to and where, the opening of its README —
// so that the next agent to ask "what uses this" gets an answer made of
// what repositories say.
//
// Only a repository's own account is sent: a checkout that is clean and at
// the tip of its default branch as the remote has it. A branch in progress,
// or a file edited and not committed, is nobody's account of the project.

// envHub names a hub when the workspace's config does not.
const envHub = "GITTY_HUB"

// hubClient talks to one hub.
type hubClient struct {
	base  string // no trailing slash
	token string
	http  *http.Client
}

// hubAddress works out which hub a run talks to — the flag, the environment,
// then the workspace's config — and that it is somewhere a token may go.
func hubAddress(flagValue string, cfg *Config) (string, error) {
	raw, from := flagValue, "--hub"
	if raw == "" {
		raw, from = os.Getenv(envHub), envHub
	}
	if raw == "" {
		raw, from = cfg.Hub, "hub in .gitty/config"
	}
	if raw == "" {
		return "", nil
	}
	return cleanHubURL(raw, from)
}

func cleanHubURL(raw, from string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", usageErrf("%s is %q, which is not the address of a hub: expected something like https://gitty.example.com", from, raw)
	}
	return u.String(), nil
}

// newHubClient prepares to talk to a hub as the holder of token. The token
// is the user's own GitLab token — the hub asks the instance whose it is —
// so it is not sent anywhere it could be read on the way.
func newHubClient(base, token string) (*hubClient, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if token != "" && u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, usageErrf("the hub at %s is plain http: your GitLab token would cross the network unprotected. Use an https address for it", base)
	}
	return &hubClient{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// do sends one request and decodes the answer.
func (c *hubClient) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gitty/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not reach the hub at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading the hub's answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var said struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &said)
		if said.Error == "" {
			said.Error = http.StatusText(resp.StatusCode)
		}
		return &hubRefusal{status: resp.StatusCode, msg: said.Error, base: c.base}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("the answer from %s%s is not what a hub sends: is that address a gitty-up?", c.base, path)
		}
	}
	return nil
}

// hubRefusal is the hub saying no.
type hubRefusal struct {
	status int
	msg    string
	base   string
}

func (e *hubRefusal) Error() string {
	return fmt.Sprintf("the hub at %s answered %d: %s", e.base, e.status, e.msg)
}

// shareStats counts what a share did.
type shareStats struct{ Shared, Skipped, Errors int }

// shareEvent is one line of a share's account of itself.
type shareEvent struct {
	Kind    string // share, skip, error
	Project string
	Detail  string
}

// maxSharedFiles bounds how many files' blob ids are sent for one project.
// A repository with fewer sends them all, so that any file a note cites can
// be checked later; a larger one sends those gitty reads.
const maxSharedFiles = 2500

// share sends the hub what the workspace's checkouts say, and returns what
// became of each, in order of project path. In a dry run the hub is asked
// what it has, and nothing is sent to it.
func (w *workspace) share(ctx context.Context, hub *hubClient, dryRun bool) ([]shareEvent, shareStats, error) {
	var stats shareStats
	var info contract.HubInfo
	if err := hub.do(ctx, http.MethodGet, "/v1/info", nil, &info); err != nil {
		return nil, stats, err
	}
	if err := contract.Supported("the hub's answer", info.Schema, contract.HubSchema); err != nil {
		return nil, stats, fmt.Errorf("%w (the hub is %s %s)", err, info.Name, info.Version)
	}
	if !sameInstance(info.Instance, w.s.cfg.URL) {
		return nil, stats, usageErrf("the hub at %s is for %s, and this workspace is for %s", hub.base, info.Instance, w.s.cfg.URL)
	}

	idx, _, err := w.refreshIndex(ctx, refreshOptions{DryRun: dryRun})
	if err != nil {
		return nil, stats, fmt.Errorf("the workspace index could not be brought up to date: %w", err)
	}

	var events []shareEvent
	var have []contract.HubHead
	byName := map[string]*contract.Project{}
	for name, meta := range idx.Projects {
		switch {
		case meta.Local == nil:
		case meta.ID == 0:
			events = append(events, shareEvent{"skip", name, "no-project-upstream"})
		case meta.Local.Head == "":
			events = append(events, shareEvent{"skip", name, "no-commits"})
		default:
			have = append(have, contract.HubHead{Project: name, ID: meta.ID, Commit: meta.Local.Head})
			byName[name] = meta
		}
	}
	sort.Slice(have, func(i, j int) bool { return have[i].Project < have[j].Project })

	var wanted contract.HubWanted
	if len(have) > 0 {
		if err := hub.do(ctx, http.MethodPost, "/v1/want", contract.HubWant{Schema: contract.HubSchema, Have: have}, &wanted); err != nil {
			return nil, stats, err
		}
	}
	for project, why := range wanted.Skipped {
		events = append(events, shareEvent{"skip", project, eventWord(why)})
	}

	links := map[string][]contract.Link{}
	for _, l := range idx.Links {
		links[l.From] = append(links[l.From], l)
	}
	var mu sync.Mutex
	forEachConcurrent(ctx, w.s.jobs, wanted.Want, func(project string) {
		ev := w.shareOne(ctx, hub, project, byName[project], links[project], dryRun)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if ctx.Err() != nil {
		return nil, stats, errInterrupted
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Project < events[j].Project })
	for _, ev := range events {
		switch ev.Kind {
		case "share":
			stats.Shared++
		case "skip":
			stats.Skipped++
		case "error":
			stats.Errors++
		}
	}
	return events, stats, nil
}

// eventWord makes a reason fit for an event line: one token.
func eventWord(reason string) string {
	return strings.ReplaceAll(strings.TrimSpace(reason), " ", "-")
}

// shareOne sends one project's facts, if its checkout is the project's own
// account of itself.
func (w *workspace) shareOne(ctx context.Context, hub *hubClient, project string, meta *contract.Project, links []contract.Link, dryRun bool) shareEvent {
	skip := func(why string) shareEvent { return shareEvent{"skip", project, why} }
	fail := func(why string, err error) shareEvent {
		w.s.diagf("%s: %v", project, err)
		return shareEvent{"error", project, why}
	}
	if meta == nil {
		return skip("not-here") // the hub asked for something that was not offered
	}
	dir, head := meta.Local.Dir, meta.Local.Head
	git := func(args ...string) (string, error) {
		out, err := w.s.git(ctx, dir, nil, args...)
		return strings.TrimSpace(string(out)), err
	}

	if status, err := git("status", "--porcelain", "--untracked-files=no"); err != nil {
		return fail("git status failed", err)
	} else if status != "" {
		return skip("uncommitted-changes")
	}
	branch := meta.DefaultBranch
	if branch == "" {
		if ref, err := git("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			branch = strings.TrimPrefix(ref, "origin/")
		}
	}
	if branch == "" {
		return skip("no-default-branch")
	}
	if tip, err := git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err != nil || tip != head {
		return skip("not-default-branch-tip")
	}
	stamp, err := git("log", "-1", "--format=%cI", head)
	if err != nil {
		return fail("git log failed", err)
	}
	committed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return fail("git log failed", fmt.Errorf("commit date %q: %w", stamp, err))
	}
	files, err := w.blobIDs(ctx, dir)
	if err != nil {
		return fail("git ls-files failed", err)
	}
	if dryRun {
		return shareEvent{"share", project, shortSHA(head)}
	}

	facts := contract.Facts{
		Schema: contract.HubSchema, Scanner: contract.ScannerVersion,
		Project: project, ID: meta.ID, Commit: head, CommittedAt: committed.UTC(),
		Summary: meta.Local.Summary, Docs: meta.Local.Docs, Provides: meta.Local.Provides,
		Links: links, Files: files,
	}
	var receipt contract.HubReceipt
	if err := hub.do(ctx, http.MethodPut, "/v1/facts", facts, &receipt); err != nil {
		var refusal *hubRefusal
		if errors.As(err, &refusal) {
			return fail(fmt.Sprintf("the hub answered %d", refusal.status), err)
		}
		return fail("the hub could not be reached", err)
	}
	if !receipt.Stored {
		return skip(eventWord(receipt.Reason))
	}
	return shareEvent{"share", project, shortSHA(head)}
}

func shortSHA(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// blobIDs lists a checkout's tracked files with the id of each one's
// contents, abbreviated: what lets a later reader tell whether a file
// someone pointed at is still the file they saw.
func (w *workspace) blobIDs(ctx context.Context, dir string) (map[string]string, error) {
	out, err := w.s.git(ctx, dir, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	entries := strings.Split(string(out), "\x00")
	all := make(map[string]string, len(entries))
	var read []string
	for _, e := range entries {
		// <mode> <object> <stage>\t<path>
		meta, name, ok := strings.Cut(e, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" || len(fields[1]) < 12 {
			continue
		}
		all[name] = fields[1][:12]
		if classifyFile(name) != classNone || !strings.Contains(name, "/") || strings.HasPrefix(name, "templates/") {
			read = append(read, name)
		}
	}
	if len(all) <= maxSharedFiles {
		return all, nil
	}
	sort.Strings(read)
	if len(read) > maxSharedFiles {
		read = read[:maxSharedFiles]
	}
	some := make(map[string]string, len(read))
	for _, name := range read {
		some[name] = all[name]
	}
	return some, nil
}

// shareOptions bundles the share command's flags.
type shareOptions struct {
	Hub     string
	Token   string
	Anon    bool
	DryRun  bool
	Verbose bool
	Jobs    int
}

// runShare is `gitty share`.
//
// Events, one per line on stdout, in order of project path:
//
//	share <project> <commit>           what the checkout says was sent, and kept
//	skip <project> known               the hub already has it at this commit
//	skip <project> not-covered         private, or outside what the hub covers
//	skip <project> uncommitted-changes the checkout has local edits
//	skip <project> not-default-branch-tip
//	skip <project> no-project-upstream a checkout of nothing on the instance
//	error <project> <reason>
//	summary shared=N skipped=N errors=N
//
// With --dry-run the hub is asked what it has and nothing is sent: the same
// lines, with `plan share` for what would be.
func runShare(ctx context.Context, opts shareOptions) error {
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}
	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s := w.s
	s.dryRun, s.verbose, s.jobs = opts.DryRun, opts.Verbose, opts.Jobs

	address, err := hubAddress(opts.Hub, s.cfg)
	if err != nil {
		return err
	}
	if address == "" {
		return usageErrf("this workspace names no hub. Add one to .gitty/config (hub = 'https://gitty.example.com'), or pass --hub")
	}
	if s.cred.token == "" && !opts.Anon {
		return errTokenRequired()
	}
	hub, err := newHubClient(address, s.cred.token)
	if err != nil {
		return err
	}
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}
	s.diagf("Sharing what the checkouts here say with %s", address)

	events, stats, err := w.share(ctx, hub, opts.DryRun)
	if err != nil {
		return err
	}
	for _, ev := range events {
		s.event(ev.Kind, ev.Project, ev.Detail)
	}
	fmt.Fprintf(s.out, "summary shared=%d skipped=%d errors=%d\n", stats.Shared, stats.Skipped, stats.Errors)
	s.recapFailures()
	if stats.Errors > 0 {
		return &syncFailedError{failures: stats.Errors}
	}
	return nil
}

// shareAfter is the sharing that follows a sync or a fetch when the
// workspace names a hub. It is a courtesy on the way out: it says what it
// did in a line on stderr, and nothing that goes wrong in it changes what
// the command it follows reports.
func shareAfter(ctx context.Context, token string, anon, verbose bool, jobs int) {
	if ctx.Err() != nil {
		return
	}
	w, err := openWorkspace(token, anon)
	if err != nil {
		return
	}
	s := w.s
	s.verbose, s.jobs = verbose, jobs
	address, err := hubAddress("", s.cfg)
	if err != nil {
		s.diagf("note: not shared with a hub: %v", err)
		return
	}
	if address == "" || (s.cred.token == "" && !anon) {
		return
	}
	hub, err := newHubClient(address, s.cred.token)
	if err != nil {
		s.diagf("note: not shared with the hub: %v", err)
		return
	}
	events, stats, err := w.share(ctx, hub, false)
	if err != nil {
		if !errors.Is(err, errInterrupted) {
			s.diagf("note: not shared with the hub: %v", err)
		}
		return
	}
	for _, ev := range events {
		if ev.Kind == "error" {
			s.diagf("note: %s was not shared with the hub: %s", ev.Project, ev.Detail)
		}
	}
	if stats.Shared > 0 || stats.Errors > 0 {
		s.diagf("Shared what %d checkouts say with the hub at %s ('gitty share' shows each)", stats.Shared, address)
	}
}

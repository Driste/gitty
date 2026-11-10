package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitty/internal/contract"
)

// testNow is the moment every test happens at.
var testNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return testNow.Add(-time.Duration(n) * 24 * time.Hour) }

// fakeGitLab is as much of GitLab as the hub talks to.
type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	groups   []glGroup
	projects map[bool][]glProject // by archived
	users    map[string]glUser    // by token
	// access is a user's level on a project or group; absent means they can
	// see it and no more. hidden lists what a user cannot see at all.
	access map[string]int // "<username>/project/<id>" or "<username>/group/<id>"
	hidden map[string]bool
	hits   map[string]int // requests by path
	// throttle makes the next n requests answer 429.
	throttle int
	down     bool
	pageSize int
	// noTotals leaves out the page count, as GitLab does for long listings.
	noTotals bool
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{
		t: t, projects: map[bool][]glProject{}, users: map[string]glUser{},
		access: map[string]int{}, hidden: map[string]bool{}, hits: map[string]int{}, pageSize: 100,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

var (
	reGroup       = regexp.MustCompile(`^/api/v4/groups/([^/]+)$`)
	reDescendants = regexp.MustCompile(`^/api/v4/groups/(\d+)/descendant_groups$`)
	reGroupProjs  = regexp.MustCompile(`^/api/v4/groups/(\d+)/projects$`)
	reMember      = regexp.MustCompile(`^/api/v4/groups/(\d+)/members/all/(\d+)$`)
	reProject     = regexp.MustCompile(`^/api/v4/projects/(\d+)$`)
)

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	f.hits[path]++
	if f.down {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	if f.throttle > 0 {
		f.throttle--
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	user, known := f.users[token]
	page := func(n int) (from, to int) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if p < 1 {
			p = 1
		}
		from = (p - 1) * f.pageSize
		to = from + f.pageSize
		if !f.noTotals {
			w.Header().Set("X-Total-Pages", strconv.Itoa((n+f.pageSize-1)/f.pageSize))
		}
		if to >= n {
			to = n
		} else {
			w.Header().Set("X-Next-Page", strconv.Itoa(p+1))
		}
		if from > n {
			from = n
		}
		return from, to
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	under := func(root glGroup, path string) bool { return strings.HasPrefix(path, root.FullPath+"/") }
	groupByID := func(id string) (glGroup, bool) {
		for _, g := range f.groups {
			if strconv.FormatInt(g.ID, 10) == id || strings.EqualFold(g.FullPath, strings.ReplaceAll(id, "%2F", "/")) {
				return g, true
			}
		}
		return glGroup{}, false
	}

	switch {
	case path == "/api/v4/user":
		if !known {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		reply(user)
	case path == "/api/v4/groups":
		var roots []glGroup
		for _, g := range f.groups {
			if !strings.Contains(g.FullPath, "/") {
				roots = append(roots, g)
			}
		}
		from, to := page(len(roots))
		reply(roots[from:to])
	case reDescendants.MatchString(path):
		root, _ := groupByID(reDescendants.FindStringSubmatch(path)[1])
		var out []glGroup
		for _, g := range f.groups {
			if under(root, g.FullPath) {
				out = append(out, g)
			}
		}
		from, to := page(len(out))
		reply(out[from:to])
	case reGroupProjs.MatchString(path):
		root, _ := groupByID(reGroupProjs.FindStringSubmatch(path)[1])
		if r.URL.Query().Get("simple") != "true" || r.URL.Query().Get("with_shared") != "false" {
			f.t.Errorf("projects were not asked for in the trimmed form, own only: %s", r.URL.RawQuery)
		}
		var out []glProject
		for _, p := range f.projects[r.URL.Query().Get("archived") == "true"] {
			if under(root, p.PathWithNamespace) {
				out = append(out, p)
			}
		}
		from, to := page(len(out))
		reply(out[from:to])
	case reMember.MatchString(path):
		m := reMember.FindStringSubmatch(path)
		level, ok := f.access[user.Username+"/group/"+m[1]]
		if !ok {
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
			return
		}
		reply(map[string]any{"access_level": level})
	case reGroup.MatchString(path):
		g, ok := groupByID(reGroup.FindStringSubmatch(path)[1])
		if !ok || f.hidden[user.Username+"/group/"+strconv.FormatInt(g.ID, 10)] {
			http.Error(w, `{"message":"404 Group Not Found"}`, http.StatusNotFound)
			return
		}
		reply(g)
	case reProject.MatchString(path):
		id := reProject.FindStringSubmatch(path)[1]
		if f.hidden[user.Username+"/project/"+id] {
			http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
			return
		}
		perms := map[string]any{"project_access": nil, "group_access": nil}
		if level, ok := f.access[user.Username+"/project/"+id]; ok {
			perms["project_access"] = map[string]any{"access_level": level}
		}
		reply(map[string]any{"id": id, "permissions": perms})
	default:
		f.t.Errorf("the hub asked GitLab for something unexpected: %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeGitLab) hitsFor(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeGitLab) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeGitLab) setThrottle(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.throttle = n
}

// clock is a time a test can move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// A small instance:
//
//	acme/                        Everything Acme builds
//	  platform/                  Shared infrastructure
//	    ci/                      (no description)
//	      build      10  Builds and pushes container images
//	      old-build  11  DEPRECATED: use build                 (active yesterday)
//	      lint       12  EXPERIMENTAL linting
//	      attic      13  archived
//	    deploy       14  (no description) gitops for the shop
//	    policies     15  Security policies
//	  shop/
//	    web          20  The storefront
//	    api          21  Orders API
//	    dusty        22  quiet for two years
//	  secret/                    a private group
//	    plans        30  private
//	  shop/hidden    23  private
func (f *fakeGitLab) seed() {
	f.groups = []glGroup{
		{ID: 1, FullPath: "acme", Description: "Everything Acme builds", Visibility: "internal"},
		{ID: 2, FullPath: "acme/platform", Description: "Shared infrastructure", Visibility: "internal"},
		{ID: 3, FullPath: "acme/platform/ci", Visibility: "internal"},
		{ID: 4, FullPath: "acme/shop", Description: "The shop", Visibility: "public"},
		{ID: 5, FullPath: "acme/secret", Description: "Not for everyone", Visibility: "private"},
	}
	p := func(id int64, path, desc, vis string, quiet int) glProject {
		return glProject{ID: id, PathWithNamespace: path, Description: desc, Visibility: vis,
			LastActivityAt: daysAgo(quiet), DefaultBranch: "main", WebURL: "https://gitlab.example.com/" + path}
	}
	build := p(10, "acme/platform/ci/build", "Builds and pushes container images", "internal", 2)
	build.Topics, build.StarCount = []string{"ci", "containers"}, 7
	f.projects[false] = []glProject{
		build,
		p(11, "acme/platform/ci/old-build", "DEPRECATED: use build", "internal", 1),
		p(12, "acme/platform/ci/lint", "EXPERIMENTAL linting", "internal", 3),
		p(14, "acme/platform/deploy", "", "internal", 5),
		p(15, "acme/platform/policies", "Security policies", "internal", 30),
		p(20, "acme/shop/web", "The storefront", "public", 1),
		p(21, "acme/shop/api", "Orders API", "internal", 4),
		p(22, "acme/shop/dusty", "Something old", "internal", 800),
		p(23, "acme/shop/hidden", "Nobody's business", "private", 1),
		p(30, "acme/secret/plans", "Plans", "private", 1),
	}
	f.projects[true] = []glProject{p(13, "acme/platform/ci/attic", "An earlier attempt", "internal", 400)}
	f.users = map[string]glUser{
		"tok-ana":  {ID: 100, Username: "ana", State: "active"},
		"tok-ben":  {ID: 101, Username: "ben", State: "active"},
		"tok-gone": {ID: 102, Username: "gone", State: "blocked"},
		"tok-ext":  {ID: 103, Username: "ext", State: "active", External: true},
		"tok-hub":  {ID: 1, Username: "hub", State: "active"},
	}
	f.access["ana/project/10"] = levelMaintainer
	f.access["ana/group/3"] = levelMaintainer
	f.access["ben/project/20"] = levelDeveloper
}

// testHub is a hub over a fake instance, listed, with its HTTP front.
type testHub struct {
	*hub
	fake  *fakeGitLab
	web   *httptest.Server
	logs  *syncBuffer
	dir   string
	clock *clock
}

// syncBuffer is a log that a server writes and a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newTestHub(t *testing.T, open bool) *testHub {
	t.Helper()
	f := newFakeGitLab(t)
	f.seed()
	return openTestHub(t, f, t.TempDir(), open)
}

func openTestHub(t *testing.T, f *fakeGitLab, dir string, open bool) *testHub {
	t.Helper()
	logs := &syncBuffer{}
	logf := func(format string, a ...any) {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		fmt.Fprintf(&logs.b, format+"\n", a...)
	}
	gl, err := newGitLab(f.srv.URL, "tok-hub", logf)
	if err != nil {
		t.Fatal(err)
	}
	gl.wait = func(context.Context, time.Duration) error { return nil }
	st, err := openStore(dir, f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.close)
	if err := st.list(context.Background(), gl, nil, testNow); err != nil {
		t.Fatal(err)
	}
	ck := &clock{t: testNow}
	h := &hub{st: st, gl: gl, auth: newAuthenticator(gl, open), quietAfter: 365 * 24 * time.Hour, now: ck.now, logf: logf}
	h.auth.now = ck.now
	web := httptest.NewServer(h.routes(nil))
	t.Cleanup(web.Close)
	return &testHub{hub: h, fake: f, web: web, logs: logs, dir: dir, clock: ck}
}

// facts builds one project's facts the way gitty would send them.
func facts(id int64, path, commit string, day int, links ...contract.Link) *contract.Facts {
	for i := range links {
		links[i].From = path
	}
	return &contract.Facts{Schema: contract.HubSchema, Scanner: contract.ScannerVersion, Project: path, ID: id,
		Commit: commit, CommittedAt: daysAgo(day), Links: links}
}

func link(to, kind, evidence, target string) contract.Link {
	return contract.Link{To: to, Kind: kind, Evidence: evidence, Target: target}
}

func (h *testHub) mustPut(t *testing.T, f *contract.Facts) contract.HubReceipt {
	t.Helper()
	r, err := h.st.putFacts(f)
	if err != nil {
		t.Fatalf("putFacts(%s): %v", f.Project, err)
	}
	return r
}

// --- the listing ---

func TestListingKeepsOnlyWhatIsOpen(t *testing.T) {
	// Several pages of everything, read both ways: side by side when the
	// instance says how many pages there are, one after another when not.
	for _, noTotals := range []bool{false, true} {
		f := newFakeGitLab(t)
		f.seed()
		f.pageSize, f.noTotals = 2, noTotals
		h := openTestHub(t, f, t.TempDir(), true)
		var paths []string
		for _, p := range h.st.projects {
			paths = append(paths, p.Path)
		}
		if len(paths) != 9 || len(h.st.groups) != 4 {
			t.Errorf("paged listing (totals hidden: %t): %d projects in %d groups, want 9 in 4", noTotals, len(paths), len(h.st.groups))
		}
	}

	f := newFakeGitLab(t)
	f.seed()
	f.pageSize = 3
	h := openTestHub(t, f, t.TempDir(), true)
	s := h.st
	if len(s.projects) != 9 || len(s.groups) != 4 {
		t.Fatalf("listed %d projects in %d groups, want 9 in 4 (nothing private)", len(s.projects), len(s.groups))
	}
	for _, private := range []string{"acme/shop/hidden", "acme/secret/plans"} {
		if s.project(private) != nil {
			t.Errorf("%s is private and must not be held", private)
		}
	}
	if s.group("acme/secret") != nil {
		t.Error("a private group must not be held")
	}
	attic := s.project("ACME/Platform/CI/attic.git")
	if attic == nil || !attic.Archived {
		t.Errorf("attic = %+v; an archived project is listed as one, and paths are found whatever their case", attic)
	}
	if got := s.beneath["acme"]; got != 9 {
		t.Errorf("projects beneath acme = %d", got)
	}
	if got := s.beneath["acme/platform"]; got != 6 {
		t.Errorf("projects beneath acme/platform = %d", got)
	}
	if b := s.project("acme/platform/ci/build"); b.Stars != 7 || strings.Join(b.Topics, ",") != "ci,containers" || !b.LastActivity.Equal(daysAgo(2)) {
		t.Errorf("build = %+v", b)
	}

	// It is on disk, and a hub started on it later has it before listing.
	again, err := openStore(h.dir, f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	if len(again.projects) != 9 || again.listedAt.IsZero() {
		t.Errorf("reloaded: %d projects, listed at %v", len(again.projects), again.listedAt)
	}
	// Data is about one instance, and says so when pointed at another.
	if _, err := openStore(h.dir, "https://other.example.com"); err == nil || !strings.Contains(err.Error(), "--data") {
		t.Errorf("opening one instance's data for another: %v", err)
	}
}

func TestListingOfChosenGroups(t *testing.T) {
	f := newFakeGitLab(t)
	f.seed()
	h := openTestHub(t, f, t.TempDir(), true)
	if err := h.st.list(context.Background(), h.gl, []string{"acme/platform"}, testNow); err != nil {
		t.Fatal(err)
	}
	if len(h.st.projects) != 6 || h.st.project("acme/shop/web") != nil || h.st.group("acme/platform/ci") == nil {
		t.Errorf("covering one group: %d projects", len(h.st.projects))
	}
	if len(h.st.roots) != 1 || h.st.roots[0].Path != "acme/platform" {
		t.Errorf("roots = %v; a group whose parent is not covered stands at the top", h.st.roots)
	}
}

func TestThrottleIsWaitedOutLoud(t *testing.T) {
	f := newFakeGitLab(t)
	f.seed()
	h := openTestHub(t, f, t.TempDir(), true)
	var waits []time.Duration
	h.gl.wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	f.setThrottle(2)
	if err := h.st.list(context.Background(), h.gl, nil, testNow); err != nil {
		t.Fatalf("a throttled listing should wait and go on: %v", err)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second {
		t.Errorf("waits = %v, want what the instance asked for, twice", waits)
	}
	if !strings.Contains(h.logs.String(), "throttled by") || !strings.Contains(h.logs.String(), "waiting 7s") {
		t.Errorf("the wait must be said:\n%s", h.logs.String())
	}

	// And not for ever.
	f.setThrottle(1000)
	err := h.st.list(context.Background(), h.gl, nil, testNow)
	if err == nil || statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("an instance that never lets up: %v", err)
	}
	if len(h.st.projects) != 9 {
		t.Error("a failed listing must leave the last good one in place")
	}
}

// --- MCP ---

type rpcReply struct {
	status int
	header http.Header
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
	raw string
}

func (r *rpcReply) text() string {
	if r.Result == nil {
		return ""
	}
	content, _ := r.Result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func (r *rpcReply) isError() bool {
	b, _ := r.Result["isError"].(bool)
	return b
}

// post sends a raw body with the given headers.
func (h *testHub) post(t *testing.T, path, body string, headers map[string]string) *rpcReply {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.web.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := &rpcReply{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	json.Unmarshal(raw, out)
	return out
}

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// modern sends a well-formed request of the current revision.
func (h *testHub) modern(t *testing.T, token, method, params string) *rpcReply {
	t.Helper()
	headers := map[string]string{"MCP-Protocol-Version": protoModern, "Mcp-Method": method}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	if method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		json.Unmarshal([]byte("{"+params+"}"), &p)
		headers["Mcp-Name"] = p.Name
	}
	if params != "" {
		params += ","
	}
	return h.post(t, "/mcp", `{"jsonrpc":"2.0","id":7,"method":"`+method+`","params":{`+params+modernMeta+`}}`, headers)
}

// call runs a tool as a caller and returns what it said.
func (h *testHub) call(t *testing.T, token, tool, args string) *rpcReply {
	t.Helper()
	r := h.modern(t, token, "tools/call", `"name":"`+tool+`","arguments":`+args)
	if r.status != http.StatusOK || r.Result == nil {
		t.Fatalf("%s %s: HTTP %d: %s", tool, args, r.status, r.raw)
	}
	return r
}

func TestMCPModern(t *testing.T) {
	h := newTestHub(t, true)

	d := h.modern(t, "", "server/discover", "")
	if d.status != 200 || d.Result["resultType"] != "complete" || d.Result["cacheScope"] != "public" ||
		fmt.Sprint(d.Result["supportedVersions"]) != "[2026-07-28 2025-11-25 2025-06-18 2025-03-26]" ||
		!strings.Contains(fmt.Sprint(d.Result["instructions"]), "gitty_tree") {
		t.Errorf("server/discover: %d %s", d.status, d.raw)
	}
	if info := d.Result["_meta"].(map[string]any)[metaServerInfo].(map[string]any); info["name"] != "gitty-up" {
		t.Errorf("serverInfo = %v", info)
	}
	if d.header.Get("Mcp-Session-Id") != "" {
		t.Error("no session is ever issued")
	}

	l := h.modern(t, "", "tools/list", "")
	var names []string
	for _, raw := range l.Result["tools"].([]any) {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s has no object schema", tool["name"])
		}
	}
	if got := strings.Join(names, " "); got != "gitty_tree gitty_browse gitty_search gitty_outline gitty_note" {
		t.Errorf("tools = %s", got)
	}
	if l.Result["resultType"] != "complete" || l.Result["ttlMs"] == nil || l.Result["cacheScope"] != "public" {
		t.Errorf("tools/list must be cacheable and typed: %s", l.raw)
	}

	c := h.call(t, "", "gitty_tree", `{}`)
	if c.isError() || !strings.Contains(c.text(), "acme/  9 projects, 2 groups — Everything Acme builds") || c.Result["resultType"] != "complete" {
		t.Errorf("gitty_tree: %s", c.raw)
	}

	// What the revision requires of a request, it is held to.
	body := func(method, params, meta string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + params + meta + `}}`
	}
	good := map[string]string{"MCP-Protocol-Version": protoModern, "Mcp-Method": "tools/list"}
	with := func(over map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range good {
			out[k] = v
		}
		for k, v := range over {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}
	tests := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
		code    int
	}{
		{"no version header", body("tools/list", "", modernMeta), with(map[string]string{"MCP-Protocol-Version": ""}), 400, codeHeaderMismatch},
		{"no version in the body", body("tools/list", "", `"_meta":{}`), good, 400, codeInvalidParams},
		{"header and body disagree", body("tools/list", "", strings.Replace(modernMeta, "2026-07-28", "2027-01-01", 1)), good, 400, codeHeaderMismatch},
		{"a version from the future", body("tools/list", "", strings.Replace(modernMeta, "2026-07-28", "2027-01-01", 1)),
			with(map[string]string{"MCP-Protocol-Version": "2027-01-01"}), 400, codeBadVersion},
		{"a future version in the header alone", body("tools/list", "", ""), with(map[string]string{"MCP-Protocol-Version": "2027-01-01"}), 400, codeBadVersion},
		{"no capabilities", body("tools/list", "", `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`), good, 400, codeInvalidParams},
		{"no method header", body("tools/list", "", modernMeta), with(map[string]string{"Mcp-Method": ""}), 400, codeHeaderMismatch},
		{"method header disagrees", body("tools/list", "", modernMeta), with(map[string]string{"Mcp-Method": "tools/call"}), 400, codeHeaderMismatch},
		{"no name header on a call", body("tools/call", `"name":"gitty_tree","arguments":{},`, modernMeta), with(map[string]string{"Mcp-Method": "tools/call"}), 400, codeHeaderMismatch},
		{"name header disagrees", body("tools/call", `"name":"gitty_tree","arguments":{},`, modernMeta),
			with(map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "gitty_note"}), 400, codeHeaderMismatch},
		{"an unknown tool", body("tools/call", `"name":"nope","arguments":{},`, modernMeta),
			with(map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "nope"}), 400, codeInvalidParams},
		{"a method this server does not have", body("resources/list", "", modernMeta), with(map[string]string{"Mcp-Method": "resources/list"}), 404, codeMethodNotFound},
		{"the old handshake, sent the new way", body("initialize", "", modernMeta), with(map[string]string{"Mcp-Method": "initialize"}), 404, codeMethodNotFound},
	}
	for _, tt := range tests {
		r := h.post(t, "/mcp", tt.body, tt.headers)
		if r.status != tt.status || r.Error == nil || r.Error.Code != tt.code {
			t.Errorf("%s: HTTP %d %s, want %d with code %d", tt.name, r.status, r.raw, tt.status, tt.code)
		}
	}
	bad := h.post(t, "/mcp", body("tools/list", "", strings.Replace(modernMeta, "2026-07-28", "1900-01-01", 1)), with(map[string]string{"MCP-Protocol-Version": "1900-01-01"}))
	if bad.Error == nil || bad.Error.Data["requested"] != "1900-01-01" || !strings.Contains(fmt.Sprint(bad.Error.Data["supported"]), "2026-07-28") {
		t.Errorf("an unsupported version must be answered with the supported ones: %s", bad.raw)
	}

	// A name that plain ASCII cannot carry arrives encoded.
	enc := h.post(t, "/mcp", body("tools/call", `"name":"gitty_tree","arguments":{},`, modernMeta),
		with(map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "=?base64?Z2l0dHlfdHJlZQ==?="}))
	if enc.status != 200 || enc.isError() {
		t.Errorf("an encoded Mcp-Name: %d %s", enc.status, enc.raw)
	}

	// Transport: one message per POST, nothing else.
	if r := h.post(t, "/mcp", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`, good); r.status != http.StatusAccepted || r.raw != "" {
		t.Errorf("a notification: %d %q", r.status, r.raw)
	}
	if r := h.post(t, "/mcp", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, good); r.status != 400 {
		t.Errorf("a batch: %d", r.status)
	}
	if r := h.post(t, "/mcp", `not json`, good); r.status != 400 || r.Error == nil || r.Error.Code != codeParse {
		t.Errorf("not JSON: %d %s", r.status, r.raw)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(method, h.web.URL+"/mcp", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp = %d, want 405: there is no stream and no session", method, resp.StatusCode)
		}
	}
}

func TestMCPLegacyHandshake(t *testing.T) {
	h := newTestHub(t, true)
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "2024-11-05": "2025-11-25", "": "2025-11-25"} {
		r := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+asked+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
		if r.status != 200 || r.Result["protocolVersion"] != want || r.Result["serverInfo"] == nil || r.Result["instructions"] == nil {
			t.Errorf("initialize %q: %d %s, want version %s", asked, r.status, r.raw, want)
		}
		if _, typed := r.Result["resultType"]; typed || r.header.Get("Mcp-Session-Id") != "" {
			t.Errorf("an older client is answered in its own terms, with no session: %s", r.raw)
		}
	}
	old := map[string]string{"MCP-Protocol-Version": "2025-06-18"}
	if r := h.post(t, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, old); r.status != http.StatusAccepted {
		t.Errorf("initialized: %d", r.status)
	}
	if r := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, old); r.status != 200 || r.Result == nil {
		t.Errorf("ping: %d %s", r.status, r.raw)
	}
	l := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, old)
	if l.status != 200 || len(l.Result["tools"].([]any)) != 5 || l.Result["ttlMs"] != nil {
		t.Errorf("tools/list: %d %s", l.status, l.raw)
	}
	// No version header at all is the oldest of them.
	c := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"gitty_browse","arguments":{"path":"acme/shop"}}}`, nil)
	if c.status != 200 || c.isError() || !strings.Contains(c.text(), "- web — The storefront") {
		t.Errorf("tools/call: %d %s", c.status, c.raw)
	}
	// In that era an unknown method is an answer, not an HTTP error.
	if r := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`, old); r.status != 200 || r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Errorf("unknown method: %d %s", r.status, r.raw)
	}
	if r := h.post(t, "/mcp", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, old); r.status != 200 || r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool: %d %s", r.status, r.raw)
	}
}

func TestOriginIsChecked(t *testing.T) {
	h := newTestHub(t, true)
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if r := h.post(t, "/mcp", body, map[string]string{"Origin": "https://evil.example"}); r.status != http.StatusForbidden {
		t.Errorf("a page on another site: %d", r.status)
	}
	if r := h.post(t, "/mcp", body, map[string]string{"Origin": h.web.URL}); r.status != 200 {
		t.Errorf("the hub's own origin: %d %s", r.status, r.raw)
	}
	allowed := httptest.NewServer(h.routes([]string{"https://console.example"}))
	defer allowed.Close()
	req, _ := http.NewRequest(http.MethodPost, allowed.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Origin", "https://console.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("an allowed origin: %d", resp.StatusCode)
	}
}

// --- who is asking ---

func TestCallersAreAskedWhoTheyAre(t *testing.T) {
	h := newTestHub(t, false)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	as := func(token string) *rpcReply {
		headers := map[string]string{}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		return h.post(t, "/mcp", ping, headers)
	}
	if r := as(""); r.status != 401 || !strings.Contains(r.header.Get("WWW-Authenticate"), "Bearer") || !strings.Contains(r.raw, "Authorization: Bearer") {
		t.Errorf("no token: %d %s", r.status, r.raw)
	}
	for i := 0; i < 3; i++ {
		if r := as("nonsense"); r.status != 401 {
			t.Errorf("a token the instance does not know: %d", r.status)
		}
		if r := as("tok-ana"); r.status != 200 {
			t.Errorf("a token the instance knows: %d %s", r.status, r.raw)
		}
	}
	if r := as("tok-gone"); r.status != 401 {
		t.Errorf("a blocked user: %d", r.status)
	}
	// Four tokens were asked about once each: the instance is not asked
	// again for every call.
	if got := h.fake.hitsFor("/api/v4/user"); got != 3 {
		t.Errorf("GitLab was asked who a caller is %d times, want 3 (ana, nonsense, gone)", got)
	}
	// GitLab's other header works too.
	if r := h.post(t, "/mcp", ping, map[string]string{"PRIVATE-TOKEN": "tok-ben"}); r.status != 200 {
		t.Errorf("PRIVATE-TOKEN: %d", r.status)
	}
	// An instance that cannot be asked lets nobody new in, and says why.
	h.fake.setDown(true)
	if r := h.post(t, "/mcp", ping, map[string]string{"Authorization": "Bearer tok-ext"}); r.status != http.StatusServiceUnavailable {
		t.Errorf("with the instance down: %d", r.status)
	}
	if r := as("tok-ana"); r.status != 200 {
		t.Errorf("someone already known is still known for a while: %d", r.status)
	}
	h.fake.setDown(false)
	// The answer is forgotten after a while, and asked for again.
	h.clock.set(testNow.Add(callerTTL + time.Second))
	before := h.fake.hitsFor("/api/v4/user")
	as("tok-ana")
	if h.fake.hitsFor("/api/v4/user") != before+1 {
		t.Error("a caller must be asked about again once the answer is old")
	}
	// Nothing anywhere holds a token.
	h.st.close()
	filepath.Walk(h.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if data, _ := os.ReadFile(path); bytes.Contains(data, []byte("tok-")) {
				t.Errorf("%s holds a token", path)
			}
		}
		return nil
	})
	if strings.Contains(h.logs.String(), "tok-") {
		t.Errorf("the log holds a token:\n%s", h.logs.String())
	}
}

// --- facts ---

func TestFacts(t *testing.T) {
	h := newTestHub(t, true)
	web := facts(20, "acme/shop/web", "aaa111", 3,
		link("acme/platform/ci/build", contract.KindCIComponent, ".gitlab-ci.yml:4", "build"),
		link("ACME/shop/API", contract.KindDepends, "package.json:9", ""),     // found whatever its case
		link("acme/shop/web", contract.KindDepends, "package.json:10", ""),    // itself
		link("acme/shop/hidden", contract.KindDepends, "package.json:11", ""), // private: not covered
		link("elsewhere/thing", contract.KindDepends, "package.json:12", ""),
		link("acme/shop/api", "invented-kind", "x:1", ""),
	)
	web.Summary = "The  storefront.\nBuilt with care."
	web.Provides = []contract.Provided{{Kind: contract.ProvidesImage, Name: "image", File: "Dockerfile"}, {Kind: "made-up", Name: "x"}}
	web.Files = map[string]string{".gitlab-ci.yml": "b1", "../escape": "b2", "package.json": "b3"}
	r := h.mustPut(t, web)
	if !r.Stored || r.Links != 2 {
		t.Fatalf("receipt = %+v, want stored with the 2 links that end at a covered project", r)
	}
	got := h.st.facts[20]
	if got.Summary != "The storefront. Built with care." || len(got.Provides) != 1 || len(got.Files) != 2 ||
		got.Links[1].To != "acme/shop/api" || got.Links[0].From != "acme/shop/web" {
		t.Errorf("stored facts = %+v", got)
	}
	if us := h.st.usedBy[10]; len(us) != 1 || us[0].From.Path != "acme/shop/web" || us[0].Target != "build" {
		t.Errorf("build is used by %+v", us)
	}

	// Newer replaces; older and the same do not.
	if r := h.mustPut(t, facts(20, "acme/shop/web", "aaa111", 3)); r.Stored || r.Reason != "already known at this commit" {
		t.Errorf("the same commit again: %+v", r)
	}
	if r := h.mustPut(t, facts(20, "acme/shop/web", "old000", 9)); r.Stored || !strings.Contains(r.Reason, "newer") {
		t.Errorf("an older commit: %+v", r)
	}
	if r := h.mustPut(t, facts(20, "acme/shop/web", "bbb222", 1, link("acme/shop/api", contract.KindDepends, "package.json:9", ""))); !r.Stored {
		t.Errorf("a newer commit: %+v", r)
	}
	if len(h.st.usedBy[10]) != 0 || len(h.st.usedBy[21]) != 1 {
		t.Error("links that are gone from the repository must be gone from the hub")
	}

	// What is refused, and why.
	refusals := map[string]*contract.Facts{
		"not a project this hub covers": facts(23, "acme/shop/hidden", "c", 1),
		"is acme/shop/api here":         facts(21, "acme/shop/renamed", "c", 1),
		"different releases":            {Schema: 99, Project: "acme/shop/api", ID: 21, Commit: "c", CommittedAt: daysAgo(1)},
		"which commit":                  {Schema: contract.HubSchema, Project: "acme/shop/api", ID: 21},
	}
	for want, f := range refusals {
		if _, err := h.st.putFacts(f); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("facts for %s: %v, want a refusal saying %q", f.Project, err, want)
		}
	}

	// What gitty asks before sending.
	w := h.st.wants([]contract.HubHead{
		{Project: "acme/shop/web", ID: 20, Commit: "bbb222"},
		{Project: "acme/shop/web", ID: 20, Commit: "ccc333"},
		{Project: "acme/shop/api", ID: 21, Commit: "x"},
		{Project: "acme/shop/hidden", ID: 23, Commit: "x"},
		{Project: "acme/shop/moved", ID: 22, Commit: "x"},
	})
	if strings.Join(w.Want, " ") != "acme/shop/web acme/shop/api" || w.Skipped["acme/shop/hidden"] != "not covered" ||
		w.Skipped["acme/shop/moved"] != "known here as acme/shop/dusty" {
		t.Errorf("wants = %+v", w)
	}

	// It is on disk, by id, and comes back.
	again, err := openStore(h.dir, h.fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	if f := again.facts[20]; f == nil || f.Commit != "bbb222" || len(again.usedBy[21]) != 1 {
		t.Errorf("reloaded facts = %+v", f)
	}
}

func TestFactsOverHTTP(t *testing.T) {
	h := newTestHub(t, false)
	send := func(token, method, path string, body any) (int, map[string]any) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, h.web.URL+path, bytes.NewReader(data))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := send("", http.MethodGet, "/v1/info", nil); code != 401 {
		t.Errorf("info without a token: %d", code)
	}
	code, info := send("tok-ana", http.MethodGet, "/v1/info", nil)
	if code != 200 || info["name"] != "gitty-up" || info["projects"] != float64(9) || info["instance"] != h.fake.srv.URL {
		t.Errorf("info: %d %v", code, info)
	}
	code, wanted := send("tok-ana", http.MethodPost, "/v1/want", contract.HubWant{Schema: contract.HubSchema,
		Have: []contract.HubHead{{Project: "acme/shop/web", ID: 20, Commit: "a"}}})
	if code != 200 || fmt.Sprint(wanted["want"]) != "[acme/shop/web]" {
		t.Errorf("want: %d %v", code, wanted)
	}
	if code, out := send("tok-ana", http.MethodPost, "/v1/want", contract.HubWant{Schema: 99}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "different releases") {
		t.Errorf("want from another release: %d %v", code, out)
	}
	code, receipt := send("tok-ana", http.MethodPut, "/v1/facts", facts(20, "acme/shop/web", "a", 1, link("acme/shop/api", contract.KindDepends, "go.mod:3", "")))
	if code != 200 || receipt["stored"] != true || receipt["links"] != float64(1) {
		t.Errorf("facts: %d %v", code, receipt)
	}
	if !strings.Contains(h.logs.String(), "facts: acme/shop/web at a from @ana") {
		t.Errorf("stored facts are logged with who sent them:\n%s", h.logs.String())
	}
	// Refusals are answers, not failures: gitty reports them and goes on.
	code, receipt = send("tok-ana", http.MethodPut, "/v1/facts", facts(23, "acme/shop/hidden", "a", 1))
	if code != 200 || receipt["stored"] != false || receipt["reason"] != "not a project this hub covers" {
		t.Errorf("facts for a project not covered: %d %v", code, receipt)
	}
	// An external user speaks for public projects only.
	code, receipt = send("tok-ext", http.MethodPut, "/v1/facts", facts(21, "acme/shop/api", "a", 1))
	if code != 200 || receipt["stored"] != false {
		t.Errorf("an external user and an internal project: %d %v", code, receipt)
	}
	if code, _ := send("tok-ana", http.MethodPost, "/v1/facts", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/facts: %d", code)
	}
}

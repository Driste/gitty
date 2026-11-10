package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitty/internal/contract"
)

// stocked is a hub that has been told things: three repositories read, and
// what they say of each other.
func stocked(t *testing.T, open bool) *testHub {
	t.Helper()
	h := newTestHub(t, open)
	build := facts(10, "acme/platform/ci/build", "b111111111", 2)
	build.Summary = "A CI/CD component that builds an image with Kaniko and pushes it."
	build.Provides = []contract.Provided{
		{Kind: contract.ProvidesCIComponent, Name: "build", File: "templates/build.yml", About: "Builds and pushes a container image.", Inputs: []string{"image_name", "context"}},
		{Kind: contract.ProvidesCIComponent, Name: "scan", File: "templates/scan/template.yml"},
	}
	build.Files = map[string]string{"templates/build.yml": "blob-build-1", "README.md": "blob-readme-1"}
	h.mustPut(t, build)
	h.mustPut(t, facts(20, "acme/shop/web", "w222222222", 1,
		link("acme/platform/ci/build", contract.KindCIComponent, ".gitlab-ci.yml:4", "build"),
		link("acme/shop/api", contract.KindDepends, "package.json:9", ""),
		link("acme/platform/policies", contract.KindMentions, "README.md:30", "")))
	h.mustPut(t, facts(21, "acme/shop/api", "a333333333", 4,
		link("acme/platform/ci/build", contract.KindCIComponent, ".gitlab-ci.yml:7", "build, scan"),
		link("acme/platform/ci/old-build", contract.KindCIInclude, ".gitlab-ci.yml:2", "templates/old.yml")))
	deploy := facts(14, "acme/platform/deploy", "d444444444", 5,
		link("acme/shop/web", contract.KindDeploys, "envs/prod/values.yaml:3", ""),
		link("acme/shop/api", contract.KindDeploys, "envs/prod/values.yaml:9", ""))
	deploy.Summary = "Helm values for everything the shop runs in production."
	h.mustPut(t, deploy)
	return h
}

func wantText(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s is missing %q:\n%s", what, w, got)
		}
	}
}

func refuseText(t *testing.T, what, got string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(got, n) {
			t.Errorf("%s must not hold %q:\n%s", what, n, got)
		}
	}
}

// --- standing ---

func TestStanding(t *testing.T) {
	h := stocked(t, true)
	s := h.st
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []*node
	for _, p := range s.projects {
		all = append(all, p)
	}
	offer, aside := s.rank(all, testNow, h.quietAfter)
	names := func(rs []ranked) string {
		var out []string
		for _, r := range rs {
			out = append(out, fmt.Sprintf("%s(%d)", r.p.name(), r.st.UsedBy))
		}
		return strings.Join(out, " ")
	}
	// By use, then by how lately anything happened; what says it is
	// unfinished goes below its peers. A mention is not a use.
	if got, want := names(offer), "build(2) api(1) web(0) deploy(0) policies(0) lint(0)"; got != want {
		t.Errorf("offered:\n%s\n--- want:\n%s", got, want)
	}
	if got, want := names(aside), "old-build(1) attic(0) dusty(0)"; got != want {
		t.Errorf("set aside:\n%s\n--- want:\n%s", got, want)
	}
	reasons := map[string]string{}
	for _, r := range aside {
		reasons[r.p.name()] = r.st.Aside
	}
	// Active yesterday, and still deprecated: its own words outrank that.
	if reasons["old-build"] != "deprecated, by its own account" || reasons["attic"] != "archived" || reasons["dusty"] != "no activity for 2 years" {
		t.Errorf("reasons = %v", reasons)
	}
	if st := s.standingOf(s.project("acme/platform/ci/lint"), testNow, h.quietAfter); !st.Experimental || st.Aside != "" {
		t.Errorf("lint = %+v; unfinished is a label, not a reason to set aside", st)
	}
	// With no period set, nothing is set aside for being quiet.
	if st := s.standingOf(s.project("acme/shop/dusty"), testNow, 0); st.Aside != "" {
		t.Errorf("dusty with --quiet-after=0 = %+v", st)
	}
}

// --- the tree ---

func TestTree(t *testing.T) {
	h := stocked(t, true)
	got, err := h.renderTree("", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Groups of ` + h.gl.host + `: 9 projects in 4 groups, listed moments ago
acme/  9 projects, 2 groups — Everything Acme builds
  platform/  6 projects, 1 group — Shared infrastructure
    ci/  4 projects
  shop/  3 projects — The shop
`
	if !strings.HasPrefix(got, want) {
		t.Errorf("tree:\n%s\n--- want it to begin:\n%s", got, want)
	}
	wantText(t, "tree", got, "Next: gitty_browse")

	sub, _ := h.renderTree("acme/platform", 0)
	wantText(t, "subtree", sub, "# Groups beneath acme/platform: 6 projects, 1 subgroup\nci/  4 projects\n")
	shallow, _ := h.renderTree("", 1)
	wantText(t, "depth 1", shallow, "acme/  9 projects, 2 groups", "shown 1 level deep of 3")
	refuseText(t, "depth 1", shallow, "platform/")

	for path, want := range map[string]string{"acme/shop/web": "is a project, not a group", "nope": "There is no group"} {
		if _, err := h.renderTree(path, 0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("tree of %s: %v", path, err)
		}
	}
}

// A large instance still fits one answer, and says what it left out.
func TestTreeFitsItsBudget(t *testing.T) {
	h := newTestHub(t, true)
	var groups, projects []*node
	id := int64(1000)
	long := strings.Repeat("What this group is for, at some length. ", 6)
	for a := 0; a < 30; a++ {
		top := fmt.Sprintf("division-%02d", a)
		id++
		groups = append(groups, &node{ID: id, Path: top, Description: long})
		for b := 0; b < 20; b++ {
			mid := fmt.Sprintf("%s/team-%02d", top, b)
			id++
			groups = append(groups, &node{ID: id, Path: mid, Description: long})
			for c := 0; c < 3; c++ {
				id++
				groups = append(groups, &node{ID: id, Path: fmt.Sprintf("%s/area-%d", mid, c), Description: long})
				id++
				projects = append(projects, &node{ID: id, Path: fmt.Sprintf("%s/area-%d/svc", mid, c)})
			}
		}
	}
	if err := h.st.setTree(groups, projects, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := h.renderTree("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > treeBudget+1000 {
		t.Errorf("the tree of %d groups is %d bytes, over the budget of %d", len(groups), len(got), treeBudget)
	}
	wantText(t, "a large tree", got, "division-00/  60 projects, 20 groups", "(cut to 2 levels deep")
	// Every group is still reachable: one level down, the rest is there.
	sub, _ := h.renderTree("division-07", 0)
	wantText(t, "its subtree", sub, "team-19/  3 projects, 3 groups", "\n  area-2/  1 project")
	if strings.Contains(sub, "(cut") {
		t.Errorf("a subtree that fits should not be cut:\n%s", sub[len(sub)-300:])
	}
}

// --- browse, search ---

func TestBrowse(t *testing.T) {
	h := stocked(t, true)
	got, err := h.renderBrowse("acme/platform/ci", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `# acme/platform/ci/
4 projects directly here, 4 beneath, 0 subgroups

## Projects (2), most used and best kept first
- build — Builds and pushes container images · used by 2 · provides ci-component build, scan
- lint — EXPERIMENTAL linting · experimental

## Set aside (2): archived, deprecated or long quiet. Here so they are known, not to build on
- old-build — DEPRECATED: use build · used by 1 · deprecated, by its own account
- attic — An earlier attempt · archived
`
	if !strings.HasPrefix(got, want) {
		t.Errorf("browse:\n%s\n--- want it to begin:\n%s", got, want)
	}

	top, _ := h.renderBrowse("", 0, 0)
	wantText(t, "the top", top, "## Subgroups (1)\n- acme/  9 projects — Everything Acme builds\n")
	platform, _ := h.renderBrowse("acme/platform", 0, 0)
	// A project with no description is described by its README when its
	// repository has been read, and that is said.
	wantText(t, "a group with subgroups", platform,
		"# acme/platform/ — Shared infrastructure", "- ci/  4 projects\n",
		"- deploy — readme: Helm values for everything the shop runs in production.")

	paged, _ := h.renderBrowse("acme/platform/ci", 1, 2)
	wantText(t, "a page", paged, "## Projects (2 to 3 of 4)", "- lint", "## Set aside (2)", "- old-build",
		`(more: gitty_browse {"path": "acme/platform/ci", "offset": 3})`)
	refuseText(t, "a page", paged, "- build —", "- attic")

	if _, err := h.renderBrowse("acme/shop/web", 0, 0); err == nil || !strings.Contains(err.Error(), "gitty_outline") {
		t.Errorf("browsing a project should point at the outline: %v", err)
	}
}

func TestSearch(t *testing.T) {
	h := stocked(t, true)
	got, err := h.renderSearch("Kaniko image", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Words from a README and from what a repository provides are found.
	wantText(t, "search", got, `# "kaniko image": 1 project, 0 groups`, "- acme/platform/ci/build — Builds and pushes container images · used by 2")

	inputs, _ := h.renderSearch("image_name", "", 0)
	wantText(t, "a component's input", inputs, "acme/platform/ci/build")

	all, _ := h.renderSearch("build", "", 0)
	wantText(t, "set aside comes after", all, "## Projects, most used and best kept first\n- acme/platform/ci/build", "## Set aside (1)", "- acme/platform/ci/old-build")
	if strings.Index(all, "old-build") < strings.Index(all, "## Set aside") {
		t.Errorf("a deprecated project was offered among the rest:\n%s", all)
	}

	scoped, _ := h.renderSearch("shop", "acme/platform", 0)
	wantText(t, "beneath a group", scoped, "1 project", "acme/platform/deploy")
	refuseText(t, "beneath a group", scoped, "acme/shop/web")

	groups, _ := h.renderSearch("infrastructure", "", 0)
	wantText(t, "groups too", groups, "## Groups\n- acme/platform/  6 projects — Shared infrastructure")

	none, _ := h.renderSearch("kubernetes operator", "", 0)
	wantText(t, "nothing found", none, "0 projects", "matched literally", "gitty_tree")
	if _, err := h.renderSearch("   ", "", 0); err == nil {
		t.Error("a search for nothing should say what a search needs")
	}
	limited, _ := h.renderSearch("acme", "", 2)
	wantText(t, "a limit", limited, "(7 more match")
}

// --- outline ---

func TestOutlineOfAProject(t *testing.T) {
	h := stocked(t, true)
	got, err := h.renderOutline("acme/platform/ci/build")
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, "outline", got,
		"# acme/platform/ci/build\n",
		"Builds and pushes container images  [its description]",
		"A CI/CD component that builds an image with Kaniko and pushes it.  [its README, at b11111111]",
		"Standing: used by 2 projects · last activity 2 days ago · 7 stars",
		"Topics: ci, containers",
		"## Provides (2)\n- ci-component build — Builds and pushes a container image. · inputs: image_name, context (templates/build.yml)\n- ci-component scan (templates/scan/template.yml)",
		// The most recently active user first: a place to see how it is done.
		"## Used by (2): each is a place to see how\n- acme/shop/web · ci-component build (.gitlab-ci.yml:4)\n- acme/shop/api · ci-component build, scan (.gitlab-ci.yml:7)",
		"- Its repository was read at b11111111, committed 2 days ago.",
		`"Used by" counts the projects whose repositories have been read: 4 of 9 here so far.`,
		"gitty fetch acme/platform/ci/build",
		"gitty_note",
	)

	web, _ := h.renderOutline("acme/shop/web")
	wantText(t, "an application", web,
		"## Uses (2)\n- acme/platform/ci/build · ci-component build (.gitlab-ci.yml:4)\n- acme/shop/api · depends (package.json:9)",
		"## Mentions in its documentation (1)\n- acme/platform/policies · mentions (README.md:30)",
		"## Deployed by (1)\n- acme/platform/deploy · deploys (envs/prod/values.yaml:3)",
	)
	deploy, _ := h.renderOutline("acme/platform/deploy")
	wantText(t, "what deploys", deploy, "## Deploys (2)\n- acme/shop/web · deploys (envs/prod/values.yaml:3)")
	policies, _ := h.renderOutline("acme/platform/policies")
	wantText(t, "a project nothing was read from", policies,
		"Standing: used by 0 projects", "Mentioned in the documentation of 1 other project.",
		"Its repository has not been read", "unknown, not empty")
	old, _ := h.renderOutline("acme/platform/ci/old-build")
	wantText(t, "a deprecated project", old, "Deprecated, by its own account: not something to build on")

	for path, want := range map[string]string{"": "needs `path`", "acme/shop/hidden": "It may be private", "acme/nope/x": `{"query": "x"}`} {
		if _, err := h.renderOutline(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("outline of %q: %v", path, err)
		}
	}
}

func TestOutlineOfAGroup(t *testing.T) {
	h := stocked(t, true)
	got, err := h.renderOutline("acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	// How things are done here, as far as repositories show: what the
	// group's projects lean on, and how many of those read do.
	wantText(t, "a group", got,
		"# acme/shop/ (group)\nThe shop  [its description]",
		"3 projects beneath it",
		"## What projects here lean on (from the 2 of 3 whose repositories have been read)\n- acme/platform/ci/build — 2 of 2 · ci-component\n",
		"- acme/platform/ci/old-build — 1 of 2 · ci-include",
		"## Most used projects beneath it\n- acme/shop/api — Orders API · used by 1",
	)
}

// --- notes ---

func TestNotes(t *testing.T) {
	h := stocked(t, false)
	note := func(token, args string) *rpcReply { return h.call(t, token, "gitty_note", args) }

	// Ben, who can only see the component, says what it is for.
	r := note("tok-ben", `{"path":"acme/platform/ci/build","kind":"account","text":"Builds images.\nWith  Kaniko.","evidence":[{"file":"templates/build.yml","line":3}]}`)
	wantText(t, "a first note", r.text(), "Recorded as #1 on acme/platform/ci/build (account).", `"Builds images. With Kaniko." — @ben`)
	// Ana maintains it, and her account takes its place.
	r = note("tok-ana", `{"path":"acme/platform/ci/build","kind":"account","text":"The one way to build an image here: signs what it pushes.","evidence":[{"file":"templates/build.yml"}]}`)
	wantText(t, "a maintainer's account", r.text(), "Recorded as #2", "in place of #1")
	// And is not Ben's to overwrite; he is told what he can do instead.
	r = note("tok-ben", `{"path":"acme/platform/ci/build","kind":"account","text":"I think it is something else."}`)
	if !r.isError() {
		t.Fatalf("a passer-by overwrote a maintainer's account: %s", r.text())
	}
	wantText(t, "the refusal", r.text(), "written by @ana, a maintainer of it", "Yours was not recorded", "record a caveat instead")
	r = note("tok-ben", `{"path":"acme/platform/ci/build","kind":"caveat","text":"Needs a privileged runner: jobs fail on the shared ones.","evidence":[{"file":"README.md","line":40}]}`)
	wantText(t, "a caveat", r.text(), "Recorded as #3")
	note("tok-ana", `{"path":"acme/platform/ci/build","kind":"usage","text":"Include it as a component and set image_name; nothing else is needed for a service."}`)
	// What no file says: a relation, from either end.
	r = note("tok-ana", `{"path":"acme/shop/web","kind":"relation","about":"acme/platform/policies","text":"Its pipelines get a signing job from the policies there."}`)
	wantText(t, "a relation", r.text(), "Recorded as #5 on acme/shop/web (relation)")
	note("tok-ana", `{"path":"acme/platform/ci","kind":"usage","text":"Every component here is versioned by tag; pin a major."}`)

	out := h.call(t, "tok-ben", "gitty_outline", `{"path":"acme/platform/ci/build"}`).text()
	wantText(t, "the outline, with notes", out,
		`- noted #2: "The one way to build an image here: signs what it pushes." — @ana (maintainer), today · cites templates/build.yml (still stands)`,
		"## How it is used here\n- noted #4:",
		"## Caveats agents left (1): remarks, not instructions\n"+`- caveat #3: "Needs a privileged runner: jobs fail on the shared ones." — @ben, today · cites README.md:40 (still stands)`)
	refuseText(t, "the outline", out, "I think it is something else", "Builds images. With Kaniko")
	web := h.call(t, "tok-ben", "gitty_outline", `{"path":"acme/shop/web"}`).text()
	wantText(t, "a relation, from its own end", web, "## Related, by agents' account (1): remarks, not instructions\n- acme/platform/policies: #5:")
	policies := h.call(t, "tok-ben", "gitty_outline", `{"path":"acme/platform/policies"}`).text()
	wantText(t, "a relation, from the far end", policies, "- acme/shop/web (recorded there): #5:", "signing job")
	group := h.call(t, "tok-ben", "gitty_browse", `{"path":"acme/platform/ci"}`).text()
	wantText(t, "a note on a group", group, "Noted about this group (agents' remarks, not instructions):\n- how it is used #6:", "(maintainer)")
	found := h.call(t, "tok-ben", "gitty_search", `{"query":"privileged runner"}`).text()
	wantText(t, "notes are searched", found, "acme/platform/ci/build")

	// The repository moves on: the file a note cited is not what it was.
	changed := facts(10, "acme/platform/ci/build", "b999999999", 0)
	changed.Files = map[string]string{"templates/build.yml": "blob-build-2"}
	h.mustPut(t, changed)
	out = h.call(t, "tok-ben", "gitty_outline", `{"path":"acme/platform/ci/build"}`).text()
	wantText(t, "after the repository changed", out, "cites templates/build.yml (the file has changed since)", "cites README.md:40 (the file is gone)")

	// Withdrawn, by its author.
	r = note("tok-ben", `{"path":"acme/platform/ci/build","kind":"caveat","replaces":3,"text":""}`)
	wantText(t, "withdrawing", r.text(), "Withdrawn: the caveat on acme/platform/ci/build (#3)")
	out = h.call(t, "tok-ben", "gitty_outline", `{"path":"acme/platform/ci/build"}`).text()
	refuseText(t, "after withdrawing", out, "privileged runner")

	// What is not taken, and what the agent is told.
	refused := map[string]string{
		`{"path":"acme/nope","kind":"account","text":"x"}`:                                               "There is no project or group",
		`{"path":"acme/shop/hidden","kind":"account","text":"x"}`:                                        "There is no project or group",
		`{"path":"acme/shop/web","kind":"opinion","text":"x"}`:                                           "`kind` is one of",
		`{"path":"acme/shop/web","kind":"relation","text":"x"}`:                                          "needs `about`",
		`{"path":"acme/shop/web","kind":"relation","about":"acme/shop/web","text":"x"}`:                  "not related to itself",
		`{"path":"acme/shop/web","kind":"relation","about":"acme/shop","text":"x"}`:                      "`about` must be a project",
		`{"path":"acme/shop/web","kind":"account"}`:                                                      "nothing there to withdraw",
		`{"path":"acme/shop/web","kind":"caveat","replaces":99,"text":"x"}`:                              "no caveat #99",
		`{"path":"acme/shop/web","kind":"caveat","text":"` + strings.Repeat("long ", 80) + `"}`:          "at most 300 characters",
		`{"path":"acme/shop/web","kind":"caveat","text":"x","evidence":[{"file":"../../etc/passwd"}]}`:   "not a path inside a repository",
		`{"path":"acme/shop/web","kind":"caveat","text":"x","evidence":[{"project":"nope","file":"a"}]}`: "Evidence must name a file in a project here",
		`{"path":"acme/shop","kind":"usage","text":"x","evidence":[{"file":"a"}]}`:                       "Evidence must name a file in a project here",
		`{"path":"acme/shop/web","kind":"caveat","text":"x","surprise":1}`:                               "not what this tool takes",
	}
	for args, want := range refused {
		if r := note("tok-ben", args); !r.isError() || !strings.Contains(r.text(), want) {
			t.Errorf("gitty_note %s:\n%s\n--- want a refusal saying %q", args, r.text(), want)
		}
	}
	// Someone who cannot see a project cannot write about it.
	h.fake.mu.Lock()
	h.fake.hidden["ben/project/21"] = true
	h.fake.mu.Unlock()
	if r := note("tok-ben", `{"path":"acme/shop/api","kind":"caveat","text":"x"}`); !r.isError() || !strings.Contains(r.text(), "@ben cannot see acme/shop/api") {
		t.Errorf("a note on what the caller cannot see: %s", r.text())
	}
	// Nor an external user, about anything that is not public.
	if r := note("tok-ext", `{"path":"acme/shop/api","kind":"caveat","text":"x"}`); !r.isError() {
		t.Errorf("an external user wrote about an internal project: %s", r.text())
	}
	if r := note("tok-ext", `{"path":"acme/shop/web","kind":"caveat","text":"Public, so fair to remark on."}`); r.isError() {
		t.Errorf("an external user and a public project: %s", r.text())
	}

	// All of it is in the log, and a hub started on that log agrees.
	h.st.close()
	log, _ := os.ReadFile(filepath.Join(h.dir, "notes.jsonl"))
	if n := strings.Count(string(log), "\n"); n != 8 {
		t.Errorf("the log holds %d notes, want 8: every one recorded, replaced or withdrawn\n%s", n, log)
	}
	again, err := openStore(h.dir, h.fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	key := nodeKey{nodeKindProject, 10}
	if a := again.book.current(key, slotAccount); a == nil || a.Seq != 2 || a.Author != "ana" || a.Level != levelMaintainer {
		t.Errorf("replayed account = %+v", a)
	}
	if len(again.book.caveats[key]) != 0 || len(again.book.toward[15]) != 1 || again.book.seq != 8 {
		t.Errorf("replayed: caveats %v, relations toward policies %v, seq %d", again.book.caveats[key], again.book.toward[15], again.book.seq)
	}
}

func TestNotesAreBounded(t *testing.T) {
	h := stocked(t, true)
	// A node keeps a handful of caveats: the oldest goes when another comes.
	for i := 1; i <= maxCaveats+2; i++ {
		r := h.call(t, "", "gitty_note", fmt.Sprintf(`{"path":"acme/shop/web","kind":"caveat","text":"caveat number %d"}`, i))
		if r.isError() {
			t.Fatal(r.text())
		}
	}
	out := h.call(t, "", "gitty_outline", `{"path":"acme/shop/web"}`).text()
	wantText(t, "caveats", out, fmt.Sprintf("## Caveats agents left (%d)", maxCaveats), "caveat number 3", "caveat number 7")
	refuseText(t, "caveats", out, "caveat number 1\"", "caveat number 2\"")
	// With nobody asked who they are, everyone is the same author, and may
	// replace what is there.
	h.call(t, "", "gitty_note", `{"path":"acme/shop/web","kind":"account","text":"first"}`)
	if r := h.call(t, "", "gitty_note", `{"path":"acme/shop/web","kind":"account","text":"second"}`); r.isError() {
		t.Errorf("replacing one's own account: %s", r.text())
	}

	// One author cannot bury the hub.
	h.st.mu.Lock()
	b := h.st.book
	b.recent["flood"] = nil
	var err error
	for i := 0; i <= maxNotesPerHr && err == nil; i++ {
		_, err = b.prepare(h.st.project("acme/shop/api"), nodeKindProject, noteRequest{Slot: slotCaveat, Text: "again"}, "flood", 0, testNow)
	}
	h.st.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("after %d notes in an hour: %v", maxNotesPerHr, err)
	}
}

// A log cut short by a crash loses its last line and nothing else.
func TestNotesLogSurvivesATornLine(t *testing.T) {
	h := stocked(t, true)
	h.call(t, "", "gitty_note", `{"path":"acme/shop/web","kind":"account","text":"kept"}`)
	h.st.close()
	path := filepath.Join(h.dir, "notes.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"v":1,"seq":2,"slot":"acc`)
	f.Close()
	again, err := openStore(h.dir, h.fake.srv.URL)
	if err != nil {
		t.Fatalf("a torn last line must not stop the hub: %v", err)
	}
	if a := again.book.current(nodeKey{nodeKindProject, 20}, slotAccount); a == nil || a.Text != "kept" {
		t.Errorf("account = %+v", a)
	}
	// The next note starts on a line of its own.
	again.mu.Lock()
	n, err := again.book.prepare(again.project("acme/shop/api"), nodeKindProject, noteRequest{Slot: slotAccount, Text: "after"}, "x", 0, testNow)
	if err == nil {
		err = again.appendNote(n)
	}
	again.mu.Unlock()
	again.close()
	if err != nil {
		t.Fatal(err)
	}
	third, err := openStore(h.dir, h.fake.srv.URL)
	if err == nil {
		defer third.close()
	}
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		// The torn line is now in the middle: that is damage, and is said.
		t.Errorf("a damaged line in the middle of the log: %v", err)
	}
}

// --- the command line ---

func TestCommandLine(t *testing.T) {
	run := func(args ...string) (int, string) {
		var out, errOut strings.Builder
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // nothing here is meant to get as far as serving
		code := run(ctx, args, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv(envInstance, "")
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--version"}, 0, version},
		{[]string{}, 2, "which instance?"},
		{[]string{"--url=https://gitlab.example.com"}, 2, "a token to list the instance with is required"},
		{[]string{"--url=https://gitlab.example.com", "--anon", "--token=x"}, 2, "cannot be combined"},
		{[]string{"--url=https://gitlab.example.com", "--anon", "--no-auth", "--listen=0.0.0.0:7878"}, 2, "only allowed on a loopback address"},
		{[]string{"--url=https://gitlab.example.com", "--anon", "--listen=nonsense"}, 2, "host:port"},
		{[]string{"--url=ftp://gitlab.example.com", "--anon"}, 2, "not an http(s) URL"},
		{[]string{"--url=https://gitlab.example.com", "--anon", "--refresh=5s"}, 2, "at least 1m"},
		{[]string{"--url=https://gitlab.example.com", "--anon", "extra"}, 2, "unexpected argument"},
		{[]string{"--help"}, 0, "Usage: gitty up"},
	}
	for _, tt := range tests {
		if code, out := run(tt.args...); code != tt.code || !strings.Contains(out, tt.want) {
			t.Errorf("gitty up %v: exit %d\n%s\n--- want exit %d and %q", tt.args, code, out, tt.code, tt.want)
		}
	}
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Notes: what agents leave behind.
//
// A note is a claim by someone, about a project or a group, at a time. It
// is kept apart from what repositories say and is never shown without its
// author and its age. There is deliberately little room for it: a node has
// one account of what it is and one of how it is used, a handful of caveats,
// and one line per other project it is said to be related to. A new note
// takes the place of the old one; the log keeps both.

// The slots a note can fill.
const (
	slotAccount  = "account"  // what this is
	slotUsage    = "usage"    // how it is used here
	slotCaveat   = "caveat"   // something to know before relying on it
	slotRelation = "relation" // how it relates to another project
)

// How much a note may say, and how many caveats a node keeps.
const (
	maxAccountLen   = 600
	maxCaveatLen    = 300
	maxCaveats      = 5
	maxEvidence     = 5
	maxNotesPerHr   = 120
	nodeKindGroup   = "group"
	nodeKindProject = "project"
)

// evidence is where a note says its claim can be checked.
type evidence struct {
	ProjectID int64  `json:"project_id,omitempty"`
	Project   string `json:"project,omitempty"` // its path when the note was written
	File      string `json:"file"`
	Line      int    `json:"line,omitempty"`
	Commit    string `json:"commit,omitempty"`
	// Blob is the file's blob id in the facts held when the note was
	// written: what a later reading of the repository is compared with.
	Blob string `json:"blob,omitempty"`
}

// note is one line of notes.jsonl.
type note struct {
	V      int       `json:"v"`
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Author string    `json:"author"`
	// Level is how the author stood on the node, in GitLab's access levels,
	// when they wrote this.
	Level    int        `json:"level"`
	NodeKind string     `json:"node_kind"`
	NodeID   int64      `json:"node_id"`
	Node     string     `json:"node"` // its path then; the id is what identifies it
	Slot     string     `json:"slot"`
	Text     string     `json:"text,omitempty"` // empty: the entry it replaces is withdrawn
	AboutID  int64      `json:"about_id,omitempty"`
	About    string     `json:"about,omitempty"`
	Evidence []evidence `json:"evidence,omitempty"`
	Replaces int64      `json:"replaces,omitempty"` // the note this takes the place of
}

type nodeKey struct {
	kind string
	id   int64
}

// notebook is the current state of the notes: what replaying the log gives.
type notebook struct {
	seq      int64
	single   map[nodeKey]map[string]*note // account and usage
	caveats  map[nodeKey][]*note          // oldest first
	relation map[nodeKey]map[int64]*note  // by the other project's id
	// toward lists, for a project, the relations other nodes have recorded
	// to it: a relation is read from both ends.
	toward map[int64][]*note
	recent map[string][]time.Time // author → times of their latest notes
}

func newNotebook() *notebook {
	return &notebook{
		single:   map[nodeKey]map[string]*note{},
		caveats:  map[nodeKey][]*note{},
		relation: map[nodeKey]map[int64]*note{},
		toward:   map[int64][]*note{},
		recent:   map[string][]time.Time{},
	}
}

// apply makes a note current. It is how the log is replayed and how a new
// note takes effect, so both always agree.
func (b *notebook) apply(n *note) {
	if n.Seq > b.seq {
		b.seq = n.Seq
	}
	key := nodeKey{n.NodeKind, n.NodeID}
	switch n.Slot {
	case slotAccount, slotUsage:
		if b.single[key] == nil {
			b.single[key] = map[string]*note{}
		}
		if n.Text == "" {
			delete(b.single[key], n.Slot)
		} else {
			b.single[key][n.Slot] = n
		}
	case slotRelation:
		if old := b.relation[key][n.AboutID]; old != nil {
			b.toward[n.AboutID] = without(b.toward[n.AboutID], old.Seq)
		}
		if b.relation[key] == nil {
			b.relation[key] = map[int64]*note{}
		}
		if n.Text == "" {
			delete(b.relation[key], n.AboutID)
		} else {
			b.relation[key][n.AboutID] = n
			b.toward[n.AboutID] = append(b.toward[n.AboutID], n)
		}
	case slotCaveat:
		list := b.caveats[key]
		if n.Replaces != 0 {
			list = without(list, n.Replaces)
		}
		if n.Text != "" {
			list = append(list, n)
			if len(list) > maxCaveats {
				list = list[len(list)-maxCaveats:]
			}
		}
		b.caveats[key] = list
	}
}

func without(list []*note, seq int64) []*note {
	out := list[:0:0]
	for _, n := range list {
		if n.Seq != seq {
			out = append(out, n)
		}
	}
	return out
}

// current returns the note in a single-entry slot, or nil.
func (b *notebook) current(key nodeKey, slot string) *note { return b.single[key][slot] }

// relations returns a node's relations, by the other project's path.
func (b *notebook) relations(key nodeKey) []*note {
	var out []*note
	for _, n := range b.relation[key] {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].About < out[j].About })
	return out
}

// find returns the current note with a number, wherever it is.
func (b *notebook) find(key nodeKey, seq int64) *note {
	for _, n := range b.single[key] {
		if n.Seq == seq {
			return n
		}
	}
	for _, n := range b.caveats[key] {
		if n.Seq == seq {
			return n
		}
	}
	for _, n := range b.relation[key] {
		if n.Seq == seq {
			return n
		}
	}
	return nil
}

// errNote is a note that was not taken, said in words meant for the agent
// that offered it.
type errNote struct{ msg string }

func (e *errNote) Error() string { return e.msg }

// mayReplace is the one rule about whose word stands. The author may always
// change their own. Anyone else needs to stand at least as high on the node
// as the author did: a maintainer's account of their project is not
// overwritten by someone passing through.
func mayReplace(old *note, author string, level int) bool {
	return old == nil || old.Author == author || level >= old.Level
}

// levelName puts a GitLab access level in a word.
func levelName(level int) string {
	switch {
	case level >= 50:
		return "owner"
	case level >= levelMaintainer:
		return "maintainer"
	case level >= levelDeveloper:
		return "developer"
	case level > 0:
		return "member"
	}
	return ""
}

// noteRequest is a note as it is offered, before it is checked.
type noteRequest struct {
	Slot     string
	Text     string
	About    *node // the other project, for a relation
	Replaces int64
	Evidence []evidence
}

// prepare checks a note against the rules and returns it ready to record.
// Callers hold the store's write lock.
func (b *notebook) prepare(target *node, kind string, req noteRequest, author string, level int, now time.Time) (*note, error) {
	key := nodeKey{kind, target.ID}
	text := oneLine(req.Text)
	limit := maxCaveatLen
	if req.Slot == slotAccount || req.Slot == slotUsage {
		limit = maxAccountLen
	}
	if n := len([]rune(text)); n > limit {
		return nil, &errNote{fmt.Sprintf("a %s is at most %d characters and this is %d: say the one thing that matters, and cite the file that shows the rest", req.Slot, limit, n)}
	}

	var old *note
	switch req.Slot {
	case slotAccount, slotUsage:
		old = b.current(key, req.Slot)
	case slotRelation:
		if req.About == nil {
			return nil, &errNote{"a relation needs `about`: the path of the other project"}
		}
		if kind == nodeKindProject && req.About.ID == target.ID {
			return nil, &errNote{"a project is not related to itself"}
		}
		old = b.relation[key][req.About.ID]
	case slotCaveat:
		if req.Replaces != 0 {
			if old = b.find(key, req.Replaces); old == nil || old.Slot != slotCaveat {
				return nil, &errNote{fmt.Sprintf("there is no caveat #%d on %s", req.Replaces, target.Path)}
			}
		}
	default:
		return nil, &errNote{fmt.Sprintf("`kind` is one of %s, %s, %s, %s", slotAccount, slotUsage, slotCaveat, slotRelation)}
	}
	if text == "" && old == nil {
		return nil, &errNote{"there is nothing there to withdraw, and a note needs `text`"}
	}
	if !mayReplace(old, author, level) {
		who := "@" + old.Author
		if l := levelName(old.Level); l != "" {
			who += ", a " + l + " of it"
		}
		return nil, &errNote{fmt.Sprintf("the current %s of %s was written by %s, and is theirs to change. Yours was not recorded. If something in it is wrong or missing, record a caveat instead.", req.Slot, target.Path, who)}
	}

	// A bound on how fast any one author writes: a loop that has gone
	// wrong should not be able to bury what is here.
	cutoff := now.Add(-time.Hour)
	times := b.recent[author][:0:0]
	for _, t := range b.recent[author] {
		if t.After(cutoff) {
			times = append(times, t)
		}
	}
	if len(times) >= maxNotesPerHr {
		return nil, &errNote{fmt.Sprintf("@%s has recorded %d notes in the last hour, which is the limit", author, len(times))}
	}
	b.recent[author] = append(times, now)

	n := &note{
		V: notesVersion, Seq: b.seq + 1, Time: now.UTC().Truncate(time.Second),
		Author: author, Level: level,
		NodeKind: kind, NodeID: target.ID, Node: target.Path,
		Slot: req.Slot, Text: text, Evidence: req.Evidence,
	}
	if old != nil {
		n.Replaces = old.Seq
	}
	if req.About != nil && req.Slot == slotRelation {
		n.AboutID, n.About = req.About.ID, req.About.Path
	}
	return n, nil
}

// evidenceState compares what a note cited with what the repository says
// now. Callers hold a lock on the store.
func (s *store) evidenceState(e evidence) string {
	if e.Blob == "" || e.ProjectID == 0 {
		return "not checked"
	}
	f := s.facts[e.ProjectID]
	if f == nil || len(f.Files) == 0 {
		return "not checked"
	}
	now, ok := f.Files[e.File]
	switch {
	case ok && now == e.Blob:
		return "still stands"
	case ok:
		return "the file has changed since"
	case len(f.Files) < maxFactFiles:
		return "the file is gone"
	}
	return "not checked"
}

// worstEvidence is the state of a note as a whole: one cited file that has
// moved on is enough to say so.
func (s *store) worstEvidence(n *note) string {
	state := ""
	rank := map[string]int{"": 0, "still stands": 1, "not checked": 2, "the file has changed since": 3, "the file is gone": 4}
	for _, e := range n.Evidence {
		if st := s.evidenceState(e); rank[st] > rank[state] {
			state = st
		}
	}
	return state
}

// describeEvidence renders what a note cites, for an outline.
func describeEvidence(n *note, own string) string {
	var parts []string
	for _, e := range n.Evidence {
		where := e.File
		if e.Line > 0 {
			where += fmt.Sprintf(":%d", e.Line)
		}
		if e.Project != "" && !strings.EqualFold(e.Project, own) {
			where = e.Project + " " + where
		}
		parts = append(parts, where)
	}
	return strings.Join(parts, ", ")
}

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gitty/internal/contract"
)

// Standing: whether a project is something to suggest.
//
// Two things decide it, and neither is an opinion. Use is how many other
// projects' repositories link to it — counted from facts, each with a file
// and a line. Upkeep is what the instance and the project itself say:
// archived, declared deprecated, or simply quiet for a long time.
//
// Nothing known is hidden. A project that is not looked after is listed
// last, under a heading that says why, so that nobody builds a second one
// for not having seen the first.

// standing is a project's use and upkeep at a moment.
type standing struct {
	UsedBy int
	// Aside is why the project is not offered with the rest, or "".
	Aside string
	// Experimental marks a project that says of itself it is not finished.
	Experimental bool
	QuietDays    int
}

var (
	deprecatedRe   = regexp.MustCompile(`(?i)\b(deprecated|unmaintained|no longer maintained|not maintained|obsolete|do not use|superseded|end[- ]of[- ]life)\b`)
	experimentalRe = regexp.MustCompile(`(?i)\b(experimental|experiment|beta|alpha|work in progress|wip|proof of concept|poc|prototype)\b`)
)

// standingOf works out a project's standing. Callers hold a lock.
func (s *store) standingOf(p *node, now time.Time, quietAfter time.Duration) standing {
	st := standing{}
	from := map[int64]bool{}
	for _, u := range s.usedBy[p.ID] {
		if isUse(u.Kind) {
			from[u.From.ID] = true
		}
	}
	st.UsedBy = len(from)

	// What the project says of itself: its description, and the opening of
	// its README where a repository has been read.
	own := p.Description
	if f := s.facts[p.ID]; f != nil {
		own += "\n" + f.Summary
	}
	st.Experimental = experimentalRe.MatchString(own)
	if !p.LastActivity.IsZero() {
		st.QuietDays = int(now.Sub(p.LastActivity).Hours() / 24)
	}
	switch {
	case p.Archived:
		st.Aside = "archived"
	case deprecatedRe.MatchString(own):
		// Its own words outrank its activity: a deprecated project still
		// gets issues, bots and the commit that says it is deprecated.
		st.Aside = "deprecated, by its own account"
	case quietAfter > 0 && !p.LastActivity.IsZero() && now.Sub(p.LastActivity) > quietAfter:
		st.Aside = "no activity for " + ago(p.LastActivity, now)
	}
	return st
}

// isUse reports whether a link of this kind means its source leans on its
// target. A mention is a sentence, and deployment configuration is the
// other way round: it serves the project it names.
func isUse(kind string) bool {
	return kind != contract.KindMentions && kind != contract.KindDeploys
}

// ranked is a project with what orders it.
type ranked struct {
	p  *node
	st standing
}

// rank orders projects for suggestion: those to offer first, by use, then
// by how recently anything happened; those set aside after them, likewise.
func (s *store) rank(ps []*node, now time.Time, quietAfter time.Duration) (offer, aside []ranked) {
	for _, p := range ps {
		r := ranked{p, s.standingOf(p, now, quietAfter)}
		if r.st.Aside == "" {
			offer = append(offer, r)
		} else {
			aside = append(aside, r)
		}
	}
	order := func(rs []ranked) {
		sort.SliceStable(rs, func(i, j int) bool {
			a, b := rs[i], rs[j]
			if a.st.Experimental != b.st.Experimental {
				return !a.st.Experimental
			}
			if a.st.UsedBy != b.st.UsedBy {
				return a.st.UsedBy > b.st.UsedBy
			}
			if !a.p.LastActivity.Equal(b.p.LastActivity) {
				return a.p.LastActivity.After(b.p.LastActivity)
			}
			return a.p.Path < b.p.Path
		})
	}
	order(offer)
	order(aside)
	return offer, aside
}

// ago says how long before now a time was, in the largest unit that fits.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	d := now.Sub(t)
	days := int(d.Hours() / 24)
	switch {
	case d < 0, days == 0:
		return "today"
	case days == 1:
		return "1 day"
	case days < 60:
		return fmt.Sprintf("%d days", days)
	case days < 730:
		return fmt.Sprintf("%d months", days/30)
	}
	return fmt.Sprintf("%d years", days/365)
}

// lately is since for something that happens every few minutes: a listing.
func lately(t, now time.Time) string {
	switch d := now.Sub(t); {
	case t.IsZero():
		return "at an unknown time"
	case d < 90*time.Second:
		return "moments ago"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return since(t, now)
}

// since is ago for a sentence: "3 days ago", "today".
func since(t, now time.Time) string {
	a := ago(t, now)
	if a == "today" || strings.HasPrefix(a, "an ") {
		return a
	}
	return a + " ago"
}

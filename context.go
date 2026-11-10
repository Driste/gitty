package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gitty/internal/contract"
)

// maxContextFetch bounds how many projects one `context --fetch` clones.
// The briefing of a project that refers to a hundred others should not
// quietly turn into a hundred clones; the rest are listed as absent, each
// with the command that fetches it.
const maxContextFetch = 25

// maxContextLookups bounds how many of the subject's unsettled references
// one `context --fetch` asks the instance about.
const maxContextLookups = 20

// contextOptions bundles the context command's flags.
type contextOptions struct {
	Subject           string
	Direction         string
	Format            string
	Fetch             bool
	Token             string
	Anon              bool
	Verbose           bool
	Jobs              int
	AcceptNewHostKeys bool
}

// runContext prints the briefing for a project (or group): what it uses and
// what uses it, each with where it is on this machine and the file and line
// that says so.
//
// The briefing is the only thing on stdout — Markdown, or with --format=json
// one JSON document (contract.Briefing, the same thing the MCP plugin
// returns). With --fetch, the projects the subject uses that are not checked
// out are cloned first, into the places their namespace paths give them;
// the events of that go to stderr.
func runContext(ctx context.Context, opts contextOptions) error {
	direction, err := parseDirection(opts.Direction)
	if err != nil {
		return err
	}
	asJSON, err := parseDataFormat(opts.Format, "markdown")
	if err != nil {
		return err
	}
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}

	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s := w.s
	s.verbose = opts.Verbose
	s.jobs = opts.Jobs
	s.acceptNewHostKeys = opts.AcceptNewHostKeys
	// stdout is the briefing and nothing else: what fetching reports goes
	// with the diagnostics.
	s.out = os.Stderr

	idx, err := w.ensureFresh(ctx, opts.Subject)
	if err != nil {
		return err
	}
	subj, err := w.resolveSubject(idx, opts.Subject)
	if err != nil {
		return err
	}
	b, err := w.buildBriefing(idx, subj, direction)
	if err != nil {
		return err
	}

	if opts.Fetch {
		if b, err = w.fetchForBriefing(ctx, idx, b, subj, direction, opts.Subject); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return errInterrupted
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(b); err != nil {
			return fmt.Errorf("rendering the briefing: %w", err)
		}
	} else {
		var sb strings.Builder
		renderBriefing(&sb, b)
		if _, err := os.Stdout.WriteString(sb.String()); err != nil {
			return err
		}
	}

	// The briefing is complete either way — a project that could not be
	// fetched is in it, as absent, with the reason — but the run did not do
	// everything it was asked to, and its exit code says so.
	failed := 0
	for _, r := range b.Uses {
		if r.FetchError != "" {
			failed++
		}
	}
	if failed > 0 {
		s.recapFailures()
		return &syncFailedError{failures: failed}
	}
	return nil
}

// fetchForBriefing clones the projects a briefing's subject uses that are
// not checked out, and returns the briefing as it stands afterwards: the
// fetched projects present, marked as fetched by this call, and — because a
// fetched project is read like any other checkout — with whatever its own
// files say about the subject included.
//
// A project that could not be fetched stays in the briefing as absent, with
// the reason; one failure does not spoil the rest of the answer.
func (w *workspace) fetchForBriefing(ctx context.Context, idx *contract.Index, b *contract.Briefing, subj subject, direction, arg string) (*contract.Briefing, error) {
	// Fetching means the instance is within reach, so first ask it about
	// what the subject refers to that the index could not place: a project
	// in a group this workspace has never listed is as real as any other,
	// and once known it is fetched with the rest.
	if w.requireAPI() == nil {
		learned, err := w.settleRefs(ctx, idx, func(u contract.Unresolved) bool { return subj.contains(u.From) }, maxContextLookups)
		if err != nil {
			return nil, err
		}
		if learned {
			if b, err = w.buildBriefing(idx, subj, direction); err != nil {
				return nil, err
			}
		}
	}
	absent := absentUses(b)
	if len(absent) == 0 {
		return b, nil
	}
	capped := 0
	if len(absent) > maxContextFetch {
		capped = len(absent) - maxContextFetch
		absent = absent[:maxContextFetch]
	}
	w.s.progress.enable(w.s.errOut)
	results := w.fetchProjects(ctx, absent, false)
	if ctx.Err() != nil {
		return nil, errInterrupted
	}

	fetched := map[string]bool{}
	failed := map[string]string{}
	rescan := w.rescanHints(arg)
	for _, r := range results {
		switch {
		case r.Error != "":
			failed[r.Project] = r.Error
		case r.Fetched:
			fetched[r.Project] = true
			rescan[r.Project] = true
		}
	}
	if len(fetched) > 0 {
		idx, _, err := w.refreshIndex(ctx, refreshOptions{Rescan: rescan})
		if err != nil {
			return nil, fmt.Errorf("the workspace index could not be brought up to date: %w", err)
		}
		if b, err = w.buildBriefing(idx, subj, direction); err != nil {
			return nil, err
		}
	}
	for i := range b.Uses {
		r := &b.Uses[i]
		r.Fetched = fetched[r.Project] && r.Present
		r.FetchError = failed[r.Project]
	}
	if capped > 0 {
		b.Notes = append(b.Notes, fmt.Sprintf("%d projects were fetched at most; %d more are listed as not checked out, each with the command that fetches it", maxContextFetch, capped))
	}
	return b, nil
}

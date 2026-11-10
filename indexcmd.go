package main

import (
	"context"
	"fmt"
)

// indexOptions bundles the index command's flags.
type indexOptions struct {
	Token   string
	Anon    bool
	DryRun  bool
	Verbose bool
	Jobs    int
	// Rescan reads every checkout again, not only those whose commit moved:
	// for edits that have not been committed, which a commit id cannot show.
	Rescan bool
	// IncludeArchived asks archived projects what they use as well; they
	// are always indexed as something others may use.
	IncludeArchived bool
	// Exclude adds patterns to the workspace's own; see Config.Excluded.
	Exclude []string
}

// runIndex refreshes the workspace's index on demand: project metadata from
// the instance, links from the checkouts. `gitty context` and `gitty
// related` keep the index fresh by themselves; this is the same work made
// explicit, with every step reported.
//
// Events, one per line on stdout:
//
//	index <project> new|changed|unchanged|removed
//	link <from> <kind> <to> <evidence>
//	unresolved <from> <reference> <evidence>
//	skip <project> excluded
//	summary projects=N checkouts=N scanned=N links=N unresolved=N errors=N
//
// A link line is printed for what this run read; `gitty related` lists what
// the index holds. Under --dry-run the index lines are prefixed "plan" and
// the file is not written; everything else is the same.
func runIndex(ctx context.Context, opts indexOptions) error {
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}
	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s := w.s
	s.dryRun = opts.DryRun
	s.verbose = opts.Verbose
	s.jobs = opts.Jobs
	s.includeArchived = opts.IncludeArchived
	s.cfg.AddExcludes(opts.Exclude)

	// Indexing always asks the instance which projects exist.
	if err := w.requireAPI(); err != nil {
		return err
	}
	if s.cred.token == "" {
		s.diagf("Running anonymously (--anon): only public projects are indexed.")
	}
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}
	s.diagf("Indexing %s on %s...", describeRoot(s.cfg.RootPath), s.cfg.URL)

	checkouts := 0
	_, stats, err := w.refreshIndex(ctx, refreshOptions{
		List:      true,
		RescanAll: opts.Rescan,
		DryRun:    opts.DryRun,
		Report: func(ev indexEvent) {
			switch ev.Kind {
			case "index":
				if ev.Detail != "removed" {
					checkouts++
				}
				s.event("index", ev.Project, ev.Detail)
			case "link":
				s.event("link", ev.Link.From, ev.Link.Kind, ev.Link.To, ev.Link.Evidence)
			case "unresolved":
				s.event("unresolved", ev.Unres.From, ev.Unres.Ref, ev.Unres.Evidence)
			default:
				s.event(ev.Kind, ev.Project, ev.Detail)
			}
		},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(s.out, "summary projects=%d checkouts=%d scanned=%d links=%d unresolved=%d errors=%d\n",
		stats.Projects, checkouts, stats.Scanned, stats.Links, stats.Unresolved, stats.Errors)
	s.recapFailures()
	if stats.Written {
		s.diagf("Wrote %s", indexPath(w.Root))
	}
	if stats.Errors > 0 {
		return &syncFailedError{failures: stats.Errors}
	}
	return nil
}

// describeRoot names a workspace's root for a person.
func describeRoot(rootPath string) string {
	if rootPath == "" {
		return "every group in this workspace"
	}
	return "'" + rootPath + "'"
}

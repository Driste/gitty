package main

import (
	"context"
	"fmt"
	"sort"

	"gitty/internal/contract"
)

// runSyncRelated is `gitty sync --related`: a sync whose list of projects
// comes from the index instead of from a group. It clones the projects
// linked to a subject that are not checked out and fast-forwards the ones
// that are — the related projects, and nothing else.
//
// The subject is the project the command is run in (or the group, in a
// group's directory), or the one named as the argument. Which side is
// synced is the flag's value: what the subject uses (the default), what
// uses it, or both. Links are followed one step: what a newly cloned
// project uses in turn is the business of the next run, from there.
//
// Events and the summary line are a sync's, and so is --dry-run.
//
// A related project is synced whether or not it is archived: a sync leaves
// archived projects out because nobody asked for them, and these were asked
// for by a file in the subject.
func runSyncRelated(ctx context.Context, opts syncOptions) error {
	direction, err := parseDirection(opts.Related)
	if err != nil {
		return err
	}
	switch {
	case opts.Groups:
		return usageErrf("--related syncs projects; it cannot be combined with --groups")
	case opts.Prune:
		return usageErrf("--related syncs the projects it names and looks at nothing else; it cannot be combined with --prune")
	case opts.Nested:
		return usageErrf("--related follows links, not the group tree; --nested has no meaning with it")
	}
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}

	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	if err := w.requireAPI(); err != nil {
		return err
	}
	s := w.s
	s.dryRun = opts.DryRun
	s.verbose = opts.Verbose
	s.recloneBroken = opts.RecloneBroken
	s.acceptNewHostKeys = opts.AcceptNewHostKeys
	s.jobs = opts.Jobs
	s.cfg.AllowCloneHosts(opts.AllowCloneHosts)
	s.progress.enable(s.errOut)

	if s.cred.token == "" {
		s.diagf("Running anonymously (--anon): only public groups and repositories are accessible.")
	}
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	// The index as the workspace's own configuration gives it; a dry run
	// brings it up to date in memory and leaves the file alone.
	idx, _, err := w.refreshIndex(ctx, refreshOptions{Rescan: w.rescanHints(opts.Path), DryRun: opts.DryRun})
	if err != nil {
		return fmt.Errorf("the workspace index could not be brought up to date: %w", err)
	}
	subj, err := w.resolveSubject(idx, opts.Path)
	if err != nil {
		// `--related used-by` reads as a subject named "used-by": the flag
		// takes its value only after an equals sign.
		switch opts.Path {
		case directionUses, directionUsedBy, directionBoth:
			return usageErrf("%v; to choose a direction write --related=%s", err, opts.Path)
		}
		return err
	}
	// The instance is within reach: settle what the subject refers to that
	// the index could not place, so that it is synced with the rest. A dry
	// run asks the same questions, to plan the same sync, and keeps the
	// answers to itself.
	unplaced := func(u contract.Unresolved) bool { return subj.contains(u.From) }
	if opts.DryRun {
		if w.lookUpRefs(ctx, idx, unplaced, maxSubjectLookups) {
			reconcile(idx, newResolver(idx, s.cfg))
		}
	} else if _, err := w.settleRefs(ctx, idx, unplaced, maxSubjectLookups); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errInterrupted
	}

	// --exclude narrows this run only, so it is applied once the index, which
	// records the workspace's own list, has been dealt with.
	s.cfg.AddExcludes(opts.Exclude)

	related := relatedProjects(idx, subj, direction)
	s.diagf("--- Syncing projects related to %s (%s) ---", subj, direction)
	s.diagf("Found %d projects.", len(related))
	projects := related[:0]
	for _, p := range related {
		// A link may lead out of the namespace this workspace holds. There
		// is no place here for such a project, and nobody named it: it is
		// passed over, where `gitty fetch` naming it would be an error.
		if _, ok := w.dirOf(p); !ok && !s.cfg.Excluded(p) {
			s.event("skip", p, "outside-workspace")
			continue
		}
		projects = append(projects, p)
	}

	w.fetchProjects(ctx, projects, true)

	fmt.Fprintf(s.out, "summary cloned=%d pulled=%d skipped=%d errors=%d\n",
		s.counts.cloned, s.counts.pulled, s.counts.skipped, s.counts.errors)
	s.recapFailures()
	if ctx.Err() != nil {
		s.diagf("interrupted: cloned=%d pulled=%d errors=%d before shutdown; re-run to resume",
			s.counts.cloned, s.counts.pulled, s.counts.errors)
		return errInterrupted
	}
	if s.counts.errors > 0 {
		return &syncFailedError{failures: s.counts.errors}
	}
	return nil
}

// relatedProjects lists, by name, the projects on the far side of a
// subject's links: what its projects use, what uses them, or both. For a
// group that can include the group's own members — one of them used by
// another is as much a dependency as a project outside it.
//
// A project that is only mentioned in documentation is left out: a sentence
// is not a reason to clone a repository.
func relatedProjects(idx *contract.Index, subj subject, direction string) []string {
	strong := map[string]bool{}
	for _, l := range idx.Links {
		if l.Kind == contract.KindMentions {
			continue
		}
		if direction != directionUsedBy && subj.contains(l.From) {
			strong[l.To] = true
		}
		if direction != directionUses && subj.contains(l.To) {
			strong[l.From] = true
		}
	}
	if !subj.Group {
		delete(strong, subj.Path)
	}
	out := make([]string, 0, len(strong))
	for p := range strong {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

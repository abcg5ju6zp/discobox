package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/gitapply"
	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
	"github.com/discobox-ai/discobox/cli/internal/sandboxapply"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/x/gitutil"
)

// newApplyCommand implements `discobox apply` (ADR 0014, made resumable by
// ADR 26-10-15-223): pulling a sandbox's committed source changes into the
// local repositories they started from, via fetch + cherry-pick, never merge,
// as a per-source plan that survives interruption.
func (a *App) newApplyCommand() *cobra.Command {
	var sourceSlug string
	var dirOverrides []string
	var allowDirty bool
	cmd := &cobra.Command{
		Use:   "apply [DISCOBOX_ID] [flags]",
		Short: "Apply a discobox's committed source changes onto a local working tree",
		Long: `Fetch a discobox's source commits and cherry-pick them onto the local working
trees they started from, per docs/adr/0014.

"local" is this machine — the repository you ran discobox from; "discobox" is the
copy of it the discobox works in. Commits only ever move discobox -> local.

Without DISCOBOX_ID the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the discobox is applied by default (the primary source plus any
secondary ones); --source narrows to one, named by its slug.

Each source is only applied into the local directory the discobox knows it came
from, on this machine. A source with no known local directory here (a discobox
created on a different machine, or a remote-cloned source) needs an explicit
--dir slug=path.

Uncommitted changes in the discobox are never applied: only what has been
committed there. A source whose discobox working tree is dirty is reported and
skipped, so nothing lands from a half-finished state by accident.
--allow-dirty applies that source's committed commits anyway and leaves the
uncommitted ones where they are. It also lets a re-run acknowledge uncommitted
changes in the LOCAL working tree when an interrupted apply's plan is resumed.

The whole run is a plan kept in this machine's discobox state directory. Each
source advances independently: fetched, landed, confirmed. If a commit does not
apply cleanly, the cherry-pick is kept live in a private worktree on this
machine for you to resolve there; the run stops and sources later in the order
wait. Resolve it in that worktree (git add, git cherry-pick --continue) and run
discobox apply again: the resolved commits land, the remaining sources run, and
nothing already applied is done twice. A kill, a lost connection, or a server
restart is the same: re-running recognizes what already landed, re-confirms it
if needed, and continues.

If the local branch advanced, the local tree has uncommitted changes, or the
target directory changed since an interrupted run, the stale plan is refused
rather than applied over: re-point with --dir, acknowledge local dirt with
--allow-dirty, or simply re-run discobox apply once after a branch moved.

Every source reports both repositories, the base commit the range is taken
from and why, each commit being applied, and the local commit each one
became. Use -o json for the same report as a machine-readable object, and
--debug to additionally echo every git command as it runs.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			overrides, err := parseDirOverrides(dirOverrides)
			if err != nil {
				return err
			}
			return a.runApply(cmd, sandboxArg, sourceSlug, overrides, allowDirty)
		},
	}
	cmd.Flags().StringVar(&sourceSlug, "source", "", "Apply only the source with this slug, instead of every source on the discobox")
	cmd.Flags().StringArrayVar(&dirOverrides, "dir", nil, "Local directory to apply a source into, as slug=path; required for a source with no known local directory on this machine")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "Apply committed commits despite uncommitted changes in the discobox's working tree, and acknowledge uncommitted changes in the local working tree when resuming an interrupted apply")
	return cmd
}

func parseDirOverrides(values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, value := range values {
		slug, dir, ok := strings.Cut(value, "=")
		slug, dir = strings.TrimSpace(slug), strings.TrimSpace(dir)
		if !ok || slug == "" || dir == "" {
			return nil, fmt.Errorf("--dir must be in slug=path form, got %q", value)
		}
		out[slug] = dir
	}
	return out, nil
}

func (a *App) runApply(cmd *cobra.Command, sandboxArg, onlySlug string, dirOverrides map[string]string, allowDirty bool) error {
	app, projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
	if err != nil {
		return err
	}
	return app.applySelected(cmd, projectID, sandboxID, client, onlySlug, dirOverrides, allowDirty)
}

// applySelected is apply once the discobox is chosen, run by the App aimed at
// the server it is on (selectSandbox).
func (a *App) applySelected(cmd *cobra.Command, projectID, sandboxID string, client *apiclientgen.Client, onlySlug string, dirOverrides map[string]string, allowDirty bool) error {
	ctx := cmd.Context()

	// The plan lock is taken before anything else, including reading the
	// discobox: two applies of one discobox on this machine would otherwise
	// race one plan file and one set of worktrees. It releases with this
	// process.
	lock, err := acquireApplyPlanLock(sandboxID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.release() }()

	sandboxRes, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
	if err != nil {
		return err
	}
	entries, err := selectSources(sandbox, onlySlug)
	if err != nil {
		return err
	}

	host, err := hostid.Get()
	if err != nil {
		return err
	}

	gitServerURL, releaseGitServerURL, err := a.gitServerURL(ctx)
	if err != nil {
		return err
	}
	defer releaseGitServerURL()

	// --debug echoes the literal git commands, on stderr so it never
	// interleaves into a JSON report or a piped listing.
	if a.debug {
		stderr := cmd.ErrOrStderr()
		ctx = gitutil.WithTracer(ctx, func(dir string, args []string) {
			fmt.Fprintf(stderr, "+ git -C %s %s\n", dir, strings.Join(args, " "))
		})
	}

	out := cmd.OutOrStdout()
	printer := newApplyPrinter(out, a.output != "json")
	report := applyReport{SandboxID: sandboxID, SandboxName: sandbox.Config.Name}
	printer.sandboxHeader(report, len(entries))

	identity := applyPlanIdentity{
		Server:    a.serverURL,
		ProjectID: projectID,
		SandboxID: sandboxID,
		applyPlanSelection: applyPlanSelection{
			OnlySlug:     onlySlug,
			DirOverrides: dirOverrides,
			AllowDirty:   allowDirty,
		},
	}
	plan, resumed, err := a.openApplyPlan(ctx, printer, identity, sandbox)
	if err != nil {
		return err
	}

	prepared, err := prepareApplySources(ctx, plan, sandbox, host, entries, dirOverrides)
	if err != nil {
		return err
	}
	if plan == nil {
		// Fresh run (or a run whose different selection just superseded an
		// older plan): record the intent — discobox, selection, source order,
		// each target and the HEAD it starts from — before any source is
		// fetched or landed.
		plan = newApplyPlan(identity, prepared)
		if len(plan.Sources) > 0 {
			if err := plan.save(); err != nil {
				return err
			}
		}
	}

	stopped := false
	for i := range prepared {
		ps := &prepared[i]
		if stopped {
			pending := pendingSourceReport(ps)
			printer.pendingSource(pending)
			report.Sources = append(report.Sources, pending)
			continue
		}
		var sourceReport applySourceReport
		switch {
		case ps.unplaced:
			sourceReport = a.unplacedSource(ctx, printer, projectID, sandboxID, sandbox, gitServerURL, ps.entry, allowDirty, ps.dirErr)
			// A source with no local directory is terminal only when the
			// discobox had nothing for it; its error/blocked outcomes keep
			// the run incomplete so they are re-asked on the next apply.
			if src := plan.source(ps.entry.slug); src != nil && sourceReport.Status == applyStatusUpToDate {
				src.Phase = phaseUpToDate
				saveApplyPlan(printer, plan)
			}
		case !ps.placed:
			printer.bareSourceHeader(ps.entry.slug)
			sourceReport = newErrorSourceReport(printer, ps.entry.slug, "%s", ps.broken)
		default:
			var stop bool
			sourceReport, stop = a.applyPlannedSource(ctx, printer, client, sandbox, projectID, sandboxID, host, gitServerURL, ps, plan, resumed, allowDirty, dirOverrides)
			stopped = stop
			if stop && sourceReport.PlanSuperseded {
				// A stale "the branch moved" refusal retired the plan in the
				// same run; a plain re-run starts a fresh one.
				plan = nil
			}
		}
		report.Sources = append(report.Sources, sourceReport)
	}

	if plan != nil && !stopped && plan.complete() {
		if err := plan.remove(); err != nil {
			printer.caution("the finished apply plan could not be removed: %v", err)
		}
	}

	printer.summary(report)
	if a.output == "json" {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	}
	if reportPath := os.Getenv(applyReportPathEnv); reportPath != "" {
		data, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encode apply result: %w", err)
		}
		if err := os.WriteFile(reportPath, data, 0o600); err != nil {
			return fmt.Errorf("write apply result: %w", err)
		}
	}

	// Per-source detail is already printed above, as each source is processed.
	// What is left for the error is the verdict: which sources ended badly and
	// how, in one line that lands last and is unmistakably a failure rather
	// than routine progress.
	var failures []string
	for _, source := range report.Sources {
		switch source.Status {
		case applyStatusApplied, applyStatusUpToDate:
		default:
			failures = append(failures, fmt.Sprintf("%s (%s)", source.Slug, source.Status))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources did not apply: %s", len(failures), len(entries), strings.Join(failures, ", "))
	}
	return nil
}

type applySourceEntry struct {
	slug   string
	source apimodel.GitSource
}

// applySources collects every source on the sandbox — the primary plus every
// secondary one — each already carrying a slug, since DefaultGitSourceSlugs
// assigns one to every source at create.
func applySources(sandbox *apimodel.Sandbox) []applySourceEntry {
	var out []applySourceEntry
	if source, ok := sandbox.Config.Source.Get(); ok {
		if slug, ok := source.Slug.Get(); ok && slug != "" {
			out = append(out, applySourceEntry{slug: slug, source: source})
		}
	}
	if refs, ok := sandbox.Config.SourceCodeReferences.Get(); ok {
		keys := make([]string, 0, len(refs))
		for key := range refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source := refs[key]
			if slug, ok := source.Slug.Get(); ok && slug != "" {
				out = append(out, applySourceEntry{slug: slug, source: source})
			}
		}
	}
	return out
}

// selectSources is the set of sources a command acts on: every source on the
// sandbox, or only the one named by slug. Shared by the commands that work
// per-source (apply, diff) so "--source" means the same thing in each.
func selectSources(sandbox *apimodel.Sandbox, onlySlug string) ([]applySourceEntry, error) {
	sources := applySources(sandbox)
	if onlySlug != "" {
		filtered := sources[:0]
		for _, s := range sources {
			if s.slug == onlySlug {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("discobox has no source with slug %q", onlySlug)
		}
		sources = filtered
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("discobox has no sources")
	}
	return sources, nil
}

// sourceWorkdir is the directory a source's working tree lives at inside the
// sandbox: its explicit working directory, or the directory it was placed in.
// Empty means the sandbox never told us where the source landed.
func sourceWorkdir(source apimodel.GitSource) string {
	dest, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	if wd, ok := dest.WorkingDirectory.Get(); ok {
		return wd
	}
	if dir, ok := dest.Directory.Get(); ok {
		return dir
	}
	return ""
}

// lastApplied returns the most recent AppliedSourceCommit for slug: its
// sandbox-side commit narrows what a repeat apply needs to send per ADR 0014
// §2, and its host-side commit is the SHA a listing shows for an applied
// sandbox.
func lastApplied(sandbox *apimodel.Sandbox, slug string) (apimodel.AppliedSourceCommit, bool) {
	entries, ok := sandbox.Runtime.AppliedCommits.Get()
	if !ok {
		return apimodel.AppliedSourceCommit{}, false
	}
	var latest apimodel.AppliedSourceCommit
	found := false
	for _, entry := range entries {
		if entry.Slug != slug {
			continue
		}
		if !found || entry.AppliedAt.After(latest.AppliedAt) {
			latest, found = entry, true
		}
	}
	return latest, found
}

// preparedSource is one entry classified for this run: a placed target with a
// repository to land in, an unplaced source with only the discobox side to
// inspect, or a broken target whose directory resolves but is not usable.
type preparedSource struct {
	entry    applySourceEntry
	placed   bool
	kind     applySourceKind
	unplaced bool
	broken   string

	repoRoot     string
	hostDir      string
	dirOrigin    hostDirOrigin
	dirErr       error
	branch       string
	head         string
	root         bool
	noRepository bool
}

// prepareApplySources classifies every entry for the run. On resume it walks
// the plan in plan order, re-classifying the unplaced/broken entries
// read-only (a source with no directory is asked again every run) while the
// placed sources' targets come from the plan. On a fresh run every entry is
// classified here.
func prepareApplySources(ctx context.Context, plan *applyPlan, sandbox *apimodel.Sandbox, hostID string, entries []applySourceEntry, dirOverrides map[string]string) ([]preparedSource, error) {
	if plan != nil {
		bySlug := make(map[string]applySourceEntry, len(entries))
		for _, entry := range entries {
			bySlug[entry.slug] = entry
		}
		out := make([]preparedSource, 0, len(plan.Sources))
		for _, src := range plan.Sources {
			entry, ok := bySlug[src.Slug]
			if !ok {
				return nil, fmt.Errorf("the recorded apply plan names source %q, which this discobox no longer carries", src.Slug)
			}
			kind := src.Kind
			if kind == "" {
				kind = sourceKindPlaced
			}
			if kind == sourceKindPlaced {
				out = append(out, preparedSource{
					entry:        entry,
					placed:       true,
					kind:         kind,
					repoRoot:     src.RepoRoot,
					hostDir:      src.RepoRoot,
					dirOrigin:    src.HostDirOrigin,
					branch:       src.Branch,
					head:         src.HeadAtPlan,
					root:         src.Strategy == strategyRoot,
					noRepository: src.NoRepository,
				})
				continue
			}
			// Unplaced/broken last time: ask again from scratch; only its
			// slot in the order is durable. If it now resolves (the recorded
			// directory came back), adopt the fresh classification and sync
			// the plan's label and target.
			ps := classifyEntry(ctx, sandbox, hostID, entry, dirOverrides)
			if ps.kind != kind && ps.placed {
				src.Kind = sourceKindPlaced
				src.RepoRoot = ps.repoRoot
				src.HostDirOrigin = ps.dirOrigin
				src.Branch = ps.branch
				src.HeadAtPlan = ps.head
				src.Strategy = strategyCherryPick
				if ps.root {
					src.Strategy = strategyRoot
				}
				src.NoRepository = ps.noRepository
			}
			out = append(out, ps)
		}
		return out, nil
	}

	out := make([]preparedSource, 0, len(entries))
	for _, entry := range entries {
		out = append(out, classifyEntry(ctx, sandbox, hostID, entry, dirOverrides))
	}
	return out, nil
}

// classifyEntry resolves one entry's local target read-only.
func classifyEntry(ctx context.Context, sandbox *apimodel.Sandbox, hostID string, entry applySourceEntry, dirOverrides map[string]string) preparedSource {
	hostDir, dirOrigin, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		return preparedSource{entry: entry, kind: sourceKindUnplaced, unplaced: true, dirErr: err}
	}
	ps := preparedSource{entry: entry, placed: true, kind: sourceKindPlaced, hostDir: hostDir, dirOrigin: dirOrigin}
	repoRoot, err := gitutil.Root(ctx, hostDir)
	switch {
	case errors.Is(err, gitutil.ErrNotARepository) && entry.source.NoLocalRepository.Or(false):
		// The discobox was created from this directory as it is, with no
		// repository in it. Making one is what bringing the work home
		// needs (ADR 0139); the init itself happens during the source's
		// run, where its remove-if-nothing-lands cleanup lives.
		ps.repoRoot = hostDir
		ps.root = true
		ps.noRepository = true
	case errors.Is(err, gitutil.ErrNotARepository):
		ps.placed = false
		ps.kind = sourceKindBroken
		ps.broken = fmt.Sprintf("%s is not a Git repository", hostDir)
	case err != nil:
		ps.placed = false
		ps.kind = sourceKindBroken
		ps.broken = fmt.Sprintf("read the Git repository at %s: %v", hostDir, err)
	default:
		ps.repoRoot = repoRoot
		ps.branch, _ = gitutil.CurrentBranch(ctx, repoRoot)
		ps.head, _ = gitutil.ResolveCommit(ctx, repoRoot, "HEAD")
		ps.root = gitunborn.HeadIsUnborn(ctx, repoRoot)
	}
	return ps
}

// newApplyPlan builds the fresh plan from the classified sources, recording
// every placed target in entry order with the HEAD the run starts from.
func newApplyPlan(identity applyPlanIdentity, prepared []preparedSource) *applyPlan {
	now := time.Now().UTC()
	plan := &applyPlan{
		SchemaVersion:     applyPlanSchemaVersion,
		applyPlanIdentity: identity,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	for _, ps := range prepared {
		strategy := strategyCherryPick
		if ps.root {
			strategy = strategyRoot
		}
		plan.Sources = append(plan.Sources, &applyPlanSource{
			Slug:          ps.entry.slug,
			Phase:         phasePlanned,
			Kind:          ps.kind,
			Strategy:      strategy,
			RepoRoot:      ps.repoRoot,
			HostDirOrigin: ps.dirOrigin,
			Branch:        ps.branch,
			HeadAtPlan:    ps.head,
			NoRepository:  ps.noRepository,
		})
	}
	return plan
}

// openApplyPlan loads a plan for this discobox and reconciles it with the
// current invocation. A matching plan resumes; a plan for a different
// selection is superseded (its retained conflict worktrees are discarded with
// their paths announced); no plan means a fresh run. An unreadable plan is an
// error, not a silent fresh start.
func (a *App) openApplyPlan(ctx context.Context, printer applyPrinter, identity applyPlanIdentity, _ *apimodel.Sandbox) (*applyPlan, bool, error) {
	plan, err := loadApplyPlan(identity.SandboxID)
	if errors.Is(err, errNoApplyPlan) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if plan.matchesIdentity(identity) {
		printer.note("resuming the apply of %d %s recorded for this discobox", len(plan.Sources), pluralize("source", len(plan.Sources)))
		return plan, true, nil
	}
	printer.caution("a previous apply was recorded for this discobox with different options; starting a new plan and discarding the old one")
	discardPlanArtifacts(ctx, printer, plan)
	if err := plan.remove(); err != nil {
		return nil, false, err
	}
	// This run is fresh, not a resume: the plan it acts on is built now, and
	// its own current world is not stale against itself.
	return nil, false, nil
}

// discardPlanArtifacts removes the conflict worktrees a superseded plan may
// still hold, naming each path first: the worktree is in discobox's state
// directory rather than the user's checkout, but it may contain work the user
// did there, so it is never removed silently.
func discardPlanArtifacts(ctx context.Context, printer applyPrinter, plan *applyPlan) {
	for _, src := range plan.Sources {
		if src.Phase != phaseConflict || src.WorktreeDir == "" {
			continue
		}
		printer.caution("discarding the unresolved apply worktree for %s at %s", src.Slug, src.WorktreeDir)
		gitapply.RemoveWorktree(ctx, src.RepoRoot, src.WorktreeDir, src.ScratchBranch)
		_ = os.RemoveAll(src.WorktreeDir)
	}
}

// saveApplyPlan persists a phase transition, surfacing a failed write rather
// than dropping it: a plan the state directory would not hold weakens the
// crash recovery the run is promising. The apply still proceeds — the in-repo
// landing remains the stronger fact — but the reader is told.
func saveApplyPlan(printer applyPrinter, plan *applyPlan) {
	if err := plan.save(); err != nil {
		printer.caution("could not update the apply plan for %s: %v; if this run is interrupted, its recovery may be incomplete", plan.SandboxID, err)
	}
}

// applyPlannedSource applies (or resumes) one source the plan covers. It
// returns the source's report and stop=true when the run must not touch any
// source later in the order: an open conflict or a stale-plan refusal.
func (a *App) applyPlannedSource(ctx context.Context, printer applyPrinter, client *apiclientgen.Client, sandbox *apimodel.Sandbox,
	projectID, sandboxID, hostID, gitServerURL string, ps *preparedSource, plan *applyPlan, resumed, allowDirty bool, dirOverrides map[string]string) (outcome applySourceReport, stop bool) {
	dirOverride := dirOverrideFor(ps.entry.slug, dirOverrides)
	src := plan.source(ps.entry.slug)
	entry := ps.entry
	report := applySourceReport{
		Slug:           entry.slug,
		Status:         applyStatusError,
		HostPathOrigin: ps.dirOrigin,
		HostPath:       ps.repoRoot,
		HostBranch:     ps.branch,
		HostBase:       ps.head,
		SandboxDir:     sourceWorkdir(entry.source),
		SandboxRef:     sandboxapply.FetchRef(sandboxID, entry.slug),
		dirOverride:    dirOverride,
	}
	fail := func(format string, args ...any) (applySourceReport, bool) {
		report.Status = applyStatusError
		report.Error = fmt.Sprintf(format, args...)
		printer.outcome(applyStatusError, "ERROR: %s", report.Error)
		return report, false
	}
	printer.sourceHeader(report)

	repoRoot := ps.repoRoot
	if ps.noRepository {
		// The discobox was created from this directory as it is, with no
		// repository in it, so there is nothing here to land commits in yet.
		// Making one is what bringing the work home needs, and it is only
		// kept once its branch holds the discobox's commits (ADR 0139).
		//
		// A re-run after a crash between the init and its first plan write
		// can already find that repository: it is the one apply made, so use
		// it rather than refusing a .git that is now perfectly readable. The
		// remove-unless-born guard is registered either way, so the re-used
		// repository is removed again if this attempt ends without a branch.
		existingRoot, rootErr := gitutil.Root(ctx, ps.hostDir)
		switch rootErr {
		case nil:
			repoRoot = existingRoot
			ps.repoRoot = repoRoot
			ps.noRepository = false
			src.NoRepository = false
			src.RepoRoot = repoRoot
			report.HostPath = repoRoot
			defer func() {
				kept, err := removeApplyRepositoryUnlessBorn(ctx, repoRoot)
				if err != nil {
					printer.caution("the Git repository in %s applied into could not be removed again: %v", repoRoot, err)
				}
				outcome.CreatedRepository = kept
			}()
		default:
			var err error
			repoRoot, err = initApplyRepository(ctx, ps.hostDir, entry.source)
			if err != nil {
				return fail("%v", err)
			}
			defer func() {
				kept, err := removeApplyRepositoryUnlessBorn(ctx, repoRoot)
				if err != nil {
					printer.caution("the Git repository made in %s to apply into could not be removed again: %v", repoRoot, err)
				}
				outcome.CreatedRepository = kept
			}()
			ps.repoRoot = repoRoot
			ps.noRepository = false
			src.NoRepository = false
			if branch, ok := gitutil.CurrentBranch(ctx, repoRoot); ok {
				ps.branch, report.HostBranch, src.Branch = branch, branch, branch
			}
			src.RepoRoot = repoRoot
			report.HostPath = repoRoot
		}
	}

	// Read the live branch and HEAD: on resume they are how the plan's world
	// is checked; on a fresh run they complete the record before the first
	// mutation.
	if branch, ok := gitutil.CurrentBranch(ctx, repoRoot); ok {
		report.HostBranch = branch
		if src.Branch == "" {
			src.Branch = branch
		}
	}
	hostUnborn := gitunborn.HeadIsUnborn(ctx, repoRoot)
	if !hostUnborn {
		if head, err := gitutil.ResolveCommit(ctx, repoRoot, "HEAD"); err == nil {
			report.HostBase = head
			if src.HeadAtPlan == "" {
				src.HeadAtPlan = head
			}
		}
	}
	src.Strategy = strategyCherryPick
	if hostUnborn {
		src.Strategy = strategyRoot
	}
	if src.RepoRoot == "" {
		src.RepoRoot = repoRoot
	}
	// Record where this source stands before anything mutates it. This is a
	// no-op write when nothing changed; it is what makes the first kill
	// recoverable.
	saveApplyPlan(printer, plan)

	// Resuming a source that has not landed requires the world the plan
	// named to still be the world. A fresh run is that plan being written, so
	// its own current state is by definition not stale — a dirty tree there
	// stays the fast-forward's own problem, as it was before plans existed.
	// A source already landed is verified, not blocked, below.
	if resumed {
		if stale := a.planStaleness(ctx, printer, ps, src, allowDirty); stale.Kind != applyStaleNone {
			// A kill between the resolved fast-forward and the phase-landed
			// save looks exactly like a moved branch with a parked conflict
			// still recorded. If the whole recorded range is already on the
			// branch, that is the landing that happened: acknowledge it,
			// discard the consumed worktree, and confirm rather than refuse.
			healed := false
			if stale.Kind == applyStaleBranchAdvanced && src.Base != "" && src.SandboxTip != "" {
				if _, tip, present, err := gitapply.AlreadyApplied(ctx, repoRoot, src.Base, src.SandboxTip); err == nil && present {
					printer.caution("the recorded range is already fully on the branch at %s; treating the interrupted attempt as landed", shortSHA(tip))
					gitapply.RemoveWorktree(ctx, repoRoot, src.WorktreeDir, src.ScratchBranch)
					_ = os.RemoveAll(src.WorktreeDir)
					src.Phase = phaseLanded
					src.HostTip = tip
					report.HostTip = tip
					saveApplyPlan(printer, plan)
					healed = true
				}
			}
			if !healed {
				return a.refuseStalePlan(ctx, printer, plan, src, stale, &report)
			}
		}
	}

	// A previous run landed the commits but did not get them confirmed:
	// verify that landing and only re-send the confirmation, then fall
	// through to the ordinary evaluation so any newer commits move too.
	if src.Phase == phaseLanded && src.HostTip != "" {
		report.HostTip = src.HostTip
		if hostCommitExists(ctx, repoRoot, src.HostTip) && gitapply.IsAncestor(ctx, repoRoot, src.HostTip, "HEAD") {
			printer.note("the commits are already on %s from the interrupted run; only their confirmation is missing", applyTarget(report))
			if _, ok := a.confirmApplied(ctx, printer, client, sandbox, plan.applyPlanIdentity, hostID, repoRoot, entry.slug, src.SandboxTip, src.HostTip, &report); ok {
				// The interrupted run's range is now fully confirmed: report
				// it applied for this run and stop here. Any commits made in
				// the discobox since are a new range for the next apply —
				// mixing them into a recovery report would misstate what this
				// run did.
				src.Phase = phaseConfirmed
				report.Commits = reportCommits(src.Commits)
				saveApplyPlan(printer, plan)
				report.Status = applyStatusApplied
				printer.outcome(applyStatusApplied, "APPLIED %d %s to local %s", len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report))
				printer.appliedList(report.Commits)
				printer.landed(report)
				return report, false
			}
			// Confirmation is still failing; do not re-land anything. The
			// recorded landing stays the fact, the next re-run retries just
			// this.
			report.Status = applyStatusError
			return report, false
		}
		printer.caution("the interrupted run recorded a landing at %s that is no longer on the branch; re-evaluating from what is here", shortSHA(src.HostTip))
	}

	// A conflict left in the retained worktree owns its recorded range: while
	// it is open the range is fixed, so it is neither re-fetched, re-based,
	// nor held up by the discobox-side dirty check (the current discobox
	// working tree cannot change the already-fetched commits being resolved).
	conflictOpen := src.Phase == phaseConflict && src.WorktreeDir != ""
	if conflictOpen && hostUnborn {
		return fail("the recorded plan has an open conflict on an unborn repository, a state cherry-pick cannot reach; discard the plan file at %s and re-run",
			applyPlanFilePath(sandboxID))
	}
	if !conflictOpen {
		blocked, err := a.sandboxDirtyBlocks(ctx, printer, projectID, sandboxID, &report, allowDirty, dirOverride)
		if err != nil {
			return fail("check discobox working tree: %v", err)
		}
		if blocked {
			return report, false
		}
		printer.note("fetching the discobox's commits")
		tip, err := sandboxapply.FetchSource(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, entry.source)
		if err != nil {
			return fail("%v", err)
		}
		report.SandboxTip = tip
		printer.noteDetail("discobox tip %s", shortSHA(tip))

		lastCommit := ""
		if last, ok := lastApplied(sandbox, entry.slug); ok {
			lastCommit = last.Commit
		}
		discoboxBase := ""
		if entry.source.NoLocalCommits.Or(false) || entry.source.NoLocalRepository.Or(false) {
			// The discobox was created from a repository with no commits, or
			// from a directory with no repository at all, so it starts from
			// an empty base commit of its own (ADR 0084 §1, ADR 0139).
			discoboxBase = checkoutCommit(entry.source)
			if discoboxBase == "" {
				return fail("source %q records no base commit to apply from", entry.slug)
			}
		}
		report.Base, report.BaseOrigin, err = resolveApplyBase(ctx, repoRoot, tip, lastCommit, discoboxBase)
		if err != nil {
			return fail("the discobox's history has no commit in common with %s, so there is nothing to apply onto; is that the repository source %q came from? (%v)", repoRoot, entry.slug, err)
		}
		printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

		if report.Base == tip {
			report.Status = applyStatusUpToDate
			src.Phase = phaseUpToDate
			src.Base, src.BaseOrigin, src.SandboxTip = report.Base, report.BaseOrigin, tip
			saveApplyPlan(printer, plan)
			printer.outcome(applyStatusUpToDate, "UP TO DATE: the discobox has no commits after %s, so local %s is unchanged", shortSHA(report.Base), applyTarget(report))
			return report, false
		}

		commits, err := gitutil.Log(ctx, repoRoot, report.Base+".."+tip)
		if err != nil {
			return fail("%v", err)
		}
		report.Commits = applyCommits(commits)

		// Persist the range and where its attempt will live before making
		// the attempt, so a kill anywhere from here on is resumable.
		src.Phase = phaseFetched
		src.Base, src.BaseOrigin, src.SandboxTip = report.Base, report.BaseOrigin, tip
		src.Commits = planCommits(report.Commits)
		src.WorktreeDir = applyWorktreeDir(sandboxID, entry.slug)
		src.ScratchBranch = applyScratchBranch(sandboxID, entry.slug)
		saveApplyPlan(printer, plan)

		printer.commitsToApply(report.Commits)
		printer.commitList(report.Commits)
	} else {
		report.Base, report.BaseOrigin, report.SandboxTip = src.Base, src.BaseOrigin, src.SandboxTip
		report.Commits = reportCommits(src.Commits)
		report.WorktreeDir = src.WorktreeDir
		printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))
	}

	// Plan-free crash recovery: with no parked worktree, patch IDs can prove
	// the whole range already exists on the host as cherry-picked commits.
	// Only an all-match counts, so local-only commits are never mistaken for
	// the discobox's.
	var recognized map[string]string
	if src.Phase == phaseFetched && !hostUnborn {
		if equiv, hostTip, ok, err := gitapply.AlreadyApplied(ctx, repoRoot, report.Base, report.SandboxTip); err != nil {
			return fail("%v", err)
		} else if ok {
			printer.note("every commit in this range is already on %s with identical content; nothing is cherry-picked again", applyTarget(report))
			recognized = equiv
			for i := range report.Commits {
				report.Commits[i].HostCommit = equiv[report.Commits[i].Commit]
			}
			report.HostTip = hostTip
			src.HostTip = hostTip
			if report.HostBase == "" {
				report.HostBase = src.HeadAtPlan
			}
			src.HostBase = report.HostBase
			src.Phase = phaseLanded
			saveApplyPlan(printer, plan)
		}
	}

	if !hostUnborn && src.Phase != phaseLanded {
		printer.note("cherry-picking them in a private worktree, then fast-forwarding local %s", applyTarget(report))
	}

	// Land the range: a retained worktree from an interrupted attempt is
	// inspected first; otherwise a fresh attempt runs in the plan's worktree.
	var (
		result gitapply.Result
		err    error
	)
	if src.Phase != phaseLanded {
		if hostUnborn {
			printer.note("local %s has no commits yet: replaying them onto no history, then creating %s", repoRoot, applyTarget(report))
			wantTree, carried, treeErr := a.createdFromTree(ctx, repoRoot, gitServerURL, projectID, sandboxID, entry.source)
			if treeErr != nil {
				return fail("%v", treeErr)
			}
			result, err = gitapply.AttemptRoot(ctx, repoRoot, report.Base, report.SandboxTip, wantTree, src.WorktreeDir, src.ScratchBranch)
			if err != nil {
				return fail("%v", err)
			}
			if len(result.ChangedPaths) > 0 {
				report.Status = applyStatusBlocked
				report.LocalChanges = result.ChangedPaths
				madeRepository := entry.source.NoLocalRepository.Or(false)
				report.NextSteps = localChangesNextSteps(sandboxID, entry.slug, repoRoot, dirOverride, carried, madeRepository)
				printer.outcome(applyStatusBlocked, "BLOCKED: %s", blockedLocalChanges(repoRoot, carried, madeRepository))
				printer.detailLines(report.LocalChanges)
				printer.nextSteps(report.NextSteps)
				return report, false
			}
		} else {
			headBefore := report.HostBase
			state, resolvedTip, inspectErr := gitapply.InspectWorktree(ctx, repoRoot, src.WorktreeDir, headBefore)
			if inspectErr != nil {
				return fail("%v", inspectErr)
			}
			switch state {
			case gitapply.WorktreeConflict:
				result = gitapply.Result{HostBase: headBefore, ConflictCommit: conflictCommitOf(ctx, src.WorktreeDir), WorktreeDir: src.WorktreeDir}
			case gitapply.WorktreeResolved:
				present, rangeErr := gitapply.ReplayComplete(ctx, repoRoot, report.Base, report.SandboxTip, headBefore, resolvedTip)
				if rangeErr != nil {
					return fail("%v", rangeErr)
				}
				if !present {
					// The sequencer ended with fewer host commits than the
					// range had — a skipped or dropped cherry-pick. Landing
					// this would record the full sandbox tip against a
					// branch missing part of it; the resolution has to be
					// replayed again, not finished from here.
					gitapply.RemoveWorktree(ctx, repoRoot, src.WorktreeDir, "")
					_ = os.RemoveAll(src.WorktreeDir)
					src.Phase = phaseFetched
					src.ConflictCommit = ""
					saveApplyPlan(printer, plan)
					report.Status = applyStatusError
					report.Error = fmt.Sprintf("the conflict resolution in %s finished with fewer commits than the discobox sent; a cherry-pick was skipped or dropped, so nothing was landed", src.WorktreeDir)
					report.NextSteps = []applyNextStep{{
						Description: "start the cherry-pick again in a new retained worktree",
						Commands:    []string{applyResumeCommand(plan.applyPlanIdentity, entry.slug, dirOverride)},
					}}
					printer.outcome(applyStatusError, "ERROR: %s", report.Error)
					printer.nextSteps(report.NextSteps)
					return report, true
				}
				printer.note("the conflict in %s was resolved for all %d %s; landing the finished commits onto local %s", src.WorktreeDir, len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report))
				landed, err := gitapply.FinishResolvedWorktree(ctx, repoRoot, src.WorktreeDir, "", resolvedTip, "", false)
				if err != nil {
					return fail("%v", err)
				}
				result = gitapply.Result{HostBase: headBefore, Landed: true, HostTip: landed}
			case gitapply.WorktreeAborted, gitapply.WorktreeMissing:
				if conflictOpen {
					// The user abandoned (--abort) or lost (prune/delete) a
					// conflict the plan had already parked. Nothing lands this
					// run: say so and put the source back to fetched, so the
					// plain re-run starts the cherry-pick cleanly rather than
					// silently redoing it now.
					what := "aborted"
					if state == gitapply.WorktreeMissing {
						what = "no longer registered"
					}
					gitapply.RemoveWorktree(ctx, repoRoot, src.WorktreeDir, "")
					_ = os.RemoveAll(src.WorktreeDir)
					src.Phase = phaseFetched
					src.ConflictCommit = ""
					saveApplyPlan(printer, plan)
					report.Status = applyStatusError
					report.Error = fmt.Sprintf("the retained apply worktree at %s was %s, so its resolution did not land", src.WorktreeDir, what)
					report.NextSteps = []applyNextStep{{
						Description: "start the cherry-pick again, in a new retained worktree",
						Commands:    []string{applyResumeCommand(plan.applyPlanIdentity, entry.slug, dirOverride)},
					}}
					printer.outcome(applyStatusError, "ERROR: %s", report.Error)
					printer.nextSteps(report.NextSteps)
					return report, true
				}
				// No parked conflict: this is a crashed first attempt whose
				// worktree died with it (or an abort on that first attempt),
				// so retrying it in a clean worktree is the same operation.
				gitapply.RemoveWorktree(ctx, repoRoot, src.WorktreeDir, "")
				_ = os.RemoveAll(src.WorktreeDir)
				result, err = gitapply.Attempt(ctx, repoRoot, report.Base, report.SandboxTip, src.WorktreeDir)
				if err != nil {
					return fail("%v", err)
				}
			}
		}

		if result.HostBase != "" {
			report.HostBase = result.HostBase
			src.HostBase = result.HostBase
		}
		if !result.Landed {
			src.Phase = phaseConflict
			src.ConflictCommit = result.ConflictCommit
			src.WorktreeDir = result.WorktreeDir
			saveApplyPlan(printer, plan)

			report.Status = applyStatusConflict
			report.ConflictCommit = result.ConflictCommit
			report.WorktreeDir = result.WorktreeDir
			report.NextSteps = conflictWorktreeNextSteps(plan.applyPlanIdentity, entry.slug, result.WorktreeDir, dirOverride)
			printer.outcome(applyStatusConflict, "CONFLICT: %s%s did not apply cleanly", shortSHA(report.ConflictCommit), quoteSubject(commitSubject(report.Commits, report.ConflictCommit)))
			printer.noteDetail("the conflict is held open in %s; local %s stays at %s until you resolve it", result.WorktreeDir, applyTarget(report), shortSHA(report.HostBase))
			printer.nextSteps(report.NextSteps)
			return report, true
		}

		report.HostTip = result.HostTip
		src.HostTip = result.HostTip
		if recognized == nil {
			if hostCommits, err := gitutil.Log(ctx, repoRoot, result.HostBase+".."+result.HostTip); err == nil {
				report.Commits = pairHostCommits(report.Commits, hostCommits)
				src.Commits = planCommits(report.Commits)
			}
		}
		src.ConflictCommit = ""
		src.Phase = phaseLanded
		saveApplyPlan(printer, plan)
	}

	// The fast-forward happened (this run, a recognized earlier one, or a
	// resumed landing); confirm it on the discobox before marking the source
	// done.
	_, ok := a.confirmApplied(ctx, printer, client, sandbox, plan.applyPlanIdentity, hostID, repoRoot, entry.slug, report.SandboxTip, report.HostTip, &report)
	if !ok {
		report.Status = applyStatusError
		return report, false
	}
	src.Phase = phaseConfirmed
	saveApplyPlan(printer, plan)

	report.Status = applyStatusApplied
	printer.outcome(applyStatusApplied, "APPLIED %d %s to local %s", len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report))
	printer.appliedList(report.Commits)
	printer.landed(report)
	printer.note("recorded on discobox %s as applied to %s", sandboxID, repoRoot)
	return report, false
}

// confirmApplied posts the completion record for a landing already on the
// host branch and reads it back from the response, so "confirmed" means the
// exact tuple is verifiable — never just that the POST returned. It is
// idempotent server-side, so the interrupted-run retry and the first call
// take the same path.
func (a *App) confirmApplied(ctx context.Context, printer applyPrinter, client *apiclientgen.Client, sandbox *apimodel.Sandbox,
	identity applyPlanIdentity, hostID, repoRoot, slug, sandboxTip, hostTip string, report *applySourceReport) (*apimodel.Sandbox, bool) {
	projectID, sandboxID := identity.ProjectID, identity.SandboxID
	res, err := client.CompleteSandboxApply(ctx, &apimodel.CompleteSandboxApplyBody{
		Slug:       slug,
		Commit:     sandboxTip,
		HostCommit: hostTip,
		HostId:     hostID,
		HostPath:   repoRoot,
	}, apiclientgen.CompleteSandboxApplyParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		printer.outcome(applyStatusError, "ERROR: commits landed in %s at %s, but recording the apply on the discobox failed: %v", repoRoot, shortSHA(hostTip), err)
		printer.note("the commits are safely on the branch; run discobox apply again and only the recording is retried")
		report.Error = fmt.Sprintf("commits landed in %s at %s, but recording the apply on the discobox failed: %v", repoRoot, shortSHA(hostTip), err)
		report.NextSteps = []applyNextStep{{
			Description: "retry only the recording",
			Commands:    []string{applyResumeCommand(identity, slug, report.dirOverride)},
		}}
		return nil, false
	}
	updated, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		printer.outcome(applyStatusError, "ERROR: the apply landed at %s, but the discobox's response could not be read: %v", shortSHA(hostTip), err)
		printer.note("run discobox apply again; the recording is idempotent and nothing is applied twice")
		report.Error = fmt.Sprintf("the apply landed at %s, but the discobox's response could not be read: %v", shortSHA(hostTip), err)
		return nil, false
	}
	if !appliedCommitRecorded(updated, slug, sandboxTip, hostTip) {
		printer.outcome(applyStatusError, "ERROR: commits landed in %s at %s, but the discobox did not record them", repoRoot, shortSHA(hostTip))
		report.Error = fmt.Sprintf("commits landed in %s at %s, but the discobox did not record them", repoRoot, shortSHA(hostTip))
		return nil, false
	}
	*sandbox = *updated
	report.HostTip = hostTip
	return updated, true
}

// appliedCommitRecorded verifies the exact tuple came back on the sandbox.
func appliedCommitRecorded(sandbox *apimodel.Sandbox, slug, commit, hostCommit string) bool {
	entries, ok := sandbox.Runtime.AppliedCommits.Get()
	if !ok {
		return false
	}
	for _, entry := range entries {
		if entry.Slug == slug && entry.Commit == commit && entry.HostCommit == hostCommit {
			return true
		}
	}
	return false
}

// planStaleness gathers the target's current state and asks the plan source
// whether resuming it is still valid. Terminal (landed or later) sources
// return no staleness: applied work with commits on top is expected.
func (a *App) planStaleness(ctx context.Context, printer applyPrinter, ps *preparedSource, src *applyPlanSource, allowDirty bool) applyStale {
	if src.Phase == phaseLanded || src.Phase == phaseConfirmed || src.Phase == phaseUpToDate {
		return applyStale{}
	}
	info, statErr := os.Stat(ps.repoRoot)
	if statErr != nil || !info.IsDir() {
		return applyStale{Kind: applyStaleTargetReplaced, Remedy: applyStaleRemedyDir,
			Detail: fmt.Sprintf("the local directory %s recorded for source %q is gone", ps.repoRoot, src.Slug)}
	}
	state := applyRepoState{Exists: true}
	// Identity object: the planned HEAD when the target had commits,
	// otherwise the fetched discobox base. Either must still be present in
	// this repository.
	identity := src.HeadAtPlan
	if identity == "" {
		identity = src.Base
	}
	if head, err := gitutil.ResolveCommit(ctx, ps.repoRoot, "HEAD"); err == nil {
		state.CurrentHEAD = head
	}
	switch {
	case identity != "" && !hostCommitExists(ctx, ps.repoRoot, identity):
		state.SameRepository = false
	case state.CurrentHEAD == "" && identity != "":
		// A repository with commits the plan knew is now unreadable/unborn:
		// something else holds the directory.
		state.SameRepository = false
	default:
		// The repository is the right one; an unborn target (no identity yet)
		// and a moved working tree are still observable here.
		state.SameRepository = true
		if dirty, err := hostWorktreeDirty(ctx, ps.repoRoot); err == nil {
			state.Dirty = dirty
		}
	}
	stale := src.staleness(state)
	if stale.Kind == applyStaleDirtyWorktree && allowDirty {
		printer.caution("--allow-dirty: resuming despite the local uncommitted changes in %s", ps.repoRoot)
		return applyStale{}
	}
	return stale
}

// refuseStalePlan renders and records a stale-plan refusal and ends the run
// without a Git change. A branch that advanced retires the plan itself; the
// other two keep it for the explicit override to act on.
func (a *App) refuseStalePlan(ctx context.Context, printer applyPrinter, plan *applyPlan, src *applyPlanSource, stale applyStale, report *applySourceReport) (applySourceReport, bool) {
	report.Status = applyStatusError
	report.PlanSuperseded = stale.Remedy == applyStaleRemedyRerun
	report.Error = stale.Detail
	switch stale.Remedy {
	case applyStaleRemedyDir:
		report.NextSteps = []applyNextStep{{
			Description: fmt.Sprintf("point this apply at the repository source %q should land in now", src.Slug),
			Commands:    []string{applyResumeCommand(plan.applyPlanIdentity, src.Slug, applyDirPlaceholder)},
		}}
	case applyStaleRemedyAllowDirty:
		report.NextSteps = []applyNextStep{{
			Description: "acknowledge the local uncommitted changes and resume the apply",
			Commands:    []string{applyResumeCommand(plan.applyPlanIdentity, src.Slug, report.resumeDirOverride(), "--allow-dirty")},
		}}
	case applyStaleRemedyRerun:
		// Nothing else is applied over a moved branch: retire the plan in
		// this run, so a plain re-run with the same selection plans from the
		// current HEAD; sources already confirmed are recognized by the
		// server-side baseline.
		discardPlanArtifacts(ctx, printer, plan)
		_ = plan.remove()
		report.NextSteps = []applyNextStep{{
			Description: "re-run the apply; it plans from the current branch and does not repeat anything already applied",
			Commands:    []string{applyResumeCommand(plan.applyPlanIdentity, src.Slug, report.resumeDirOverride())},
		}}
	}
	printer.outcome(applyStatusError, "STALE PLAN: %s", stale.Detail)
	printer.nextSteps(report.NextSteps)
	return *report, true
}

// conflictCommitOf reads CHERRY_PICK_HEAD from a retained worktree; empty
// when it cannot be read, which the conflict report renders without a SHA.
func conflictCommitOf(ctx context.Context, worktreeDir string) string {
	out, err := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "CHERRY_PICK_HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// hostCommitExists reports whether sha is a commit object in repoRoot.
func hostCommitExists(ctx context.Context, repoRoot, sha string) bool {
	if sha == "" {
		return false
	}
	_, err := gitutil.Output(ctx, repoRoot, nil, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// hostWorktreeDirty reports whether the host repository's working tree has
// uncommitted changes — tracked changes or untracked files, the same
// question `git status --porcelain` answers for a human.
func hostWorktreeDirty(ctx context.Context, repoRoot string) (bool, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// dirOverrideFor is the --dir value a re-run for slug needs carried through,
// "" when the source resolves its own directory.
func dirOverrideFor(slug string, dirOverrides map[string]string) string {
	if dir, ok := dirOverrides[slug]; ok {
		return dir
	}
	return ""
}

// conflictWorktreeNextSteps are the literal ways out of a retained conflict:
// finish the cherry-pick in the private worktree, then re-run apply to land
// it and continue with the sources that waited. The resume command rebuilds
// the run's original selection — never a narrower --source one, which would
// supersede this plan and delete the worktree being resolved.
func conflictWorktreeNextSteps(identity applyPlanIdentity, slug, worktreeDir, dir string) []applyNextStep {
	return []applyNextStep{
		{
			Description: fmt.Sprintf("resolve the conflict in the retained worktree at %s", worktreeDir),
			Commands: []string{
				fmt.Sprintf("git -C %s status", worktreeDir),
				fmt.Sprintf("git -C %s add -u", worktreeDir),
				fmt.Sprintf("git -C %s cherry-pick --continue", worktreeDir),
			},
		},
		{
			Description: "land the resolved commits and continue with the remaining sources",
			Commands:    []string{applyResumeCommand(identity, slug, dir)},
		},
	}
}

// planCommits converts report commits into their durable plan form, and back.
func planCommits(commits []applyCommit) []applyPlanCommit {
	out := make([]applyPlanCommit, 0, len(commits))
	for _, commit := range commits {
		out = append(out, applyPlanCommit{SHA: commit.Commit, Subject: commit.Subject, Author: commit.Author, Date: commit.Date})
	}
	return out
}

func reportCommits(commits []applyPlanCommit) []applyCommit {
	out := make([]applyCommit, 0, len(commits))
	for _, commit := range commits {
		out = append(out, applyCommit{Commit: commit.SHA, Subject: commit.Subject, Author: commit.Author, Date: commit.Date})
	}
	return out
}

// newErrorSourceReport renders an error for a target that could not even be
// opened as a source.
func newErrorSourceReport(printer applyPrinter, slug, format string, args ...any) applySourceReport {
	report := applySourceReport{Slug: slug, Status: applyStatusError, Error: fmt.Sprintf(format, args...)}
	printer.outcome(applyStatusError, "ERROR: %s", report.Error)
	return report
}

// resumeDirOverride is the --dir value a resume of this source carries: the
// PATH placeholder the user still has to fill in when the source was never
// placed, or the explicit directory this run used.
func (r *applySourceReport) resumeDirOverride() string {
	if r.dirOverride != "" {
		return r.dirOverride
	}
	if r.HostPathError != "" {
		return applyDirPlaceholder
	}
	return ""
}

// pendingSourceReport is the report shape a source gets when an earlier
// source stopped the run before this one was touched. The same object in text
// and JSON.
func pendingSourceReport(ps *preparedSource) applySourceReport {
	report := applySourceReport{Slug: ps.entry.slug, Status: applyStatusPending}
	if ps.placed {
		report.HostPath = ps.repoRoot
		report.HostPathOrigin = ps.dirOrigin
	}
	return report
}

// initApplyRepository makes a repository in dir, a directory a discobox was
// created from that holds none, and returns its root. HEAD is left unborn on
// the branch the discobox was created on, so the first apply into it is the
// one that gives it its history (ADR 0084) and its branch is the one the
// discobox's work is on.
//
// It refuses a directory that already has a .git of any kind. Git says "not
// a git repository" of a .git it cannot read — HEAD missing, say — and
// `git init` then reuses it, objects and refs and all; a repository apply did
// not make is never one it may remove again.
func initApplyRepository(ctx context.Context, dir string, source apimodel.GitSource) (string, error) {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", fmt.Errorf("check %s for a .git before making a repository there: %w", dir, err)
		}
		return "", fmt.Errorf("%s has a .git that Git does not read as a repository; apply will not make one over it", dir)
	}
	args := []string{"init", "--quiet"}
	if checkout, ok := source.Checkout.Get(); ok && checkout.RefType.Or("") == "branch" {
		if branch := strings.TrimSpace(checkout.RefName.Or("")); branch != "" {
			args = append(args, "--initial-branch="+branch)
		}
	}
	if _, err := gitutil.Output(ctx, dir, nil, nil, args...); err != nil {
		return "", fmt.Errorf("make a Git repository in %s to apply into: %w", dir, err)
	}
	root, err := gitutil.Root(ctx, dir)
	if err != nil {
		_ = os.RemoveAll(filepath.Join(dir, ".git"))
		return "", err
	}
	return root, nil
}

// removeApplyRepositoryUnlessBorn takes away the repository initApplyRepository
// made at root, unless its branch now holds a commit — and reports whether it
// kept it. The branch is what decides, not whether the apply reported landing:
// AttemptRoot creates the branch before it checks the commits out, so an apply
// that fails or is interrupted between the two has still put the commits there,
// and removing the repository would lose them with the working tree already
// half replaced. The check ignores cancellation for the same reason; if it
// cannot tell, the repository stays.
func removeApplyRepositoryUnlessBorn(ctx context.Context, root string) (bool, error) {
	if !gitunborn.HeadIsUnborn(context.WithoutCancel(ctx), root) {
		return true, nil
	}
	return false, os.RemoveAll(filepath.Join(root, ".git"))
}

// resolveApplyBase chooses the exclusive end of the sandbox commit range.
// A prior apply is the narrowest answer while its sandbox-side commit remains
// in the current history. A rebase can rewrite that commit away; in that case
// it is a stale cursor, so the range is derived from the histories that exist
// now. Repositories created without commits retain their explicit empty base,
// because their histories need not share an ancestor (ADR 0084).
func resolveApplyBase(ctx context.Context, repoRoot, tip, lastCommit, discoboxBase string) (string, baseOrigin, error) {
	if lastCommit != "" && gitapply.IsAncestor(ctx, repoRoot, lastCommit, tip) {
		return lastCommit, baseOriginLastApplied, nil
	}
	if discoboxBase != "" {
		return discoboxBase, baseOriginDiscoboxBase, nil
	}
	base, err := gitapply.MergeBase(ctx, repoRoot, tip)
	if err != nil {
		return "", "", err
	}
	return base, baseOriginMergeBase, nil
}

// sandboxDirtyBlocks makes the discobox-side working-tree check that comes
// before either apply path decides anything, records what it found on the
// report, and says whether that ends the source.
//
// Both paths make it and both say the same thing about it, because it is the
// same fact about the same discobox: uncommitted work is never applied, and a
// source is not reported as drained while some of it is still sitting there.
// Where the two differ is only the re-run they print — a source with no local
// directory needs a --dir on it — which is what dir carries.
func (a *App) sandboxDirtyBlocks(ctx context.Context, printer applyPrinter, projectID, sandboxID string, report *applySourceReport, allowDirty bool, dir string) (bool, error) {
	if report.SandboxDir == "" {
		// The discobox never said where this source landed, so there is no
		// working tree to ask about.
		return false, nil
	}
	printer.note("checking the discobox working tree (git status --porcelain)")
	dirty, status, err := a.sandboxSourceDirty(ctx, projectID, sandboxID, report.SandboxDir)
	if err != nil {
		return false, err
	}
	switch {
	case dirty && !allowDirty:
		report.Status = applyStatusBlocked
		report.UncommittedChanges = statusLines(status)
		report.NextSteps = dirtyNextSteps(sandboxID, report.Slug, report.SandboxDir, dir)
		printer.outcome(applyStatusBlocked, "BLOCKED: the discobox has %d uncommitted %s; only committed work is applied",
			len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
		printer.detailLines(report.UncommittedChanges)
		printer.nextSteps(report.NextSteps)
		return true, nil
	case dirty:
		// --allow-dirty. The uncommitted work is still listed: it stays in
		// the sandbox, and the whole point of the flag is that the user
		// chose to leave it there rather than not knowing about it.
		report.UncommittedChanges = statusLines(status)
		report.DirtyIgnored = true
		printer.caution("--allow-dirty: applying anyway; %d uncommitted %s stay in the discobox and are not applied",
			len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
		printer.detailLines(report.UncommittedChanges)
	default:
		printer.noteDetail("clean, nothing uncommitted")
	}
	return false, nil
}

// unplacedSource ends a source whose local directory could not be resolved:
// a discobox created on another machine, a source cloned from a remote rather
// than pushed from a checkout here, or a directory that has since moved.
//
// Not knowing where a source would land only costs anything when it has
// commits to land. A source cloned from a remote and left alone in the
// discobox has nothing to place, and failing over it would fail every apply of
// every discobox that carries one, forever — so the discobox's tip is read
// straight off its repository, which ls-remote does without a local clone, and
// compared against the commit this source is already accounted for at: the
// last apply's, or the commit it was created at. Those match only when the
// discobox has committed nothing here, and that is the one case where an
// unknown directory is not a failure.
func (a *App) unplacedSource(ctx context.Context, printer applyPrinter, projectID, sandboxID string, sandbox *apimodel.Sandbox, gitServerURL string, entry applySourceEntry, allowDirty bool, dirErr error) applySourceReport {
	report := applySourceReport{
		Slug:          entry.slug,
		Status:        applyStatusError,
		SandboxDir:    sourceWorkdir(entry.source),
		SandboxRef:    sandboxapply.FetchRef(sandboxID, entry.slug),
		HostPathError: dirErr.Error(),
	}
	// The heading names the discobox side, which is the half of this source
	// that is known, and says in the local repo's own row why there is no
	// local repo — before anything else happens, because everything that
	// follows may hand the reader a --dir to fill in, and none of it explains
	// itself without this. Status is not decided by it: a discobox that has
	// committed nothing to this source needs no directory at all.
	printer.sourceHeader(report)
	fail := func() applySourceReport {
		report.Status = applyStatusError
		printer.outcome(applyStatusError, "ERROR: %s", report.Error)
		printer.nextSteps(report.NextSteps)
		return report
	}

	// The same working-tree check every other route makes, and for the same
	// reason: a source is never reported as having nothing to apply while
	// uncommitted work sits in it. The re-run it prints carries the --dir this
	// source will need once that work is committed.
	blocked, err := a.sandboxDirtyBlocks(ctx, printer, projectID, sandboxID, &report, allowDirty, applyDirPlaceholder)
	if err != nil {
		report.Error = fmt.Sprintf("%v (and its working tree could not be checked: %v)", dirErr, err)
		return fail()
	}
	if blocked {
		return report
	}

	report.Base, report.BaseOrigin = unplacedSourceBase(sandbox, entry)
	if report.Base == "" {
		// Nothing records the commit this source started from — a source
		// created from a URL and a branch name, never applied since, has only
		// the branch name — so no tip can be measured against anything and
		// whether there is work here cannot be told. Saying that is the honest
		// answer; assuming there was none would drop the discobox's commits on
		// the floor.
		report.Error = fmt.Sprintf("%v (and nothing records the commit it started from, so whether it has anything to apply cannot be told)", dirErr)
		return fail()
	}

	printer.note("reading the discobox's tip (git ls-remote), to see whether it has anything that needs one")
	tip, err := sandboxapply.Tip(ctx, gitServerURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		report.Error = fmt.Sprintf("%v (and whether it has anything to apply could not be checked: %v)", dirErr, err)
		return fail()
	}
	report.SandboxTip = tip
	printer.noteDetail("discobox tip %s", shortSHA(tip))
	printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

	if tip == report.Base {
		report.Status = applyStatusUpToDate
		printer.outcome(applyStatusUpToDate, "UP TO DATE: the discobox has no commits after %s, so there is nothing to apply and nothing needs a local directory here", shortSHA(report.Base))
		return report
	}
	report.Error = fmt.Sprintf("the discobox has commits after %s to apply, but %v", shortSHA(report.Base), dirErr)
	report.NextSteps = []applyNextStep{{
		Description: fmt.Sprintf("apply them into a local checkout of the repository source %q came from", entry.slug),
		Commands:    []string{applyRerun(sandboxID, entry.slug, applyDirPlaceholder)},
	}}
	return fail()
}

// unplacedSourceBase is the commit a source with no local directory is already
// accounted for at, and where that commit came from: the last apply of this
// source, or failing that the commit the source was created at. Anything the
// discobox has committed after it is work that would be applied, and is what
// makes an unknown local directory matter.
//
// It is deliberately not a merge base: there is no local repository here to
// take one against, which is the situation that got us here. For the same
// reason it cannot make resolveApplyBase's check that a recorded last-applied
// commit is still an ancestor of the tip — a rebase in the discobox can leave
// that cursor stale, and IsAncestor needs the objects. A stale one simply
// fails to match the tip, so the source is reported as having work to place
// and asks for a --dir; erring towards "there is something here" is the safe
// way to be wrong when the answer cannot be checked.
func unplacedSourceBase(sandbox *apimodel.Sandbox, entry applySourceEntry) (string, baseOrigin) {
	if last, ok := lastApplied(sandbox, entry.slug); ok && last.Commit != "" {
		return last.Commit, baseOriginLastApplied
	}
	if commit := checkoutCommit(entry.source); commit != "" {
		return commit, baseOriginSourceCheckout
	}
	return "", ""
}

// checkoutCommit is the commit a source was created against, which for a
// discobox created from a repository with no commits is the empty base commit
// its whole history hangs off.
func checkoutCommit(source apimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(checkout.Commit.Or(""))
}

// createdFromTree is the tree the discobox was created from: the workspace
// snapshot it carried, or the empty tree when it carried nothing. The second
// return says which of those it was, because a working tree that no longer
// matches means something different in each case.
//
// It is what a local repository with no commits must still hold for its first
// apply to be safe, so a missing ref must not read as a changed working tree.
// The ref is resolved out of the repository first, where create wrote it, and
// fetched from the discobox's own origin if it is gone from here — delivery
// pushed it there, so the discobox is the second copy of the answer.
func (a *App) createdFromTree(ctx context.Context, repoRoot, gitServerURL, projectID, sandboxID string, source apimodel.GitSource) (string, bool, error) {
	ref := ""
	if workspace, ok := source.Workspace.Get(); ok {
		ref = strings.TrimSpace(workspace.SnapshotRef.Or(""))
	}
	if ref == "" {
		// Nothing carried, so the discobox was created from an empty
		// working tree and that is what this one has to still be.
		tree, err := gitutil.EmptyTree(ctx, repoRoot)
		return tree, false, err
	}
	if tree, err := resolveTree(ctx, repoRoot, ref); err == nil {
		return tree, true, nil
	}
	if err := sandboxapply.Fetch(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, source, "+"+ref+":"+ref); err != nil {
		return "", true, fmt.Errorf("fetch what the discobox was created from (%s), to check nothing here has changed since: %w", ref, err)
	}
	tree, err := resolveTree(ctx, repoRoot, ref)
	return tree, true, err
}

func resolveTree(ctx context.Context, repoRoot, rev string) (string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", rev+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve tree of %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// blockedLocalChanges says why the local working tree is not what the discobox
// was created from. There are two ways to get here and they are different
// things to have done: the user changed a working tree the discobox was
// given, or the user told it not to carry that working tree in the first
// place (--include-dirty=false, or a repository that gained files after the
// create). Telling the second one their repository "has changed" accuses them
// of something they did not do.
//
// noRepository says the directory had no repository until this apply made one,
// which it has taken away again, so there is not even an empty history there
// to keep anything in.
func blockedLocalChanges(repoRoot string, carried, noRepository bool) string {
	keep := "has no commits to keep"
	if noRepository {
		keep = "is not a Git repository to keep"
	}
	if carried {
		return fmt.Sprintf("local %s has changed since this discobox was created, and %s those changes in", repoRoot, keep)
	}
	return fmt.Sprintf("local %s holds files this discobox was never given, and %s them in", repoRoot, keep)
}

// localChangesNextSteps is the way out of a first apply refused because the
// local working tree is not what the discobox was created from. Committing the
// local work is the one that keeps it, and it is the same answer either way:
// the range being applied never depended on a shared history, so it
// cherry-picks onto whatever the user commits just as well.
//
// The alternative is not the same answer. Undoing an edit puts a carried
// working tree back the way the discobox found it; doing that to files the
// discobox was never given would mean deleting them, which is nobody's idea
// of a way out.
//
// A directory that had no repository until this apply made one — and took it
// away again — needs one before anything can be committed in it, so both
// ways out start by making one.
func localChangesNextSteps(sandboxID, slug, repoRoot, dir string, carried, noRepository bool) []applyNextStep {
	rerun := applyRerun(sandboxID, slug, dir)
	alternative := "or look at what changed, and put it back the way the discobox found it"
	lookCommands := []string{fmt.Sprintf("git -C %s status", repoRoot)}
	if !carried {
		alternative = "or look at what is here, and move aside anything the discobox's commits would land on"
	}
	var commitCommands []string
	if noRepository {
		commitCommands = append(commitCommands, fmt.Sprintf("git -C %s init", repoRoot))
		lookCommands = []string{fmt.Sprintf("ls -A %s", repoRoot)}
	}
	commitCommands = append(commitCommands,
		fmt.Sprintf("git -C %s add -A", repoRoot),
		fmt.Sprintf("git -C %s commit -m MESSAGE", repoRoot),
		rerun,
	)
	return []applyNextStep{
		{
			Description: "commit the local files first, then apply the discobox's commits on top of them",
			Commands:    commitCommands,
		},
		{
			Description: alternative,
			Commands:    lookCommands,
		},
	}
}

// applyFrom names where the commits landed from, for a line that reads as a
// range. A repository getting its first commits has no "from" commit to name.
func applyFrom(report applySourceReport) string {
	if report.HostBase == "" {
		return "no commits"
	}
	return shortSHA(report.HostBase)
}

// applyTarget names what the commits land on locally: the branch when there is
// one, the repository path when HEAD is detached.
func applyTarget(report applySourceReport) string {
	if report.HostBranch != "" {
		return report.HostBranch
	}
	return report.HostPath + " (detached HEAD)"
}

func commitSubject(commits []applyCommit, sha string) string {
	for _, commit := range commits {
		if commit.Commit == sha {
			return commit.Subject
		}
	}
	return ""
}

// applyDirPlaceholder stands in for the local directory in a re-run printed for
// a source that has none: the user is the only one who knows which checkout it
// should be, and the command is printed to be filled in and run.
const applyDirPlaceholder = "PATH"

// applyRerun is the command that runs this apply again FOR ONE SOURCE, used
// where the guidance is genuinely per-source (a dirty discobox, an unplaced
// source): it deliberately narrows to --source. dir is the local directory to
// name in a --dir: the one the caller passed, or the PATH placeholder for a
// source that cannot be re-run without being given one. Empty means the
// source resolves its own directory.
//
// Guidance that resumes a multi-source plan must not use this: a narrower
// selection supersedes the plan and discards its retained worktree. Use
// applyResumeCommand there.
func applyRerun(sandboxID, slug, dir string) string {
	rerun := fmt.Sprintf("discobox apply %s --source %s", sandboxID, slug)
	if dir != "" {
		rerun += fmt.Sprintf(" --dir %s=%s", slug, dir)
	}
	return rerun
}

// applyResumeCommand rebuilds the command that resumes an existing plan with
// its ORIGINAL selection — exactly enough to match its fingerprint: no
// --source unless the plan narrowed to one, every --dir the plan carried
// (the remedy's dir replaces the one it re-points), and --allow-dirty when
// the plan had it. Anything fewer changes the fingerprint, supersedes the
// plan, and deletes a conflict worktree the user already resolved — never on
// a resume. extraFlags appends flags the remedy itself adds (such as a new
// --allow-dirty acknowledgment).
func applyResumeCommand(identity applyPlanIdentity, slug, dir string, extraFlags ...string) string {
	cmd := "discobox apply " + identity.SandboxID
	if identity.OnlySlug != "" {
		cmd += " --source " + identity.OnlySlug
	}
	overrides := make(map[string]string, len(identity.DirOverrides)+1)
	for k, v := range identity.DirOverrides {
		overrides[k] = v
	}
	if dir != "" {
		overrides[slug] = dir
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd += fmt.Sprintf(" --dir %s=%s", k, overrides[k])
	}
	if identity.AllowDirty {
		cmd += " --allow-dirty"
	}
	for _, flag := range extraFlags {
		cmd += " " + flag
	}
	return cmd
}

// dirtyNextSteps is the two ways out of a dirty sandbox working tree: commit
// the work there and apply it too, or apply only what is already committed and
// leave the rest. Both re-runs are spelled out for this exact source, --dir
// override included, so neither has to be reassembled by hand.
func dirtyNextSteps(sandboxID, slug, sandboxDir, dir string) []applyNextStep {
	rerun := applyRerun(sandboxID, slug, dir)
	return []applyNextStep{
		{
			Description: "commit them in the discobox, then apply again",
			Commands: []string{
				fmt.Sprintf("discobox shell %s -- git -C %s commit -a -m MESSAGE", sandboxID, sandboxDir),
				rerun,
			},
		},
		{
			Description: "or apply only what is already committed, leaving them in the discobox",
			Commands:    []string{rerun + " --allow-dirty"},
		},
	}
}

// statusLines splits `git status --porcelain` output into its entries, keeping
// the two-column status prefix that says what happened to each path.
func statusLines(status string) []string {
	var lines []string
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	}
	return lines
}

// resolveApplyHostDir picks the local directory a source applies into: an
// explicit --dir override, or the source's own LocalDirectory when this
// machine is the one the sandbox was created from.
//
// Applying into a directory the sandbox did not come from would cherry-pick
// one repository's commits onto another, so the identity check is the gate:
// the sandbox's origin host must be this host, that source must have recorded
// where it came from, and that directory must still be there. Each of those
// fails for a different reason and says which, because "pass --dir" is only
// actionable once you know whether the sandbox is from another machine, was
// cloned from a remote, or simply moved.
func resolveApplyHostDir(sandbox *apimodel.Sandbox, hostID string, entry applySourceEntry, dirOverrides map[string]string) (string, hostDirOrigin, error) {
	if dir, ok := dirOverrides[entry.slug]; ok {
		return dir, hostDirFromOverride, nil
	}
	needDir := fmt.Sprintf("pass --dir %s=PATH", entry.slug)

	origin, hasOrigin := sandbox.Origin.Get()
	if !hasOrigin {
		return "", "", fmt.Errorf("the discobox has no recorded origin, so nothing says which directory it came from; %s", needDir)
	}
	if origin.HostId != hostID {
		return "", "", fmt.Errorf("the discobox was created on a different machine (origin host %s%s, this machine is %s); %s",
			origin.HostId, formatOriginHostname(origin), hostID, needDir)
	}

	local := strings.TrimSpace(entry.source.LocalDirectory.Or(""))
	if local == "" {
		return "", "", fmt.Errorf("source %q has no local directory recorded — it was cloned from a remote rather than pushed from a checkout here; %s", entry.slug, needDir)
	}
	info, err := os.Stat(local)
	if err != nil {
		return "", "", fmt.Errorf("the directory source %q came from, %s, is not readable on this machine (%w); %s", entry.slug, local, err, needDir)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("the path source %q came from, %s, is no longer a directory; %s", entry.slug, local, needDir)
	}
	return local, hostDirFromSandboxOrigin, nil
}

// formatOriginHostname adds the origin machine's display hostname when the
// sandbox recorded one. The host ID alone identifies the machine but tells
// nobody which machine it was.
func formatOriginHostname(origin apimodel.Origin) string {
	if hostname := strings.TrimSpace(origin.Hostname.Or("")); hostname != "" {
		return " " + strconv.Quote(hostname)
	}
	return ""
}

// sandboxSourceDirty reports whether a source's working tree inside the
// sandbox has uncommitted changes, via `git status --porcelain` over the exec
// API. Fetching a source's commits only sees committed history (ADR 0014
// §3): this is the one thing that requires exec instead.
func (a *App) sandboxSourceDirty(ctx context.Context, projectID, sandboxID, workdir string) (bool, string, error) {
	stdout, stderr, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, workdir, []string{"git", "status", "--porcelain"})
	if err != nil {
		return false, "", err
	}
	if code != 0 {
		return false, "", fmt.Errorf("git status: %s", strings.TrimSpace(stderr+stdout))
	}
	return strings.TrimSpace(stdout) != "", stdout, nil
}

func shortSHA(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

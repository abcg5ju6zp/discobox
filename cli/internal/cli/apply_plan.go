package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A multi-source apply is a resumable per-source plan (ADR 26-10-15-223).
// Everything one run intended — which discobox, the sources in their order,
// each source's base and tip, its target directory and the local HEAD it
// started from — is recorded in one private file before any local mutation,
// and rewritten after every per-source phase transition. A re-run after a
// kill, a conflict, or a lost confirmation reads this instead of guessing
// from merge-bases, so commits that already landed are neither replayed nor
// confused with the user's own new commits.

const applyPlanSchemaVersion = 1

// applyPhase is a source's position in the plan's state machine.
type applyPhase string

const (
	// phasePlanned: the source's target and range are recorded, but its
	// discobox tip has not been fetched into the target repository yet.
	phasePlanned applyPhase = "planned"
	// phaseFetched: the tip is fetched and the range is known; the
	// cherry-pick has either not started or is safe to (re)attempt.
	phaseFetched applyPhase = "fetched"
	// phaseConflict: the cherry-pick is parked mid-conflict in the retained
	// worktree, waiting for the user to resolve it there.
	phaseConflict applyPhase = "conflict"
	// phaseLanded: the host branch holds the applied commits; the
	// server-side confirmation has not yet been read back.
	phaseLanded applyPhase = "landed"
	// phaseConfirmed: the host commits landed and complete-apply recorded
	// the exact tuple. Terminal.
	phaseConfirmed applyPhase = "confirmed"
	// phaseUpToDate: the discobox had no commits past the base. Nothing was
	// landed or confirmed, and nothing needs resuming. Terminal.
	phaseUpToDate applyPhase = "up-to-date"
)

// terminalApplyPhase reports whether nothing remains to do for a source.
func terminalApplyPhase(phase applyPhase) bool {
	return phase == phaseConfirmed || phase == phaseUpToDate
}

// applyStrategy is how a source's range lands on its target.
type applyStrategy string

const (
	strategyCherryPick applyStrategy = "cherry-pick"
	strategyRoot       applyStrategy = "root" // ADR 0084/0139: unborn host branch
)

// applyPlanSelection is the command-line selection a run was started with.
// It is the plan's fingerprint: re-running with a different selection acts on
// a different intent, so the old plan is superseded rather than resumed.
type applyPlanSelection struct {
	OnlySlug     string            `json:"onlySlug,omitempty"`
	DirOverrides map[string]string `json:"dirOverrides,omitempty"`
	AllowDirty   bool              `json:"allowDirty"`
}

func (s applyPlanSelection) equal(other applyPlanSelection) bool {
	if s.OnlySlug != other.OnlySlug || s.AllowDirty != other.AllowDirty {
		return false
	}
	if len(s.DirOverrides) != len(other.DirOverrides) {
		return false
	}
	for key, value := range s.DirOverrides {
		if other.DirOverrides[key] != value {
			return false
		}
	}
	return true
}

// applyPlanIdentity pins the plan to one server and one discobox on it.
type applyPlanIdentity struct {
	Server    string `json:"server"`
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	applyPlanSelection
}

// applyPlanCommit is one sandbox commit in a source's planned range.
type applyPlanCommit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date,omitempty"`
}

// applySourceKind is how a source was classified for the run. The plan
// records every selected source, not only the ones with a local target, so a
// resume cannot silently drop a source that had no directory or a broken
// target.
type applySourceKind string

const (
	sourceKindPlaced   applySourceKind = "placed"
	sourceKindUnplaced applySourceKind = "unplaced"
	sourceKindBroken   applySourceKind = "broken"
)

// applyPlanSource is one source's recorded state.
type applyPlanSource struct {
	Slug  string     `json:"slug"`
	Phase applyPhase `json:"phase"`
	// Kind defaults to placed: plans written by versions that only recorded
	// placed sources, and every normal source, are exactly that.
	Kind     applySourceKind `json:"kind,omitempty"`
	Strategy applyStrategy   `json:"strategy"`

	// The host target as resolved at planning time.
	RepoRoot      string        `json:"repoRoot"`
	HostDirOrigin hostDirOrigin `json:"hostDirOrigin,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	// HeadAtPlan is the target's HEAD before this source lands anything. A
	// resume fast-forwards from exactly this commit, so a different HEAD
	// means the world the plan named moved on.
	HeadAtPlan string `json:"headAtPlan"`

	// The discobox-side range.
	Base       string            `json:"base"`
	BaseOrigin baseOrigin        `json:"baseOrigin,omitempty"`
	SandboxTip string            `json:"sandboxTip"`
	Commits    []applyPlanCommit `json:"commits,omitempty"`

	// The retained worktree a conflict is resolved in, and the scratch
	// branch that worktree's HEAD is on.
	WorktreeDir    string `json:"worktreeDir,omitempty"`
	ScratchBranch  string `json:"scratchBranch,omitempty"`
	ConflictCommit string `json:"conflictCommit,omitempty"`

	// The host landing: HEAD before and the tip after the fast-forward.
	HostBase string `json:"hostBase,omitempty"`
	HostTip  string `json:"hostTip,omitempty"`

	// NoRepository says apply made the target's repository itself (ADR
	// 0139); the remove-it-unless-born cleanup stays keyed off that.
	NoRepository bool `json:"noRepository,omitempty"`
}

// applyPlan is the persisted intent and progress of one apply run.
type applyPlan struct {
	SchemaVersion int `json:"schemaVersion"`
	applyPlanIdentity
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
	Sources   []*applyPlanSource `json:"sources"`
}

// Plan storage layout under the CLI state dir:
//
//	…/cli/apply/<sandbox-id>/plan.json
//	…/cli/apply/<sandbox-id>/lock
//	…/cli/apply/<sandbox-id>/worktrees/<slug>/   (retained conflict worktree)
func applyPlanDir(sandboxID string) string {
	return filepath.Join(cliStateDir(), "apply", sandboxID)
}

func applyPlanFilePath(sandboxID string) string {
	return filepath.Join(applyPlanDir(sandboxID), "plan.json")
}

// applyWorktreeDir is the persistent linked worktree one source's attempt
// lives in, conflict or not. It is under the plan directory, never in the
// user's checkout.
func applyWorktreeDir(sandboxID, slug string) string {
	return filepath.Join(applyPlanDir(sandboxID), "worktrees", slug)
}

// applyScratchBranch is the branch the retained worktree's HEAD is on. It
// carries the discobox id so two discoboxes applied into one repository never
// collide on one branch name.
func applyScratchBranch(sandboxID, slug string) string {
	return "discobox-apply/" + sandboxID + "/" + slug
}

var (
	// errNoApplyPlan says no plan file exists for this discobox.
	// Nothing is wrong: an ordinary first apply starts from here.
	errNoApplyPlan = errors.New("no apply plan")
	// errApplyPlanVersion says the plan was written by a different version
	// of the CLI; it cannot be resumed safely.
	errApplyPlanVersion = errors.New("unknown apply plan schema version")
)

// loadApplyPlan reads the plan for sandboxID. A missing file is
// errNoApplyPlan; an unknown schema version or unparseable JSON is its own
// error, since either means a run once existed whose intent cannot be read.
func loadApplyPlan(sandboxID string) (*applyPlan, error) {
	data, err := os.ReadFile(applyPlanFilePath(sandboxID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoApplyPlan
	}
	if err != nil {
		return nil, fmt.Errorf("read apply plan for %s: %w", sandboxID, err)
	}
	var plan applyPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parse apply plan for %s: %w", sandboxID, err)
	}
	if plan.SchemaVersion != applyPlanSchemaVersion {
		return nil, fmt.Errorf("%w: got %d, this CLI uses %d",
			errApplyPlanVersion, plan.SchemaVersion, applyPlanSchemaVersion)
	}
	return &plan, nil
}

// save writes the plan atomically and privately. Every phase transition goes
// through this before the transition's next action begins.
func (p *applyPlan) save() error {
	p.UpdatedAt = time.Now().UTC()
	return writeStateFile(applyPlanFilePath(p.SandboxID), p)
}

// remove deletes the plan file once every source is terminal. The worktrees
// it names are already gone by then; the directory and lock file are left,
// empty, for the OS temp/state conventions to age out rather than raced by a
// concurrent run.
func (p *applyPlan) remove() error {
	if err := os.Remove(applyPlanFilePath(p.SandboxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove apply plan for %s: %w", p.SandboxID, err)
	}
	return nil
}

// source returns the plan entry for slug, or nil.
func (p *applyPlan) source(slug string) *applyPlanSource {
	for _, s := range p.Sources {
		if s.Slug == slug {
			return s
		}
	}
	return nil
}

// matchesIdentity reports whether the plan was started for the same server,
// discobox and command-line selection the current invocation carries.
func (p *applyPlan) matchesIdentity(identity applyPlanIdentity) bool {
	return p.Server == identity.Server &&
		p.ProjectID == identity.ProjectID &&
		p.SandboxID == identity.SandboxID &&
		p.equal(identity.applyPlanSelection)
}

// complete reports whether every source reached a terminal phase.
func (p *applyPlan) complete() bool {
	for _, s := range p.Sources {
		if !terminalApplyPhase(s.Phase) {
			return false
		}
	}
	return len(p.Sources) > 0
}

// applyRepoState is the observable current state of a source's target
// repository, gathered by the caller (with real git) so staleness itself is
// pure and table-testable.
type applyRepoState struct {
	// Exists is false when the recorded target directory/repository is gone.
	Exists bool
	// SameRepository is false when the path now holds a different Git
	// repository than the one the plan named (common-dir identity changed).
	SameRepository bool
	// CurrentHEAD is the branch's HEAD right now.
	CurrentHEAD string
	// Dirty is whether the host working tree has uncommitted changes.
	Dirty bool
}

// applyStaleKind names how the world diverged from a plan that has not yet
// landed a source.
type applyStaleKind string

const (
	applyStaleNone           applyStaleKind = ""
	applyStaleTargetReplaced applyStaleKind = "target-replaced"
	applyStaleBranchAdvanced applyStaleKind = "branch-advanced"
	applyStaleDirtyWorktree  applyStaleKind = "dirty-worktree"
)

// applyStaleRemedy names the only way a stale plan is allowed to proceed:
// re-point with --dir, acknowledge with --allow-dirty, or simply re-run after
// the refusal superseded the plan.
type applyStaleRemedy string

const (
	applyStaleRemedyDir        applyStaleRemedy = "dir"
	applyStaleRemedyAllowDirty applyStaleRemedy = "allow-dirty"
	applyStaleRemedyRerun      applyStaleRemedy = "rerun"
)

type applyStale struct {
	Kind   applyStaleKind
	Remedy applyStaleRemedy
	// Detail is the human-readable reason, already naming the facts.
	Detail string
}

// staleness decides whether a not-yet-landed source can still be resumed.
// Landed and later phases are not checked: their resume only verifies the
// recorded host tip is reachable, and a branch moving on top of applied work
// is expected, not stale.
func (s *applyPlanSource) staleness(state applyRepoState) applyStale {
	if s.Phase == phaseLanded || s.Phase == phaseConfirmed || s.Phase == phaseUpToDate {
		return applyStale{}
	}
	if !state.Exists || !state.SameRepository {
		return applyStale{
			Kind:   applyStaleTargetReplaced,
			Remedy: applyStaleRemedyDir,
			Detail: fmt.Sprintf("the local directory %s recorded for source %q no longer holds the repository the plan applied into",
				s.RepoRoot, s.Slug),
		}
	}
	if state.Dirty {
		return applyStale{
			Kind:   applyStaleDirtyWorktree,
			Remedy: applyStaleRemedyAllowDirty,
			Detail: fmt.Sprintf("the working tree at %s has uncommitted changes that were not there when this apply was started",
				s.RepoRoot),
		}
	}
	if state.CurrentHEAD != "" && state.CurrentHEAD != s.HeadAtPlan {
		return applyStale{
			Kind:   applyStaleBranchAdvanced,
			Remedy: applyStaleRemedyRerun,
			Detail: fmt.Sprintf("the branch in %s advanced from %s to %s after this apply was interrupted",
				s.RepoRoot, shortSHA(s.HeadAtPlan), shortSHA(state.CurrentHEAD)),
		}
	}
	return applyStale{}
}

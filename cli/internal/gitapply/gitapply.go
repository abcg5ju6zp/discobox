// Package gitapply lands a range of fetched commits onto a host repository's
// checked-out branch by cherry-pick, per ADR 0014 and ADR 26-10-15-223.
//
// Git has no dry-run for "will this range of commits cherry-pick cleanly" —
// the only way to find out is to attempt the three-way merges. Attempt does
// so for real, but never against the caller's actual checked-out branch: it
// runs the cherry-pick in a linked worktree the caller names, and only
// fast-forwards the real branch once the whole range has landed cleanly.
//
// Unlike a disposable scratch directory, that worktree is kept on a content
// conflict: CHERRY_PICK_HEAD and the unmerged index stay exactly where the
// user can finish the cherry-pick, while the host branch never sits in a
// partial state. The apply plan (cli/internal/cli/apply_plan.go) records the
// worktree, and a later run uses InspectWorktree plus FinishResolvedWorktree
// to finish what the user resolved. AttemptRoot is the same idea for a
// repository that has no commits at all (ADRs 0083, 0084, 0139).
package gitapply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
	"github.com/discobox-ai/x/gitutil"
)

// Result is the outcome of an Attempt.
type Result struct {
	// HostBase is the commit repoRoot's branch was on before the attempt.
	// Always set when HEAD exists: on a conflict it is where the branch still
	// is, and on a clean landing it is the "from" half of the added range.
	HostBase string
	// Landed is true when the commits were fast-forwarded into repoRoot.
	Landed bool
	// HostTip is the resulting host-side commit SHA, set when Landed.
	HostTip string
	// ConflictCommit is the sandbox-side commit that failed to cherry-pick,
	// set when !Landed because of a content conflict.
	ConflictCommit string
	// WorktreeDir is the retained linked worktree the conflict is parked in,
	// set together with ConflictCommit. The cherry-pick is still in progress
	// there; the host repository is untouched.
	WorktreeDir string
	// ChangedPaths is how AttemptRoot refuses: the host working tree is no
	// longer what the discobox was created from, and these are the paths that
	// differ. Nothing was attempted, and nothing changed.
	ChangedPaths []string
}

// MergeBase returns the common ancestor of ref and repoRoot's current HEAD.
func MergeBase(ctx context.Context, repoRoot, ref string) (string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge-base", ref, "HEAD")
	if err != nil {
		return "", fmt.Errorf("compute merge base: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// IsAncestor reports whether ancestor is reachable from descendant. A missing
// or rewritten commit is not an error to the caller: it simply cannot serve as
// a range base, and the caller can derive a current merge base instead.
func IsAncestor(ctx context.Context, repoRoot, ancestor, descendant string) bool {
	_, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// prepareWorktree creates a detached linked worktree of repoRoot at head, in
// the directory the apply plan assigned (its parent is created first, so the
// state directory need not pre-exist). It returns nothing the caller has to
// clean on success: either Attempt finishes and removes it, or a non-conflict
// failure aborts and removes it, or a conflict deliberately keeps it.
func prepareWorktree(ctx context.Context, repoRoot, worktreeDir, head string) error {
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o700); err != nil {
		return fmt.Errorf("create the apply worktree directory: %w", err)
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "add", "--detach", worktreeDir, head); err != nil {
		return fmt.Errorf("create apply worktree: %w", err)
	}
	return nil
}

// removeWorktree deletes the linked worktree and, when one was named, the
// scratch branch its orphan checkout was on. It is idempotent: a worktree or
// branch already gone is not an error, because every caller that reaches it
// wants the same end state — the attempt's working tree no longer exists.
func removeWorktree(ctx context.Context, repoRoot, worktreeDir, scratchBranch string) {
	if worktreeDir != "" {
		_, _ = gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "remove", "--force", worktreeDir)
		_ = os.RemoveAll(worktreeDir)
	}
	if scratchBranch != "" {
		_, _ = gitutil.Output(ctx, repoRoot, nil, nil, "branch", "-D", scratchBranch)
	}
}

// Attempt cherry-picks the commits in (base, tipRef] onto repoRoot's current
// HEAD. base and tipRef must both already be reachable in repoRoot (e.g. via
// a prior fetch). The attempt runs in the linked worktree at worktreeDir,
// which the plan owns and which survives a content conflict. Callers should
// not call Attempt when base and the commit tipRef resolves to are the same —
// there is nothing to cherry-pick, and an empty range fails git cherry-pick
// outright.
func Attempt(ctx context.Context, repoRoot, base, tipRef, worktreeDir string) (Result, error) {
	head, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve host HEAD: %w", err)
	}
	head = strings.TrimSpace(head)

	if err := prepareWorktree(ctx, repoRoot, worktreeDir, head); err != nil {
		return Result{}, err
	}

	rangeSpec := base + ".." + tipRef
	if _, cherryErr := gitutil.Output(ctx, worktreeDir, nil, nil, "cherry-pick", rangeSpec); cherryErr != nil {
		conflicted, err := hasUnmergedPaths(ctx, worktreeDir)
		if err != nil {
			removeWorktree(ctx, repoRoot, worktreeDir, "")
			return Result{}, fmt.Errorf("check cherry-pick conflict state: %w", err)
		}
		if !conflicted {
			// Cherry-pick failed for a reason other than conflicting content
			// (e.g. no git identity configured to create the commit) — that
			// is not something a caller should report as "commit X did not
			// apply cleanly", so surface it as a real error instead.
			_, _ = gitutil.Output(ctx, worktreeDir, nil, nil, "cherry-pick", "--abort")
			removeWorktree(ctx, repoRoot, worktreeDir, "")
			return Result{}, fmt.Errorf("cherry-pick %s: %w", rangeSpec, cherryErr)
		}
		conflict, _ := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "CHERRY_PICK_HEAD")
		return Result{HostBase: head, ConflictCommit: strings.TrimSpace(conflict), WorktreeDir: worktreeDir}, nil
	}

	scratchTip, err := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		removeWorktree(ctx, repoRoot, worktreeDir, "")
		return Result{}, fmt.Errorf("resolve apply worktree tip: %w", err)
	}
	scratchTip = strings.TrimSpace(scratchTip)
	removeWorktree(ctx, repoRoot, worktreeDir, "")

	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge", "--ff-only", scratchTip); err != nil {
		return Result{}, fmt.Errorf("fast-forward host branch onto the applied commits: %w", err)
	}

	return Result{HostBase: head, Landed: true, HostTip: scratchTip}, nil
}

// AttemptRoot lands the commits in (base, tipRef] into a repository that has no
// commits at all — the discobox this apply comes from was created from a `git
// init` and nothing since (ADR 0083), and this is the apply that gives that
// repository its history (ADR 0084).
//
// It differs from Attempt in the two ways that case demands.
//
// The cherry-pick runs onto an unborn HEAD of its own in the retained
// worktree, so the discobox's empty base commit is replayed away rather than
// inherited: the first sandbox commit becomes the repository's root commit,
// authored by whoever wrote it, and no commit the user did not write ends up
// in their history. The orphan checkout needs scratchBranch to name, since it
// creates a real ref; it is removed again with the worktree.
//
// And there is no branch to fast-forward, only untracked files to replace, so
// the landing is guarded by wantTree — the tree the discobox was created from.
// The host working tree must still be exactly that, or this refuses with the
// paths that differ and changes nothing: those files are about to be replaced
// by what the discobox made of them, and anything the user has changed since
// would go with them. wantTree is the workspace snapshot's tree, or the empty
// tree for a discobox created from an empty repository.
func AttemptRoot(ctx context.Context, repoRoot, base, tipRef, wantTree, worktreeDir, scratchBranch string) (Result, error) {
	branch, ok := gitutil.CurrentBranch(ctx, repoRoot)
	if !ok {
		return Result{}, fmt.Errorf("%s has no commits and no branch checked out, so there is nothing to land on", repoRoot)
	}
	gotTree, cleanupTree, err := gitunborn.WorkspaceTree(ctx, repoRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanupTree()
	if gotTree != wantTree {
		changed, err := changedPaths(ctx, repoRoot, wantTree, gotTree)
		if err != nil {
			return Result{}, err
		}
		return Result{ChangedPaths: changed}, nil
	}

	if err := prepareWorktree(ctx, repoRoot, worktreeDir, base); err != nil {
		return Result{}, err
	}
	// Detached at the empty base first, so the worktree's working tree and
	// index start empty; --orphan then keeps them and drops only the history,
	// which is the whole point.
	if _, err := gitutil.Output(ctx, worktreeDir, nil, nil, "checkout", "--orphan", scratchBranch); err != nil {
		removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
		return Result{}, fmt.Errorf("start the apply worktree with no history: %w", err)
	}

	rangeSpec := base + ".." + tipRef
	if _, cherryErr := gitutil.Output(ctx, worktreeDir, nil, nil, "cherry-pick", rangeSpec); cherryErr != nil {
		conflicted, err := hasUnmergedPaths(ctx, worktreeDir)
		if err != nil {
			removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
			return Result{}, fmt.Errorf("check cherry-pick conflict state: %w", err)
		}
		if !conflicted {
			_, _ = gitutil.Output(ctx, worktreeDir, nil, nil, "cherry-pick", "--abort")
			removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
			return Result{}, fmt.Errorf("cherry-pick %s: %w", rangeSpec, cherryErr)
		}
		conflict, _ := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "CHERRY_PICK_HEAD")
		return Result{ConflictCommit: strings.TrimSpace(conflict), WorktreeDir: worktreeDir}, nil
	}

	scratchTip, err := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
		return Result{}, fmt.Errorf("resolve apply worktree tip: %w", err)
	}
	scratchTip = strings.TrimSpace(scratchTip)
	removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)

	// There is no merge to make here: HEAD already names the branch, and
	// creating it at the applied tip is what being born means. The reset then
	// fills the index and replaces the working tree files with what the
	// discobox made of them — safe because they are still byte for byte what it
	// was given, which is what wantTree checked. It removes nothing the
	// repository does not track, so anything ignored or never carried stays.
	if err := gitutil.UpdateRef(ctx, repoRoot, "refs/heads/"+branch, scratchTip); err != nil {
		return Result{}, fmt.Errorf("create branch %s at the applied commits: %w", branch, err)
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "reset", "--hard"); err != nil {
		return Result{}, fmt.Errorf("check out the applied commits in %s: %w", repoRoot, err)
	}

	return Result{Landed: true, HostTip: scratchTip}, nil
}

// WorktreeState is what InspectWorktree found in a retained apply worktree.
type WorktreeState int

const (
	// WorktreeMissing: the worktree (or its registration) is gone — pruned
	// or deleted by hand. Apply cannot tell what happened to the resolution.
	WorktreeMissing WorktreeState = iota
	// WorktreeConflict: the cherry-pick is still in progress with its
	// CHERRY_PICK_HEAD; the user has not finished resolving.
	WorktreeConflict
	// WorktreeAborted: there is no cherry-pick in progress and no applied
	// commits remain — the user ran cherry-pick --abort (root: HEAD is unborn
	// again).
	WorktreeAborted
	// WorktreeResolved: the user finished the cherry-pick; the worktree's
	// HEAD carries the replayed range and is ready to land.
	WorktreeResolved
)

// InspectWorktree reads the state of a worktree a previous Attempt left
// behind on conflict. headBefore is the host commit the worktree started from
// (empty for the root strategy, whose start had no commits).
func InspectWorktree(ctx context.Context, repoRoot, worktreeDir, headBefore string) (WorktreeState, string, error) {
	if !worktreeRegistered(ctx, repoRoot, worktreeDir) {
		return WorktreeMissing, "", nil
	}
	info, err := os.Stat(worktreeDir)
	if err != nil {
		return WorktreeMissing, "", fmt.Errorf("stat the apply worktree %s: %w", worktreeDir, err)
	}
	if !info.IsDir() {
		return WorktreeMissing, "", nil
	}

	if out, err := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD"); err == nil && strings.TrimSpace(out) != "" {
		return WorktreeConflict, strings.TrimSpace(out), nil
	}

	// An unborn HEAD with no cherry-pick in flight is the root strategy
	// after an abort: there was never a commit there. Check it before asking
	// for HEAD, which is an error on an unborn branch.
	if gitunborn.HeadIsUnborn(ctx, worktreeDir) {
		return WorktreeAborted, "", nil
	}
	headOut, err := gitutil.Output(ctx, worktreeDir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return WorktreeMissing, "", fmt.Errorf("resolve the apply worktree HEAD at %s: %w", worktreeDir, err)
	}
	head := strings.TrimSpace(headOut)
	if headBefore != "" && head == headBefore {
		// Detached back where it started with nothing in flight: abort.
		return WorktreeAborted, head, nil
	}
	return WorktreeResolved, head, nil
}

// FinishResolvedWorktree lands a worktree the user resolved onto the host:
// fast-forwarding the host branch to it for the ordinary strategy, or
// creating the unborn branch and resetting the working tree for the root
// strategy. The worktree and scratch branch are removed either way. It
// returns the resulting host tip.
func FinishResolvedWorktree(ctx context.Context, repoRoot, worktreeDir, scratchBranch, resolvedTip, branch string, root bool) (string, error) {
	if resolvedTip == "" {
		return "", fmt.Errorf("no resolved tip to land from %s", worktreeDir)
	}
	if !root {
		if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge", "--ff-only", resolvedTip); err != nil {
			return "", fmt.Errorf("fast-forward host branch onto the resolved commits: %w", err)
		}
	} else {
		if branch == "" {
			b, ok := gitutil.CurrentBranch(ctx, repoRoot)
			if !ok {
				return "", fmt.Errorf("%s has no commits and no branch checked out, so there is nothing to land on", repoRoot)
			}
			branch = b
		}
		if err := gitutil.UpdateRef(ctx, repoRoot, "refs/heads/"+branch, resolvedTip); err != nil {
			return "", fmt.Errorf("create branch %s at the resolved commits: %w", branch, err)
		}
		if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "reset", "--hard"); err != nil {
			return "", fmt.Errorf("check out the resolved commits in %s: %w", repoRoot, err)
		}
	}
	removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
	return resolvedTip, nil
}

// RemoveWorktree discards a retained apply worktree (and its scratch branch,
// if any) without landing it — used when the plan that owned it is superseded
// and its resolution is no longer the one that will land.
func RemoveWorktree(ctx context.Context, repoRoot, worktreeDir, scratchBranch string) {
	removeWorktree(ctx, repoRoot, worktreeDir, scratchBranch)
}

// worktreeRegistered reports whether repoRoot still lists worktreeDir among
// its linked worktrees. A deleted registration is how "git worktree prune"
// makes a retained worktree unreachable even if its directory lingers. Git
// prints canonical paths (/var is /private/var on macOS, a state dir under a
// symlinked home is printed resolved), so the wanted path is canonicalized
// before the comparison.
func worktreeRegistered(ctx context.Context, repoRoot, worktreeDir string) bool {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	canonical, err := filepath.EvalSymlinks(worktreeDir)
	if err != nil {
		canonical = worktreeDir
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		listed := strings.TrimPrefix(line, "worktree ")
		if listed == worktreeDir || listed == canonical {
			return true
		}
	}
	return false
}

// changedPaths names what differs between the tree a discobox was created from
// and the one the working tree holds now, so a refusal says which files it is
// protecting rather than only that something moved.
func changedPaths(ctx context.Context, repoRoot, wantTree, gotTree string) ([]string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "diff", "--name-status", wantTree, gotTree)
	if err != nil {
		return nil, fmt.Errorf("compare the working tree with what the discobox was created from: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// hasUnmergedPaths reports whether dir's index currently has unresolved
// conflict entries. A real cherry-pick conflict always leaves some; other
// cherry-pick failures (e.g. no git identity configured to create the
// resulting commit) leave CHERRY_PICK_HEAD set too but stage cleanly, so this
// is what actually distinguishes the two.
func hasUnmergedPaths(ctx context.Context, dir string) (bool, error) {
	out, err := gitutil.Output(ctx, dir, nil, nil, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, fmt.Errorf("list unmerged paths: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

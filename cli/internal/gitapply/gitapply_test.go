package gitapply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a repo with one commit on main, returning its root and that
// commit's SHA.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "--initial-branch=main")
	// Configure the identity in the repo itself, not just in the environment
	// this helper passes to its own git calls. Attempt shells out to git on its
	// own, and cherry-pick needs a committer: without this the test passes only
	// on machines that happen to have a global user.name/user.email, and fails
	// with "Committer identity unknown" everywhere else.
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "test")
	writeFile(t, dir, "README.md", "base\n")
	run(t, dir, "add", "README.md")
	run(t, dir, "commit", "-m", "base")
	commit := run(t, dir, "rev-parse", "HEAD")
	return dir, commit
}

// setGitIdentity gives the test process itself a git identity, so the git
// commands Attempt runs internally (which, unlike run(), do not carry their
// own explicit identity env) do not depend on the host running the test
// already having ~/.gitconfig or /etc/gitconfig configured.
func setGitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// cloneWithCommits returns a bare-ish second working copy of repoRoot with
// extraCommits new commits on top of base, standing in for a sandbox's
// repository whose tip a caller has already fetched into repoRoot.
func addCommits(t *testing.T, repoRoot string, files ...string) string {
	t.Helper()
	for i, name := range files {
		writeFile(t, repoRoot, name, "sandbox change\n")
		run(t, repoRoot, "add", name)
		run(t, repoRoot, "commit", "-m", "sandbox commit "+string(rune('a'+i)))
	}
	return run(t, repoRoot, "rev-parse", "HEAD")
}

// newWorktreeDir is where a test's persistent apply worktree lives: a fresh
// path under the test's temp area, like the CLI state directory is in real
// use. It does not exist yet; Attempt creates it.
func newWorktreeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "apply", "primary")
}

func onlyMainWorktree(t *testing.T, host string) {
	t.Helper()
	worktrees := run(t, host, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("expected only the main worktree to remain, got:\n%s", worktrees)
	}
}

func TestAttemptLandsCleanRangeAndFastForwards(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	// Simulate a fetched sandbox tip by branching off base in the same repo
	// (Attempt only needs base and tipRef to already be reachable locally,
	// which is exactly what a real fetch would have arranged).
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt", "b.txt")
	run(t, host, "checkout", "-q", "main")

	result, err := Attempt(ctx, host, base, tip, newWorktreeDir(t))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if !result.Landed {
		t.Fatalf("expected a clean range to land, got %+v", result)
	}
	if result.HostTip == "" {
		t.Fatal("landed result has no HostTip")
	}
	// HostBase is what the caller reports the branch moved from, so it has to
	// be the pre-apply tip, not the post-apply one.
	if result.HostBase != base {
		t.Fatalf("HostBase = %s, want the pre-apply host tip %s", result.HostBase, base)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != result.HostTip {
		t.Fatalf("host HEAD = %s, want fast-forwarded to %s", headNow, result.HostTip)
	}
	// Two sandbox commits in, preserved individually rather than squashed.
	count := run(t, host, "rev-list", "--count", base+".."+headNow)
	if count != "2" {
		t.Fatalf("landed commit count = %s, want 2", count)
	}
	// No leftover apply worktree.
	onlyMainWorktree(t, host)
}

func TestAttemptConflictRetainsAWorktreeAndLeavesHostUntouched(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	// A host-side change and a conflicting sandbox-side change to the same
	// file, both starting from base.
	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	worktreeDir := newWorktreeDir(t)
	result, err := Attempt(ctx, host, base, tip, worktreeDir)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if result.Landed {
		t.Fatalf("expected a conflicting range to fail, got %+v", result)
	}
	if result.ConflictCommit != tip {
		t.Fatalf("conflictCommit = %s, want %s", result.ConflictCommit, tip)
	}
	if result.WorktreeDir != worktreeDir {
		t.Fatalf("worktreeDir = %q, want %q", result.WorktreeDir, worktreeDir)
	}
	// A conflict still reports where the host branch stands, since that is
	// what the caller tells the user is unchanged.
	if result.HostBase != hostHead {
		t.Fatalf("HostBase = %s, want %s", result.HostBase, hostHead)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != hostHead {
		t.Fatalf("host HEAD moved from %s to %s after a conflicting attempt", hostHead, headNow)
	}
	if status := run(t, host, "status", "--porcelain"); status != "" {
		t.Fatalf("host working tree is dirty after a conflicting attempt: %q", status)
	}
	// The conflict is parked in the retained worktree, in a real
	// cherry-pick the user can finish.
	if got := run(t, worktreeDir, "rev-parse", "CHERRY_PICK_HEAD"); got != tip {
		t.Fatalf("CHERRY_PICK_HEAD = %s, want %s", got, tip)
	}
	if unmerged := run(t, worktreeDir, "diff", "--name-only", "--diff-filter=U"); !strings.Contains(unmerged, "README.md") {
		t.Fatalf("unmerged paths = %q, want README.md in conflict", unmerged)
	}
	// Git prints canonical paths (/var is /private/var on macOS), so compare
	// against the resolved form.
	resolved, err := filepath.EvalSymlinks(worktreeDir)
	if err != nil {
		t.Fatalf("resolve worktree path: %v", err)
	}
	if worktrees := run(t, host, "worktree", "list", "--porcelain"); !strings.Contains(worktrees, "worktree "+resolved) {
		t.Fatalf("retained worktree not registered:\n%s", worktrees)
	}

	// Inspecting it on a re-run reports the still-open conflict.
	state, _, err := InspectWorktree(ctx, host, worktreeDir, hostHead)
	if err != nil || state != WorktreeConflict {
		t.Fatalf("inspect retained conflict: state=%d err=%v, want WorktreeConflict", state, err)
	}
}

func TestResolveInWorktreeThenFinishLandsIt(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	worktreeDir := newWorktreeDir(t)
	result, err := Attempt(ctx, host, base, tip, worktreeDir)
	if err != nil || result.Landed {
		t.Fatalf("attempt: result=%+v err=%v, want a retained conflict", result, err)
	}

	// The user resolves the conflict in the worktree and continues the
	// cherry-pick themselves.
	writeFile(t, worktreeDir, "README.md", "resolved by the user\n")
	run(t, worktreeDir, "add", "README.md")
	run(t, worktreeDir, "cherry-pick", "--continue")

	state, resolvedTip, err := InspectWorktree(ctx, host, worktreeDir, hostHead)
	if err != nil {
		t.Fatalf("inspect after resolution: %v", err)
	}
	if state != WorktreeResolved {
		t.Fatalf("state = %d, want WorktreeResolved", state)
	}
	landed, err := FinishResolvedWorktree(ctx, host, worktreeDir, "", resolvedTip, "", false)
	if err != nil {
		t.Fatalf("finish resolved worktree: %v", err)
	}
	if landed != resolvedTip {
		t.Fatalf("landed = %s, want %s", landed, resolvedTip)
	}
	if run(t, host, "rev-parse", "HEAD") != landed {
		t.Fatal("host branch did not fast-forward to the resolved tip")
	}
	if got := run(t, host, "show", "HEAD:README.md"); got != "resolved by the user" {
		t.Fatalf("resolved content did not land: %q", got)
	}
	onlyMainWorktree(t, host)
}

func TestAbortInWorktreeIsDetectedAndDiscardable(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	worktreeDir := newWorktreeDir(t)
	if _, err := Attempt(ctx, host, base, tip, worktreeDir); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	run(t, worktreeDir, "cherry-pick", "--abort")

	state, head, err := InspectWorktree(ctx, host, worktreeDir, hostHead)
	if err != nil {
		t.Fatalf("inspect after abort: %v", err)
	}
	if state != WorktreeAborted || head != hostHead {
		t.Fatalf("state = %d head = %s, want WorktreeAborted at %s", state, head, hostHead)
	}

	RemoveWorktree(ctx, host, worktreeDir, "")
	onlyMainWorktree(t, host)
	if run(t, host, "rev-parse", "HEAD") != hostHead {
		t.Fatal("host branch moved after discarding an aborted worktree")
	}
}

func TestMissingWorktreeIsDetected(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	worktreeDir := newWorktreeDir(t)
	if _, err := Attempt(ctx, host, base, tip, worktreeDir); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	// Simulate `git worktree prune` plus the directory being lost.
	run(t, host, "worktree", "remove", "--force", worktreeDir)
	_ = os.RemoveAll(worktreeDir)

	state, _, err := InspectWorktree(ctx, host, worktreeDir, hostHead)
	if err != nil {
		t.Fatalf("inspect missing worktree: %v", err)
	}
	if state != WorktreeMissing {
		t.Fatalf("state = %d, want WorktreeMissing", state)
	}
}

// TestAttemptNonConflictFailureReturnsError covers a cherry-pick that fails
// for a reason other than conflicting content — no identity to attribute the
// resulting commit to, in this case. That must surface as an error, not be
// misreported as a conflict on some commit: nothing about it is a content
// conflict a caller could ask the user to resolve, and nothing is retained.
func TestAttemptNonConflictFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt")
	run(t, host, "checkout", "-q", "main")
	hostHead := run(t, host, "rev-parse", "HEAD")

	// Explicit (even empty) identity env vars override any discovered
	// ~/.gitconfig, so this deterministically fails the same way regardless
	// of what git identity the host running the test happens to have.
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")

	result, err := Attempt(ctx, host, base, tip, newWorktreeDir(t))
	if err == nil {
		t.Fatalf("expected a non-conflict cherry-pick failure to return an error, got result %+v", result)
	}
	if !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("expected a zero Result alongside the error, got %+v", result)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != hostHead {
		t.Fatalf("host HEAD moved from %s to %s after a failed attempt", hostHead, headNow)
	}
	onlyMainWorktree(t, host)
}

func TestMergeBase(t *testing.T) {
	ctx := context.Background()
	host, base := newRepo(t)
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt")
	run(t, host, "checkout", "-q", "main")

	got, err := MergeBase(ctx, host, tip)
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if got != base {
		t.Fatalf("merge base = %s, want %s", got, base)
	}
}

// newUnbornHostAndSandbox builds the pair ADR 0083 creates: a local repository
// with no commits whose working tree holds files, and a discobox-side history
// of an empty base commit plus commits on top. Both live in the one repository,
// the way they do after `discobox apply` has fetched the discobox's tip.
//
// It returns the repository root, the empty base commit, the discobox tip, and
// the tree the discobox was created from.
func newUnbornHostAndSandbox(t *testing.T, files map[string]string) (string, string, string, string) {
	t.Helper()
	host := t.TempDir()
	run(t, host, "init", "--initial-branch=main")
	run(t, host, "config", "user.email", "test@example.com")
	run(t, host, "config", "user.name", "test")
	for name, content := range files {
		writeFile(t, host, name, content)
	}

	// The empty base and the snapshot of the working tree, as create writes
	// them: objects and refs in the user's own repository, no branch moved.
	emptyTree := run(t, host, "hash-object", "-t", "tree", "-w", "--stdin")
	base := commitTree(t, host, emptyTree, "", "discobox new empty base")
	run(t, host, "update-ref", "refs/discobox/run/base", base)
	wantTree := emptyTree
	if len(files) > 0 {
		wantTree = workspaceTree(t, host)
		run(t, host, "update-ref", "refs/discobox/run/snap", commitTree(t, host, wantTree, base, "snapshot"))
	}

	// The discobox's own commits, built in a worktree so the host's unborn HEAD
	// and untracked files are left exactly as they are.
	work := filepath.Join(t.TempDir(), "sandbox")
	run(t, host, "worktree", "add", "--detach", work, base)
	for name, content := range files {
		writeFile(t, work, name, content)
	}
	// Always something of the discobox's own in the first commit: a discobox
	// created from an empty repository has nothing else to put in it.
	writeFile(t, work, "FIRST.md", "written in the discobox\n")
	run(t, work, "add", "-A")
	run(t, work, "commit", "-m", "first commit")
	writeFile(t, work, "NEW.md", "made in the discobox\n")
	run(t, work, "add", "-A")
	run(t, work, "commit", "-m", "second commit")
	tip := run(t, work, "rev-parse", "HEAD")
	run(t, host, "worktree", "remove", "--force", work)
	return host, base, tip, wantTree
}

func commitTree(t *testing.T, repoRoot, tree, parent, message string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return run(t, repoRoot, args...)
}

func workspaceTree(t *testing.T, repoRoot string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	cmd := exec.CommandContext(context.Background(), "git", "add", "--all", "--", ".")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(context.Background(), "git", "write-tree")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git write-tree: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAttemptRootGivesAnUnbornRepositoryItsHistory(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, map[string]string{"README.md": "hello\n"})

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree,
		newWorktreeDir(t), "discobox-apply/test/primary")
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if !result.Landed || result.HostTip == "" {
		t.Fatalf("result = %+v, want the commits landed", result)
	}
	// The branch HEAD already names is what was born, at the applied commits.
	if head := run(t, host, "rev-parse", "refs/heads/main"); head != result.HostTip {
		t.Fatalf("main = %s, want the applied tip %s", head, result.HostTip)
	}
	// The discobox's empty base is replayed away: the user's own first commit
	// is the root of their history, and nothing they did not write is in it.
	roots := run(t, host, "rev-list", "--max-parents=0", "HEAD")
	if strings.Contains(roots, base) {
		t.Fatalf("history root = %s, want the discobox's empty base %s left out of it", roots, base)
	}
	subjects := run(t, host, "log", "--reverse", "--format=%s")
	if subjects != "first commit\nsecond commit" {
		t.Fatalf("history = %q, want only the discobox's own commits", subjects)
	}
	// The working tree is what the discobox made of it, and it is clean.
	if status := run(t, host, "status", "--porcelain"); status != "" {
		t.Fatalf("status = %q, want a clean tree after the apply", status)
	}
	if _, err := os.Stat(filepath.Join(host, "NEW.md")); err != nil {
		t.Fatalf("the discobox's new file did not land: %v", err)
	}
	// No scratch branch or worktree outlives the attempt.
	if branches := run(t, host, "for-each-ref", "--format=%(refname:short)", "refs/heads"); branches != "main" {
		t.Fatalf("branches = %q, want only main left behind", branches)
	}
	onlyMainWorktree(t, host)
}

func TestAttemptRootLandsIntoAnEmptyRepository(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, nil)

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree,
		newWorktreeDir(t), "discobox-apply/test/primary")
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if !result.Landed {
		t.Fatalf("result = %+v, want the commits landed", result)
	}
	if subjects := run(t, host, "log", "--reverse", "--format=%s"); subjects != "first commit\nsecond commit" {
		t.Fatalf("history = %q, want the discobox's commits rooted at the first", subjects)
	}
}

func TestAttemptRootRefusesWhenTheWorkingTreeMovedOn(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, map[string]string{"README.md": "hello\n"})
	// Work done here since the discobox was created, which has no commits to
	// survive in and would be replaced by what the discobox made of the file.
	writeFile(t, host, "README.md", "hello, and something only local\n")

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree,
		newWorktreeDir(t), "discobox-apply/test/primary")
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if result.Landed {
		t.Fatal("AttemptRoot landed over local changes it should have refused")
	}
	if len(result.ChangedPaths) != 1 || !strings.Contains(result.ChangedPaths[0], "README.md") {
		t.Fatalf("changed paths = %v, want the file that moved on named", result.ChangedPaths)
	}
	// Nothing at all happened: no branch, and the local edit is still there.
	if refs := run(t, host, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "" {
		t.Fatalf("refs/heads = %q, want the repository still without commits", refs)
	}
	content, err := os.ReadFile(filepath.Join(host, "README.md"))
	if err != nil || string(content) != "hello, and something only local\n" {
		t.Fatalf("README.md = %q (%v), want the local change untouched", content, err)
	}
}

// TestAttemptRootNonConflictFailureRemovesTheWorktreeAndLeavesHostUnborn is
// ADR 0139's keep rule under the retained-worktree design: the orphan
// cherry-pick starts from an empty tree, so a linear range cannot
// content-conflict there (every picked delta applies onto the tree its own
// history guaranteed) — the failure mode that actually exists for the root
// strategy before it lands is a non-conflict cherry-pick error, and it has to
// abort, remove the worktree and scratch branch, and leave the host checkout
// unborn so apply's remove-if-unborn cleanup still removes the .git it made.
func TestAttemptRootNonConflictFailureRemovesTheWorktreeAndLeavesHostUnborn(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, map[string]string{"README.md": "hello\n"})

	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")

	worktreeDir := newWorktreeDir(t)
	const scratchBranch = "discobox-apply/test/primary"
	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree,
		worktreeDir, scratchBranch)
	if err == nil {
		t.Fatalf("AttemptRoot without a git identity: got %+v, want an error", result)
	}
	if !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("result = %+v, want zero alongside the error", result)
	}
	// No branch was born, nothing was checked out, and no worktree or scratch
	// branch survived the failed attempt.
	if refs := run(t, host, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "" {
		t.Fatalf("refs/heads = %q, want the repository still without commits", refs)
	}
	if content, err := os.ReadFile(filepath.Join(host, "README.md")); err != nil || string(content) != "hello\n" {
		t.Fatalf("host README.md = %q (%v), want it untouched", content, err)
	}
	onlyMainWorktree(t, host)
	if branches := run(t, host, "for-each-ref", "--format=%(refname:short)", "refs/heads/discobox-apply"); branches != "" {
		t.Fatalf("scratch branch survived: %q", branches)
	}
}

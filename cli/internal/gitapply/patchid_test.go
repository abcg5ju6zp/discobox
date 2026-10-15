package gitapply

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestAlreadyAppliedFindsCherryPickedRange(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	// The sandbox's two commits, forked from base.
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt", "b.txt")
	run(t, host, "checkout", "-q", "main")

	// The host moves on, then takes the sandbox's work as cherry-picks:
	// different SHAs, identical patches.
	writeFile(t, host, "host-only.txt", "host work\n")
	run(t, host, "add", "host-only.txt")
	run(t, host, "commit", "-m", "host-only work")
	hostBefore := run(t, host, "rev-parse", "HEAD")
	run(t, host, "cherry-pick", base+".."+tip)
	hostTip := run(t, host, "rev-parse", "HEAD")

	equiv, got, ok, err := AlreadyApplied(ctx, host, base, tip)
	if err != nil {
		t.Fatalf("AlreadyApplied: %v", err)
	}
	if !ok {
		t.Fatal("expected the full sandbox range to be recognized as applied")
	}
	if got != hostTip {
		t.Fatalf("recognized host tip = %s, want %s", got, hostTip)
	}
	if len(equiv) != 2 || equiv[tip] != hostTip {
		t.Fatalf("equivalents = %v, want both sandbox commits mapped", equiv)
	}
	// Sanity: recognition did not mistake the host-only commit for anything.
	if got == hostBefore {
		t.Fatal("host tip was reported as the pre-cherry-pick commit")
	}
}

func TestAlreadyAppliedFalseForFreshRange(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt")
	run(t, host, "checkout", "-q", "main")
	writeFile(t, host, "host-only.txt", "host work\n")
	run(t, host, "add", "host-only.txt")
	run(t, host, "commit", "-m", "host-only work")

	if _, got, ok, err := AlreadyApplied(ctx, host, base, tip); err != nil {
		t.Fatalf("AlreadyApplied: %v", err)
	} else if ok || got != "" {
		t.Fatalf("unapplied range recognized as applied: ok=%v tip=%s", ok, got)
	}
}

func TestReplayCompleteCountsReplayedCommits(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt", "b.txt")
	run(t, host, "checkout", "-q", "main")

	// Fully replayed range: one host commit per range commit. The two SHAs are
	// picked explicitly — picking an A..B range whose A is HEAD is rejected by
	// modern git as an empty range.
	first, second := twoShas(t, host, base, tip)
	run(t, host, "cherry-pick", first, second)
	full := run(t, host, "rev-parse", "HEAD")
	if ok, err := ReplayComplete(ctx, host, base, tip, base, full); err != nil || !ok {
		t.Fatalf("full range: ok=%v err=%v, want complete", ok, err)
	}

	// Only the first of two sandbox commits replayed (a skipped second):
	// the count fails.
	run(t, host, "reset", "-q", "--hard", base)
	run(t, host, "cherry-pick", first)
	partial := run(t, host, "rev-parse", "HEAD")
	if ok, err := ReplayComplete(ctx, host, base, tip, base, partial); err != nil || ok {
		t.Fatalf("partial range: ok=%v err=%v, want not complete", ok, err)
	}
}

// A real conflict resolution is complete even when its content differs from
// the sandbox commit: patch identity is not the bar, the sequencer answering
// every range commit with a host commit is.
func TestReplayCompleteAfterARealConflictResolution(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)
	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "add", "README.md")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "add", "README.md")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	wt := filepath.Join(t.TempDir(), "apply")
	result, err := Attempt(ctx, host, base, tip, wt)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if result.Landed {
		t.Fatal("expected the conflicting attempt to park")
	}
	// Resolve with content neither side had, then finish the sequencer.
	writeFile(t, wt, "README.md", "resolved differently\n")
	run(t, wt, "add", "README.md")
	run(t, wt, "cherry-pick", "--continue")
	resolved := run(t, wt, "rev-parse", "HEAD")

	if ok, err := ReplayComplete(ctx, host, base, tip, hostHead, resolved); err != nil || !ok {
		t.Fatalf("resolved real conflict: ok=%v err=%v, want complete", ok, err)
	}
}

// twoShas returns the two sandbox commits in base..tip oldest first.
func twoShas(t *testing.T, repo, base, tip string) (string, string) {
	t.Helper()
	out := run(t, repo, "rev-list", "--reverse", base+".."+tip)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 commits in range, got %d", len(lines))
	}
	return lines[0], lines[1]
}

// A range only partly present is not applied: that is the state where the
// user also has local work and guessing either way would reapply something.
func TestAlreadyAppliedFalseForPartialMatch(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	first := addCommits(t, host, "a.txt")
	tip := addCommits(t, host, "b.txt")
	run(t, host, "checkout", "-q", "main")
	run(t, host, "cherry-pick", first)

	if _, got, ok, err := AlreadyApplied(ctx, host, base, tip); err != nil {
		t.Fatalf("AlreadyApplied: %v", err)
	} else if ok || got != "" {
		t.Fatalf("partially applied range recognized as fully applied: ok=%v tip=%s", ok, got)
	}
}

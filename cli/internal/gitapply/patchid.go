package gitapply

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// AlreadyApplied is the plan-free recovery check (ADR 26-10-15-223 §3): with
// no apply plan saying otherwise, are all the sandbox commits in (base, tip]
// already present on the host branch as cherry-picked equivalents?
//
// It answers with patch IDs: git canonicalizes a commit's diff the same way
// regardless of parent, author or committer, so a cherry-picked commit has
// the same patch ID under a different SHA. Every sandbox commit in the range
// must have an equivalent among the host commits since the histories forked;
// a partial match is deliberately not enough — that is the case where the
// user also has their own new commits, and guessing would reapply work.
//
// On a full match it returns every sandbox SHA mapped to its host equivalent
// and, separately, the host commit equivalent to tip — the host tip a
// confirmation is recorded against. The commands are run as separate
// processes with in-memory pipes between them rather than through a shell, so
// this works on Windows as well as Unix.
func AlreadyApplied(ctx context.Context, repoRoot, base, tip string) (equivalents map[string]string, hostTip string, ok bool, err error) {
	sandboxIDs, err := patchIDMap(ctx, repoRoot, base, tip)
	if err != nil {
		return nil, "", false, err
	}
	if len(sandboxIDs) == 0 {
		return nil, "", false, nil
	}
	// The host side to compare against runs from the common ancestor of the
	// sandbox tip and the host HEAD — everything the host has that the
	// sandbox does not — not from base, since a recorded base may predate a
	// fork.
	mergeBase, err := MergeBase(ctx, repoRoot, tip)
	if err != nil {
		return nil, "", false, err
	}
	hostCommits, err := patchIDMap(ctx, repoRoot, mergeBase, "HEAD")
	if err != nil {
		return nil, "", false, err
	}
	// Invert the host side to patch-ID → SHA: several host commits could in
	// principle share a patch ID (repeated cherry-picks), in which case any of
	// them is an equivalent; the map keeps one, which is all recognition
	// needs.
	hostByPatch := make(map[string]string, len(hostCommits))
	for sha, id := range hostCommits {
		hostByPatch[id] = sha
	}

	// Every sandbox commit must have a host equivalent; the confirmation
	// names the equivalent of the newest sandbox commit.
	equivalents = make(map[string]string, len(sandboxIDs))
	for sha, id := range sandboxIDs {
		hostSha, found := hostByPatch[id]
		if !found {
			return nil, "", false, nil
		}
		equivalents[sha] = hostSha
		if sha == tip {
			hostTip = hostSha
		}
	}
	if hostTip == "" {
		return nil, "", false, nil
	}
	return equivalents, hostTip, true, nil
}

// ReplayComplete reports that a user-finished cherry-pick sequence produced
// one host commit per sandbox commit: the replayed count over headBefore
// equals the range count over base. Call it only on a WorktreeResolved
// inspection — that state already means the sequencer ended cleanly (no
// CHERRY_PICK_HEAD, no unfinished merge) and HEAD advanced, so this count is
// the one check a `git cherry-pick --skip` or a dropped commit fails.
//
// It deliberately does NOT compare patch IDs: a conflict the user resolves
// legitimately produces a commit whose diff differs from the sandbox original
// (its pre-image is the host content, not the sandbox parent's) — even
// verbatim adoption of the sandbox side differs. The resolution content is
// the user's decision; the guarantee is that every range commit was answered
// with a commit, never silently skipped.
func ReplayComplete(ctx context.Context, repoRoot, base, tip, headBefore, resolvedTip string) (bool, error) {
	wantN, err := revListCount(ctx, repoRoot, base, tip)
	if err != nil {
		return false, err
	}
	gotN, err := revListCount(ctx, repoRoot, headBefore, resolvedTip)
	if err != nil || gotN != wantN {
		return false, err
	}
	return true, nil
}

// revListCount is `git rev-list --count base..head`.
func revListCount(ctx context.Context, repoRoot, base, head string) (int, error) {
	out, err := runGit(ctx, repoRoot, nil, "rev-list", "--count", base+".."+head)
	if err != nil {
		return 0, fmt.Errorf("count commits %s..%s: %w", base, head, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// patchIDMap maps every commit in (base, head] to its patch ID, keyed by
// commit SHA. The pipeline is `git log -p | git patch-id` — patch-id keys the
// diffs it parses to the "commit <sha>" lines log prints between them, which
// diff-tree --stdin does not emit — run without a shell, with log's output
// fed to patch-id in memory.
func patchIDMap(ctx context.Context, repoRoot, base, head string) (map[string]string, error) {
	logOut, err := runGit(ctx, repoRoot, nil, "log", "-p", "--no-color", base+".."+head)
	if err != nil {
		return nil, fmt.Errorf("list commits %s..%s: %w", base, head, err)
	}
	if len(bytes.TrimSpace(logOut)) == 0 {
		return map[string]string{}, nil
	}
	out, err := runGit(ctx, repoRoot, logOut, "patch-id")
	if err != nil {
		return nil, fmt.Errorf("compute patch IDs for %s..%s: %w", base, head, err)
	}
	ids := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			ids[fields[1]] = fields[0]
		}
	}
	return ids, nil
}

// runGit runs one git command in repoRoot with stdin, returning its stdout.
func runGit(ctx context.Context, repoRoot string, stdin []byte, args ...string) ([]byte, error) {
	//nolint:gosec // G204: git subcommand and ref arguments produced by this package from local repository state, not external input.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

package sandboxcreate

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"

	"github.com/discobox-ai/x/gitutil"
)

// assertLocalGeneration refuses to deliver a local source from a repository
// that no longer is the sandbox's frozen generation (ADR 26-10-15-943 §6):
// pushing from it would make the frozen namespace point at different objects.
//
// The pinned commit must still exist; for a branch checkout the local branch
// must not have moved off it (a checkout at a tag or bare commit does not move
// with local work); and a dirty-workspace snapshot ref must still be present.
// A remote-URL source is exempt — its frozen objects never lived on this
// machine — as is a sandbox created without a generation.
func assertLocalGeneration(ctx context.Context, repoRoot string, source apimodel.GitSource) error {
	if repoRoot == "" {
		// Nothing local to speak for: a remote-URL source or a later
		// re-delivery that found no local repository.
		return nil
	}
	commit, branch, snapshot := localGenerationPins(source)
	if commit == "" {
		return nil
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("source %q: the pinned commit %s is no longer in the local repository; the discobox was frozen to it", source.Slug.Or(""), commit)
	}
	if branch != "" {
		head, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "HEAD")
		if err == nil && strings.TrimSpace(head) != commit {
			return fmt.Errorf("source %q: the local branch %q has moved off the pinned commit %s (now %s); finish the discobox from the repository it was created from", source.Slug.Or(""), branch, commit, strings.TrimSpace(head))
		}
	}
	if snapshot != "" {
		if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", "--quiet", snapshot); err != nil {
			return fmt.Errorf("source %q: the dirty-workspace snapshot %s is gone; the discobox was frozen to it", source.Slug.Or(""), snapshot)
		}
	}
	return nil
}

// localGenerationPins reads the objects a local repository must still hold for
// the source's frozen generation.
func localGenerationPins(source apimodel.GitSource) (commit, branch, snapshot string) {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "", "", ""
	}
	commit = strings.ToLower(strings.TrimSpace(checkout.Commit.Or("")))
	if commit == "" {
		return "", "", ""
	}
	if strings.TrimSpace(checkout.RefType.Or("")) == runSourceRefTypeBranch {
		branch = strings.TrimSpace(checkout.RefName.Or(""))
	}
	if workspace, ok := source.Workspace.Get(); ok && workspace.Mode.Or("") == runWorkspaceModeDirty {
		snapshot = strings.TrimSpace(workspace.SnapshotRef.Or(""))
	}
	return commit, branch, snapshot
}

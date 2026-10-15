package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Pin gates for approval-minted authorizations (ADR 26-10-15-871). A grant or
// trust minted by approving an agent's ask is pinned to the sandbox run that
// asked and — a grant — to the credential version that answered. Every live
// read re-checks the pins against the rows as they are now, so a rebuild,
// archival, or deliberate replacement stops authorizing without the row being
// deleted. Zero on either pin is an unpinned, static authorization and always
// passes: preconfigured grants and rows written before pinning keep the old
// behavior wholesale.

// GrantPinLive reports whether a pinned grant's sandbox run and credential
// version still match. A missing sandbox or secret reads as dead rather than
// erroring: a live read must not fail a whole listing because one row's
// counterpart was deleted.
func (s *Store) GrantPinLive(ctx context.Context, grant *model.SecretGrant) (bool, error) {
	if grant.SecretVersion > 0 {
		read, err := s.getRead(ctx)
		if err != nil {
			return false, err
		}
		var version int64
		result := read.Model(&model.Secret{}).
			Where("project_id = ? AND id = ?", grant.ProjectID, grant.SecretID).
			Select("value_version").Limit(1).Scan(&version)
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected == 0 || version != grant.SecretVersion {
			return false, nil
		}
	}
	if grant.SandboxGeneration > 0 {
		live, err := s.sandboxPinLive(ctx, grant.ProjectID, grant.ScopeKey, grant.SandboxGeneration)
		if err != nil || !live {
			return false, err
		}
	}
	return true, nil
}

// TrustPinLive reports whether a trust pinned to a sandbox run is still that
// run.
func (s *Store) TrustPinLive(ctx context.Context, trust *model.HostTrust) (bool, error) {
	if trust.SandboxGeneration == 0 {
		return true, nil
	}
	return s.sandboxPinLive(ctx, trust.ProjectID, trust.SandboxID, trust.SandboxGeneration)
}

// sandboxPinLive is the generation gate shared by grants and trusts: the
// sandbox exists as a runtime (not archived/deleted) and its current
// generation is the one the authorization was approved for.
func (s *Store) sandboxPinLive(ctx context.Context, projectID, sandboxID string, generation int64) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var row struct {
		State        string
		Generation   int64
		DesiredState string
	}
	result := read.Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		Select("state", "generation", "desired_state").Limit(1).
		Scan(&row)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	if row.State == model.SandboxStateArchived || row.State == model.SandboxStateDeleted ||
		row.DesiredState == model.DesiredStateArchived || row.DesiredState == model.DesiredStateDeleted {
		return false, nil
	}
	return row.Generation == generation, nil
}

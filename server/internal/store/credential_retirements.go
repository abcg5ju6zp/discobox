package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Credential retirements (ADR 26-10-15-871): the outbox of pool-local
// invalidations. Enqueue helpers run inside the transaction of the event that
// retires the authorization, so an event can never commit without its items.
// The pool lists pending items for its sandboxes, applies them locally, and
// acknowledges the IDs it applied; the ack is the only state the pool writes
// back, and it is idempotent.

// pendingCredentialRetirementLimit bounds one pool's read; a pool that falls
// behind pages through the backlog across polls.
const pendingCredentialRetirementLimit = 500

// ListPendingCredentialRetirements returns the items a pool has not yet
// acknowledged, oldest first. The live grant/trust reads are authoritative;
// these only drive prompt local invalidation, so the read never errors on age
// or duplication.
func (s *Store) ListPendingCredentialRetirements(ctx context.Context, poolID string) ([]model.CredentialRetirement, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	out := []model.CredentialRetirement{}
	err = read.Where("pool_id = ? AND acked_at IS NULL", poolID).
		Order("created_at ASC").Order("id ASC").
		Limit(pendingCredentialRetirementLimit).
		Find(&out).Error
	return out, err
}

// AcknowledgeCredentialRetirements marks the named items applied. It is
// scoped to the pool acknowledging them and is idempotent: unknown or
// already-acked IDs are acknowledged again, so a lost acknowledgement retried
// acks nothing new.
func (s *Store) AcknowledgeCredentialRetirements(ctx context.Context, poolID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return write.Model(&model.CredentialRetirement{}).
		Where("pool_id = ? AND id IN ?", poolID, ids).
		Update("acked_at", now).Error
}

// enqueueSandboxUseRetirements writes one use item for every pinned use the
// sandbox's approval-minted grants carry. Called in the transaction that
// rebuilds, archives, or deletes the sandbox, so the sandbox (and its pool ID)
// is still readable.
func enqueueSandboxUseRetirements(tx *gorm.DB, sandbox *model.Sandbox, reason string) error {
	var grants []model.SecretGrant
	if err := tx.Where("scope = ? AND scope_key = ? AND sandbox_generation > 0",
		model.SecretGrantScopeSandbox, sandbox.ID).Find(&grants).Error; err != nil {
		return err
	}
	rows := make([]model.CredentialRetirement, 0, len(grants))
	for i := range grants {
		for _, use := range grants[i].Uses {
			if use.UseID == "" {
				continue
			}
			rows = append(rows, model.CredentialRetirement{
				ProjectID: sandbox.ProjectID,
				PoolID:    sandbox.PoolID,
				SandboxID: sandbox.ID,
				Kind:      model.CredentialRetirementKindUse,
				Reason:    reason,
				TargetID:  use.UseID,
			})
		}
	}
	return createRetirementRows(tx, rows)
}

// enqueueSecretUseRetirements writes a credential_replaced item for every
// pinned grant of the secret, resolving each sandbox's pool. Unpinned grants
// (static ones) carry sandbox_generation zero and are deliberately not
// retired: a standing grant survives replacement as it always has.
func enqueueSecretUseRetirements(tx *gorm.DB, projectID, secretID string) error {
	var grants []model.SecretGrant
	if err := tx.Where("secret_id = ? AND sandbox_generation > 0", secretID).Find(&grants).Error; err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	sandboxIDs := make([]string, 0, len(grants))
	seen := map[string]struct{}{}
	for i := range grants {
		if _, ok := seen[grants[i].ScopeKey]; !ok {
			seen[grants[i].ScopeKey] = struct{}{}
			sandboxIDs = append(sandboxIDs, grants[i].ScopeKey)
		}
	}
	var sandboxes []model.Sandbox
	if err := tx.Select("id", "project_id", "pool_id").Where("id IN ?", sandboxIDs).Find(&sandboxes).Error; err != nil {
		return err
	}
	pools := make(map[string]string, len(sandboxes))
	for i := range sandboxes {
		pools[sandboxes[i].ID] = sandboxes[i].PoolID
	}
	rows := []model.CredentialRetirement{}
	for i := range grants {
		poolID := pools[grants[i].ScopeKey]
		if poolID == "" {
			// The sandbox is gone; its grants authorize nothing and its pool
			// has no state to invalidate.
			continue
		}
		for _, use := range grants[i].Uses {
			if use.UseID == "" {
				continue
			}
			rows = append(rows, model.CredentialRetirement{
				ProjectID: projectID,
				PoolID:    poolID,
				SandboxID: grants[i].ScopeKey,
				Kind:      model.CredentialRetirementKindUse,
				Reason:    model.CredentialRetirementReasonCredentialReplaced,
				TargetID:  use.UseID,
			})
		}
	}
	return createRetirementRows(tx, rows)
}

// enqueueTrustRetirement writes one trust item for a revoked host trust,
// targeting the trust itself so the pool drops its pin and the trust's uses.
func enqueueTrustRetirement(tx *gorm.DB, trust *model.HostTrust) error {
	if trust.SandboxGeneration == 0 || len(trust.Uses) == 0 {
		return nil
	}
	var sandbox model.Sandbox
	if err := tx.Select("id", "pool_id").Where("id = ?", trust.SandboxID).First(&sandbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	return createRetirementRows(tx, []model.CredentialRetirement{{
		ProjectID: trust.ProjectID,
		PoolID:    sandbox.PoolID,
		SandboxID: trust.SandboxID,
		Kind:      model.CredentialRetirementKindTrust,
		Reason:    model.CredentialRetirementReasonTrustRevoked,
		TargetID:  trust.ID,
	}})
}

func createRetirementRows(tx *gorm.DB, rows []model.CredentialRetirement) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.Create(&rows).Error
}

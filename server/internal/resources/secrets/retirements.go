package secrets

import (
	"context"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The pool-facing side of credential retirements (ADR 26-10-15-871). The
// server's live grant/trust reads are the authority; these methods only serve
// the outbox that makes a pool drop its local caches promptly.

// ListCredentialRetirements returns the local invalidations a pool has not yet
// acknowledged, oldest first. The store bounds one read; a pool pages across a
// backlog across polls.
func (s *Service) ListCredentialRetirements(ctx context.Context, poolID string) ([]model.CredentialRetirement, error) {
	if strings.TrimSpace(poolID) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool ID is required")
	}
	return s.store.ListPendingCredentialRetirements(ctx, poolID)
}

// AcknowledgeCredentialRetirements marks items the pool applied. Unknown or
// already-acknowledged IDs are acknowledged again, so a retried
// acknowledgement and a pool that applied an item twice are both fine.
func (s *Service) AcknowledgeCredentialRetirements(ctx context.Context, poolID string, retirementIDs []string) error {
	if strings.TrimSpace(poolID) == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "pool ID is required")
	}
	return s.store.AcknowledgeCredentialRetirements(ctx, poolID, retirementIDs)
}

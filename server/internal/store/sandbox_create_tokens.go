package store

import (
	"context"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// GetSandboxCreateToken reads a project's idempotency record for one
// client-minted create token. ErrNotFound is an unseen token.
func (s *Store) GetSandboxCreateToken(ctx context.Context, projectID, token string) (*model.SandboxCreateToken, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var record model.SandboxCreateToken
	if err := read.First(&record, "project_id = ? AND token = ?", projectID, token).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &record, nil
}

// CreateSandboxCreateToken records the sandbox a token's first request made
// and the fingerprint of the body it carried, in the create's transaction.
func (s *Store) CreateSandboxCreateToken(ctx context.Context, record *model.SandboxCreateToken) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(record).Error
}

// DeleteSandboxCreateToken removes a token's record. A missing row is not an
// error: replay cleanup is idempotent.
func (s *Store) DeleteSandboxCreateToken(ctx context.Context, projectID, token string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Delete(&model.SandboxCreateToken{}, "project_id = ? AND token = ?", projectID, token).Error
}

// DeleteExpiredSandboxCreateTokens reaps token records older than before. It
// is called opportunistically on a create, so no sweeper owns the table.
func (s *Store) DeleteExpiredSandboxCreateTokens(ctx context.Context, before time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Delete(&model.SandboxCreateToken{}, "created_at < ?", before).Error
}

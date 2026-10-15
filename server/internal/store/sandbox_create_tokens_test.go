package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestSandboxCreateTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	record := &model.SandboxCreateToken{
		ProjectID:       "project-1",
		Token:           "tok-1",
		BodyFingerprint: "fp-1",
		SandboxID:       "sbx-1",
	}
	if err := s.CreateSandboxCreateToken(ctx, record); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetSandboxCreateToken(ctx, "project-1", "tok-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SandboxID != "sbx-1" || got.BodyFingerprint != "fp-1" || got.CreatedAt.IsZero() {
		t.Fatalf("record = %+v", got)
	}

	// The unique index is (project, token), so the same token in another
	// project is independent.
	if err := s.CreateSandboxCreateToken(ctx, &model.SandboxCreateToken{
		ProjectID: "project-1", Token: "tok-1", BodyFingerprint: "fp-2", SandboxID: "sbx-2",
	}); err == nil {
		t.Fatal("a second row with the same project and token was accepted")
	}
	if err := s.CreateSandboxCreateToken(ctx, &model.SandboxCreateToken{
		ProjectID: "project-2", Token: "tok-1", BodyFingerprint: "fp-3", SandboxID: "sbx-3",
	}); err != nil {
		t.Fatalf("the same token in another project: %v", err)
	}

	if err := s.DeleteSandboxCreateToken(ctx, "project-1", "tok-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSandboxCreateToken(ctx, "project-1", "tok-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete = %v, want ErrNotFound", err)
	}
	// Deleting again is idempotent.
	if err := s.DeleteSandboxCreateToken(ctx, "project-1", "tok-1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDeleteExpiredSandboxCreateTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateSandboxCreateToken(ctx, &model.SandboxCreateToken{
		ProjectID: "project-1", Token: "one", BodyFingerprint: "fp", SandboxID: "sbx-1",
	}); err != nil {
		t.Fatal(err)
	}
	// Every current row is older than a cutoff an hour ahead, so the sweep
	// removes it.
	if err := s.DeleteExpiredSandboxCreateTokens(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := s.GetSandboxCreateToken(ctx, "project-1", "one"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row = %v, want it swept", err)
	}

	// A cutoff in the past sweeps nothing that exists now.
	if err := s.CreateSandboxCreateToken(ctx, &model.SandboxCreateToken{
		ProjectID: "project-1", Token: "two", BodyFingerprint: "fp", SandboxID: "sbx-2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExpiredSandboxCreateTokens(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("past sweep: %v", err)
	}
	if _, err := s.GetSandboxCreateToken(ctx, "project-1", "two"); err != nil {
		t.Fatalf("a fresh row was swept: %v", err)
	}
}

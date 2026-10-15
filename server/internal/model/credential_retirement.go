package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/x/id"
)

// Credential retirements (ADR 26-10-15-871): the pool-local invalidation a
// pool owes when an approval-minted authorization ends before its TTL — a
// credential a person replaced, a sandbox rebuilt/archived/deleted, or a host
// trust revoked. The live grant and trust reads on the server are
// authoritative; a retirement item only makes the pool that hosted the
// sandbox drop its cached activations, positive value cache entries, and
// trust pin promptly.
//
// The item targets one pool-side thing: an approved use (KindUse, TargetID is
// the use ID — one item per use) or a host trust (KindTrust, TargetID is the
// trust ID — one item drops the pin and the trust's uses). The pool lists
// pending items, applies them locally, and acknowledges the IDs it applied;
// acknowledgement is idempotent and one item's failure never blocks another
// target.

// idPrefixCredentialRetirement identifies one retirement item. It is this
// package's own, like the host trust prefixes: nothing outside the server
// mints one.
const idPrefixCredentialRetirement = "crt"

const (
	// CredentialRetirementKindUse retires one approved use (its activations
	// and positive cache entries).
	CredentialRetirementKindUse = "use"
	// CredentialRetirementKindTrust retires one host trust (its proxy pin and
	// the trust's uses).
	CredentialRetirementKindTrust = "trust"
)

const (
	// CredentialRetirementReasonCredentialReplaced: a person replaced the
	// credential the uses were approved against.
	CredentialRetirementReasonCredentialReplaced = "credential_replaced" //nolint:gosec // G101: contract reason vocabulary, not a credential
	// CredentialRetirementReasonSandboxGeneration: the sandbox moved to a new
	// run (rebuild, upgrade, repair).
	CredentialRetirementReasonSandboxGeneration = "sandbox_generation"
	// CredentialRetirementReasonSandboxArchived: the sandbox was archived.
	CredentialRetirementReasonSandboxArchived = "sandbox_archived"
	// CredentialRetirementReasonSandboxDeleted: the sandbox was deleted.
	CredentialRetirementReasonSandboxDeleted = "sandbox_deleted"
	// CredentialRetirementReasonTrustRevoked: the host trust was revoked.
	CredentialRetirementReasonTrustRevoked = "trust_revoked"
)

// CredentialRetirement is one pending pool-local invalidation, addressed to
// the pool that hosts the sandbox.
type CredentialRetirement struct {
	// ID is the item ID the pool acknowledges after applying it.
	ID string `gorm:"primaryKey;type:text" json:"id" doc:"Retirement item ID, used when acknowledging"`
	// ProjectID and PoolID route the item; neither is serialized to the pool.
	ProjectID string `gorm:"column:project_id;not null;type:text;index" json:"-"`
	PoolID    string `gorm:"column:pool_id;not null;type:text;index" json:"-"`
	SandboxID string `gorm:"column:sandbox_id;not null;type:text" json:"sandboxId" doc:"The sandbox whose local pool state the item retires"`
	// Kind says what TargetID names.
	Kind string `gorm:"column:kind;not null;type:text" json:"kind" doc:"What targetId names: a use or a trust" enum:"use,trust"`
	// Reason says why the authorization retired.
	Reason string `gorm:"column:reason;not null;type:text" json:"reason" doc:"Why the authorization retired" enum:"credential_replaced,sandbox_generation,sandbox_archived,sandbox_deleted,trust_revoked"`
	// TargetID is the approved use ID (KindUse) or the host trust ID
	// (KindTrust) the pool drops locally.
	TargetID  string     `gorm:"column:target_id;not null;type:text" json:"targetId" doc:"The approved use ID or host trust ID the pool drops"`
	AckedAt   *time.Time `gorm:"column:acked_at;type:datetime;index" json:"-"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"createdAt,omitempty" format:"date-time"`
}

func (CredentialRetirement) TableName() string { return "credential_retirements" }

func (r *CredentialRetirement) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		if r.ID, err = id.New(idPrefixCredentialRetirement); err != nil {
			return err
		}
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	} else {
		r.CreatedAt = r.CreatedAt.UTC()
	}
	return nil
}

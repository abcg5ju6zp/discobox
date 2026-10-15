package database

import (
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
)

// dropSandboxPoolConstraint removes the database-level foreign key from
// sandboxes.pool_id to pools (ADR 26-10-15-871). Pool membership is enforced
// where it is used — every broker/resolve call re-derives that the pool owns
// the sandbox — and pool lifecycle is an empty-pool delete plus re-registration
// under the same ID; the table constraint coupled the two DDL lifetimes for
// nothing. The GORM relation stays (it still serves Preload); only the
// constraint is gone. Runs on every start but drops once.
func dropSandboxPoolConstraint(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasConstraint(&model.Sandbox{}, "fk_sandboxes_pool") {
		return nil
	}
	return migrator.DropConstraint(&model.Sandbox{}, "fk_sandboxes_pool")
}

// backfillCredentialPins upgrades rows written before approval pinning (ADR
// 26-10-15-871):
//
//   - Every secret starts on value version 1.
//   - A grant an agent's approval minted — a sandbox-scoped grant referenced by
//     a secret request — is pinned to version 1 and to its sandbox's current
//     generation, so it keeps authorizing the run already using it and dies on
//     that sandbox's next rebuild.
//   - Agent credential requests and host trust requests/trusts pin to their
//     sandbox's current generation.
//   - Pre-configured grants that no request references stay unpinned (zero on
//     both): a standing grant keeps the behavior it always had.
//
// The step only ever writes zero/empty rows, so a second run changes nothing.
func backfillCredentialPins(db *gorm.DB) error {
	if err := db.Model(&model.Secret{}).Where("value_version = 0").
		UpdateColumn("value_version", 1).Error; err != nil {
		return err
	}
	// Sandbox generation by ID, for the rows that pin to one.
	var sandboxes []model.Sandbox
	if err := db.Select("id", "generation").Find(&sandboxes).Error; err != nil {
		return err
	}
	generation := make(map[string]int64, len(sandboxes))
	for i := range sandboxes {
		generation[sandboxes[i].ID] = sandboxes[i].Generation
	}

	// Approval grants: sandbox-scoped grants a secret request references.
	var grants []model.SecretGrant
	if err := db.Where("scope = ? AND sandbox_generation = 0", model.SecretGrantScopeSandbox).
		Find(&grants).Error; err != nil {
		return err
	}
	for i := range grants {
		grant := grants[i]
		gen, ok := generation[grant.ScopeKey]
		if !ok {
			continue
		}
		var count int64
		if err := db.Model(&model.SecretRequest{}).
			Where("grant_id = ?", grant.ID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			// A pre-configured grant: leave unpinned.
			continue
		}
		if err := db.Model(&model.SecretGrant{}).Where("id = ?", grant.ID).
			Updates(map[string]any{"secret_version": 1, "sandbox_generation": gen}).Error; err != nil {
			return err
		}
	}

	// Agent credential requests: protocol asks carry uses; reactive and
	// refresh requests carry none and stay unpinned.
	var requests []model.SecretRequest
	if err := db.Where("sandbox_generation = 0 AND sandbox_id <> ''").Find(&requests).Error; err != nil {
		return err
	}
	for i := range requests {
		req := requests[i]
		if len(req.Uses) == 0 {
			continue
		}
		gen, ok := generation[req.SandboxID]
		if !ok {
			continue
		}
		if err := db.Model(&model.SecretRequest{}).Where("id = ? AND sandbox_generation = 0", req.ID).
			UpdateColumn("sandbox_generation", gen).Error; err != nil {
			return err
		}
	}

	// Host trusts and trust requests. A trust always carries uses; still guard
	// the requests, which are what an older row could in principle lack.
	var trusts []model.HostTrust
	if err := db.Where("sandbox_generation = 0").Find(&trusts).Error; err != nil {
		return err
	}
	for i := range trusts {
		trust := trusts[i]
		gen, ok := generation[trust.SandboxID]
		if !ok {
			continue
		}
		if err := db.Model(&model.HostTrust{}).Where("id = ? AND sandbox_generation = 0", trust.ID).
			UpdateColumn("sandbox_generation", gen).Error; err != nil {
			return err
		}
	}
	var trustRequests []model.HostTrustRequest
	if err := db.Where("sandbox_generation = 0 AND sandbox_id <> ''").Find(&trustRequests).Error; err != nil {
		return err
	}
	for i := range trustRequests {
		req := trustRequests[i]
		if len(req.Uses) == 0 {
			continue
		}
		gen, ok := generation[req.SandboxID]
		if !ok {
			continue
		}
		if err := db.Model(&model.HostTrustRequest{}).Where("id = ? AND sandbox_generation = 0", req.ID).
			UpdateColumn("sandbox_generation", gen).Error; err != nil {
			return err
		}
	}
	return nil
}

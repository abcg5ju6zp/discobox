package secrets

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// Idempotent approval replay (ADR 26-10-15-871). An approve that finds its
// request already decided does not mint a second grant or a second set of use
// IDs: it is checked against the recorded decision and, when it matches, the
// recorded request is returned with its original grant and original use IDs.
// A retry after a lost response, a duplicate click, or a restart all replay;
// a body that asks for a different decision is refused as a conflict — the
// request has one answer.

func (s *Service) replaySecretRequest(ctx context.Context, req *model.SecretRequest, input services.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	if req.Status == model.SecretRequestStatusDenied {
		return nil, apperrors.NewStatusError(http.StatusConflict, "secret request is already denied")
	}
	if req.Status != model.SecretRequestStatusApproved || req.GrantID == "" {
		// In some other state mid-transition: ask the caller to re-read.
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}
	grant, err := s.store.GetSecretGrant(ctx, req.ProjectID, req.GrantID)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("secret request was approved as grant %s, but that grant is gone; refresh and review its grants", req.GrantID))
	}
	if err := replayMatchesGrant(ctx, s, req, grant, input); err != nil {
		return nil, err
	}
	return req, nil
}

func replayMatchesGrant(ctx context.Context, s *Service, req *model.SecretRequest, grant *model.SecretGrant, input services.ApproveSecretRequestBody) error {
	// The secret the replay names has to be the one the first approval
	// answered; an empty name replays whatever was recorded.
	if named := input.SecretId.Or(""); named != "" && named != req.SecretID {
		return replayConflict(req.ID, "secret", named, req.SecretID)
	}
	// Scope and scope key resolve the same way a first approval resolves
	// them, so a defaulted scope replays the recorded one.
	scope := string(input.Scope.Or(""))
	if scope == "" {
		if req.SandboxID != "" {
			scope = model.SecretGrantScopeSandbox
		} else {
			scope = model.SecretGrantScopeProject
		}
	}
	if scope != grant.Scope {
		return replayConflict(req.ID, "scope", scope, grant.Scope)
	}
	scopeKey, err := s.grantScopeKey(ctx, req.ProjectID, req.SandboxID, scope)
	if err != nil {
		return err
	}
	if scopeKey != grant.ScopeKey {
		return replayConflict(req.ID, "scope target", scopeKey, grant.ScopeKey)
	}
	// Hosts: the replay's names, else the request's, compared as sets the way
	// grants store them.
	hosts, named := askedHosts(input.Hosts)
	if !named {
		hosts = req.Hosts
	}
	if !sameStringSet(hosts, grant.Hosts) {
		return replayConflict(req.ID, "hosts", joinStrings(hosts), joinStrings(grant.Hosts))
	}
	// Uses: only protocol requests carry them.
	if req.FromProtocol() {
		descriptions := useDescriptions(req.Uses)
		if edited, ok := input.Uses.Get(); ok && len(edited) > 0 {
			descriptions = useDescriptions(convertAPIUses(edited))
		}
		if !sameStringSet(descriptions, useDescriptions(grant.Uses)) {
			return replayConflict(req.ID, "uses", joinStrings(descriptions), joinStrings(useDescriptions(grant.Uses)))
		}
	}
	// TTL: the granted duration has to be the one the replay asks. When the
	// replay leaves it to the default, re-derive that default rather than
	// require the body to repeat it.
	grantedSeconds := int64(0)
	if grant.ExpiresAt != nil {
		grantedSeconds = int64(grant.ExpiresAt.Sub(grant.GrantedAt) / time.Second)
	}
	if namedTTL, ok := input.GrantTTLSeconds.Get(); ok {
		if !ttlsEqual(namedTTL, grantedSeconds) {
			return replayConflict(req.ID, "grantTTLSeconds", fmt.Sprintf("%d", namedTTL), fmt.Sprintf("%d", grantedSeconds))
		}
	} else {
		secret, err := s.store.GetSecret(ctx, req.ProjectID, req.SecretID)
		if err != nil {
			return apperrors.NotFound(err, "secret not found")
		}
		if !ttlsEqual(defaultApprovalTTL(req, secret), grantedSeconds) {
			return replayConflict(req.ID, "grantTTLSeconds", fmt.Sprintf("%d (default)", defaultApprovalTTL(req, secret)), fmt.Sprintf("%d", grantedSeconds))
		}
	}
	return nil
}

// replayTrustRequest is the same replay for a host trust approval.
func (s *Service) replayTrustRequest(ctx context.Context, req *model.HostTrustRequest, input services.ApproveTrustRequestBody) (*model.HostTrustRequest, error) {
	if req.Status == model.HostTrustRequestStatusDenied {
		return nil, apperrors.NewStatusError(http.StatusConflict, "trust request is already denied")
	}
	if req.Status != model.HostTrustRequestStatusApproved || req.TrustID == "" {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("trust request is already %s", req.Status))
	}
	trust, err := s.store.GetHostTrust(ctx, req.ProjectID, req.TrustID)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("trust request was approved as trust %s, but that trust is gone; refresh and review it", req.TrustID))
	}
	pin := DefaultTrustPin(req)
	if chosen, ok := input.Pin.Get(); ok && strings.TrimSpace(string(chosen.Kind)) == model.TrustPinKindCA && strings.TrimSpace(chosen.SHA256) != "" {
		pin = model.TrustPin{Kind: strings.TrimSpace(string(chosen.Kind)), SHA256: strings.ToLower(strings.TrimSpace(chosen.SHA256))}
	}
	if pin.Kind != trust.Pin.Kind || pin.SHA256 != trust.Pin.SHA256 {
		return nil, replayConflict(req.ID, "pin", pin.Kind+":"+pin.SHA256, trust.Pin.Kind+":"+trust.Pin.SHA256)
	}
	descriptions := useDescriptions(req.Uses)
	if edited, ok := input.Uses.Get(); ok && edited != nil {
		descriptions = useDescriptions(convertAPIUses(edited))
	}
	if !sameStringSet(descriptions, useDescriptions(trust.Uses)) {
		return nil, replayConflict(req.ID, "uses", joinStrings(descriptions), joinStrings(useDescriptions(trust.Uses)))
	}
	grantedSeconds := int64(trust.ExpiresAt.Sub(trust.CreatedAt) / time.Second)
	if namedTTL, ok := input.GrantTTLSeconds.Get(); ok {
		if !ttlsEqual(namedTTL, grantedSeconds) {
			return nil, replayConflict(req.ID, "grantTTLSeconds", fmt.Sprintf("%d", namedTTL), fmt.Sprintf("%d", grantedSeconds))
		}
	} else {
		defaultSeconds := req.GrantTTL
		if defaultSeconds == 0 {
			defaultSeconds = int64(model.DefaultHostTrustTTLSeconds)
		}
		if !ttlsEqual(defaultSeconds, grantedSeconds) {
			return nil, replayConflict(req.ID, "grantTTLSeconds", fmt.Sprintf("%d (default)", defaultSeconds), fmt.Sprintf("%d", grantedSeconds))
		}
	}
	return req, nil
}

func replayConflict(id, field, replay, recorded string) error {
	return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
		"request %s was already answered with a different %s (%s); the repeated approval says %s — refresh and work from the recorded answer",
		id, field, recorded, replay))
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]string(nil), a...)
	cb := append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

func joinStrings(values []string) string {
	if len(values) == 0 {
		return ""
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return fmt.Sprint(sorted)
}

// ttlsEqual compares two TTLs in seconds, allowing a second of rounding
// between the grant's stored instant and the seconds the body named.
func ttlsEqual(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1
}

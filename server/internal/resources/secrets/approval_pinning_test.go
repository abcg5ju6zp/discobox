package secrets_test

// Behavior behind ADR 26-10-15-871: an approval is pinned to the sandbox run
// that asked and the credential version that answered; the pin is re-checked
// on every live read, a repeated approval replays the one original answer, and
// the events that end an authorization early enqueue the pool's local
// invalidation in the same transaction.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// bumpSandboxGeneration moves the test sandbox to its next run the way an
// intent does, and returns the generation it lands on. It is the event an
// approval pin is re-checked against.
func bumpSandboxGeneration(ctx context.Context, t *testing.T, st *store.Store) int64 {
	t.Helper()
	sb, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	sb.IncrementGeneration()
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("bump sandbox generation: %v", err)
	}
	return sb.Generation
}

func liveUseIDs(grant *model.SecretGrant) []string {
	ids := make([]string, 0, len(grant.Uses))
	for _, use := range grant.Uses {
		ids = append(ids, use.UseID)
	}
	return ids
}

func retirementTarget(items []model.CredentialRetirement, targetID string) *model.CredentialRetirement {
	for i := range items {
		if items[i].TargetID == targetID {
			return &items[i]
		}
	}
	return nil
}

func liveSandboxCredentials(ctx context.Context, t *testing.T, st *store.Store) []store.AgentCredential {
	t.Helper()
	sb, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	creds, err := st.ListLiveAgentCredentials(ctx, sb.ProjectID, sb.ID, store.SandboxGrantScopes(sb))
	if err != nil {
		t.Fatalf("list live credentials: %v", err)
	}
	return creds
}

// Approving an agent's ask mints a grant pinned to the run that asked and the
// credential version that answered. A rebuild drops it from every live read
// and enqueues the pool's local invalidation; the ack is idempotent.
func TestApprovalPinsRunAndVersionAndRetiresOnRebuild(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	if gen := bumpSandboxGeneration(ctx, t, st); gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if grant.SandboxGeneration != 1 || grant.SecretVersion != 1 {
		t.Fatalf("grant pins = generation %d, version %d; want 1/1", grant.SandboxGeneration, grant.SecretVersion)
	}
	useID := grant.Uses[0].UseID
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "api.github.com"); err != nil {
		t.Fatalf("the approved use is not live for the run that asked: %v", err)
	}

	// A rebuild moves the run. The old run's use stops authorizing on the
	// next read, with the grant row still present.
	bumpSandboxGeneration(ctx, t, st)
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "api.github.com"); err == nil {
		t.Fatal("ApprovedUse() kept naming a use pinned to the rebuilt-away run")
	}
	if len(liveSandboxCredentials(ctx, t, st)) != 0 {
		t.Fatal("the pinned grant still reads live after the generation moved")
	}

	items, err := svc.ListCredentialRetirements(ctx, testPoolID)
	if err != nil {
		t.Fatalf("list retirements: %v", err)
	}
	item := retirementTarget(items, useID)
	if item == nil {
		t.Fatalf("no retirement enqueued for use %s; items = %#v", useID, items)
	}
	if item.Kind != model.CredentialRetirementKindUse || item.Reason != model.CredentialRetirementReasonSandboxGeneration {
		t.Fatalf("retirement = %s/%s, want use/%s", item.Kind, item.Reason, model.CredentialRetirementReasonSandboxGeneration)
	}

	// The pool applies and acknowledges; a retried acknowledgement acks
	// nothing new.
	if err := svc.AcknowledgeCredentialRetirements(ctx, testPoolID, []string{item.ID}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if again, _ := svc.ListCredentialRetirements(ctx, testPoolID); len(again) != 0 {
		t.Fatalf("retirements = %#v, want empty after ack", again)
	}
	if err := svc.AcknowledgeCredentialRetirements(ctx, testPoolID, []string{item.ID}); err != nil {
		t.Fatalf("a repeated acknowledgement must be idempotent: %v", err)
	}
}

// A pending ask is answered for the run that made it. Approving it after the
// sandbox was rebuilt is refused and mints nothing; the ask stays pending so a
// person can deny it.
func TestAnApprovalAfterTheRunMovedIsRefusedAndMintsNothing(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	bumpSandboxGeneration(ctx, t, st)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	bumpSandboxGeneration(ctx, t, st)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	}); err == nil {
		t.Fatal("approval for a rebuilt-away run was accepted")
	} else {
		requireStatus(t, err, http.StatusConflict)
	}

	if len(liveSandboxCredentials(ctx, t, st)) != 0 {
		t.Fatal("the refused approval minted a grant")
	}
	stored, err := st.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if stored.Status != model.SecretRequestStatusPending {
		t.Fatalf("request status = %s, want it left pending", stored.Status)
	}
}

// A repeated approval is a replay of the first: same grant, same use IDs, with
// no second grant minted. A body asking for a different decision is a conflict
// rather than a second answer.
func TestRepeatedApprovalReplaysTheOriginalGrant(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	bumpSandboxGeneration(ctx, t, st)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	body := services.ApproveSecretRequestBody{
		SecretId:        serverapi.NewOptString(secret.ID),
		GrantTTLSeconds: serverapi.NewOptInt64(3600),
	}

	first, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body)
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", first.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	originalUses := liveUseIDs(grant)

	// Duplicate click / retried request after the response was lost.
	replay, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body)
	if err != nil {
		t.Fatalf("the repeated approval must replay, not conflict: %v", err)
	}
	if replay.GrantID != first.GrantID {
		t.Fatalf("replay minted grant %s, want the original %s", replay.GrantID, first.GrantID)
	}
	replayed, err := st.GetSecretGrant(ctx, "project-1", replay.GrantID)
	if err != nil {
		t.Fatalf("get replayed grant: %v", err)
	}
	replayedUses := liveUseIDs(replayed)
	if len(replayedUses) != len(originalUses) {
		t.Fatalf("replayed uses = %v, want %v", replayedUses, originalUses)
	}
	for i, id := range originalUses {
		if replayedUses[i] != id {
			t.Fatalf("replay minted new use IDs %v, want the originals %v", replayedUses, originalUses)
		}
	}
	if creds := liveSandboxCredentials(ctx, t, st); len(creds) != 1 {
		t.Fatalf("live credentials = %d, want exactly one grant after a replay", len(creds))
	}

	// A replay that asks for a different lifetime is one request with two
	// answers, and is refused.
	conflict := body
	conflict.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, conflict); err == nil {
		t.Fatal("a repeated approval with a different TTL was accepted")
	} else {
		requireStatus(t, err, http.StatusConflict)
	}
}

// Replacing the credential value moves its version: approvals pinned to the
// old version stop authorizing and are retired, while a pre-configured static
// grant keeps the behavior it always had.
func TestValueReplacementRetiresPinnedApprovalButKeepsStaticGrant(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	bumpSandboxGeneration(ctx, t, st)
	secret := createBearerSecret(ctx, t, svc)

	// A standing, pre-configured project grant: unpinned on both axes.
	static, err := svc.CreateSecretGrant(ctx, "project-1", projectUseGrant(secret.ID))
	if err != nil {
		t.Fatalf("static grant: %v", err)
	}
	staticGrant, err := st.GetSecretGrant(ctx, "project-1", static.ID)
	if err != nil {
		t.Fatalf("get static grant: %v", err)
	}
	if staticGrant.SandboxGeneration != 0 || staticGrant.SecretVersion != 0 {
		t.Fatalf("a pre-configured grant must stay unpinned, got generation %d version %d",
			staticGrant.SandboxGeneration, staticGrant.SecretVersion)
	}
	staticUse := staticGrant.Uses[0].UseID

	// The agent's approval is pinned to version 1.
	req := createAgentRequest(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	pinned, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get pinned grant: %v", err)
	}
	pinnedUse := pinned.Uses[0].UseID

	for _, useID := range []string{staticUse, pinnedUse} {
		if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "api.github.com"); err != nil {
			t.Fatalf("use %s live before replacement: %v", useID, err)
		}
	}

	// A person replaces the value. Version 2 answers from here on.
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, apimodel.UpdateSecretBody{
		Value: serverapi.NewOptSecretValue(apimodel.SecretValue{Token: serverapi.NewOptString("ghp_brandnewbrandnewbrandnewbrandnewbn99")}),
	}); err != nil {
		t.Fatalf("replace value: %v", err)
	}
	replaced, err := st.GetSecret(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if replaced.ValueVersion != 2 {
		t.Fatalf("value version = %d, want 2 after replacement", replaced.ValueVersion)
	}

	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, pinnedUse, "api.github.com"); err == nil {
		t.Fatal("a use pinned to the replaced-away version still authorizes")
	}
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, staticUse, "api.github.com"); err != nil {
		t.Fatalf("a pre-configured static grant must survive replacement: %v", err)
	}

	items, err := svc.ListCredentialRetirements(ctx, testPoolID)
	if err != nil {
		t.Fatalf("list retirements: %v", err)
	}
	item := retirementTarget(items, pinnedUse)
	if item == nil || item.Reason != model.CredentialRetirementReasonCredentialReplaced {
		t.Fatalf("retirement for the pinned use = %#v, want a credential_replaced item", item)
	}
	if retirementTarget(items, staticUse) != nil {
		t.Fatal("a static grant's use must not be retired on replacement")
	}
}

// A host trust approval answers the run that asked and drops off every live
// read once that run is rebuilt away.
func TestTrustApprovalPinsTheRunAndDiesOnRebuild(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	bumpSandboxGeneration(ctx, t, st)

	// File the ask with the run that made it, the way the trust service does
	// (approveHostTrust writes through the store and stays unpinned).
	req := &model.HostTrustRequest{
		ProjectID: "project-1", SandboxID: testSandboxID, RequestedBy: "agent:" + testSandboxID,
		Host: "kube.internal:6443", Status: model.HostTrustRequestStatusPending,
		SandboxGeneration: 1,
		Uses:              []model.SecretUse{{Description: "read the pods in the prod namespace"}},
		ObservedChain:     []model.ObservedCertificate{{SHA256: strings.Repeat("cd", 32), SPKISHA256: strings.Repeat("ef", 32)}},
	}
	if err := st.CreateHostTrustRequest(ctx, req); err != nil {
		t.Fatalf("create trust request: %v", err)
	}
	if _, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{
		GrantTTLSeconds: serverapi.NewOptInt64(3600),
	}); err != nil {
		t.Fatalf("approve trust: %v", err)
	}
	trusts, err := st.ListLiveSandboxHostTrusts(ctx, "project-1", testSandboxID, time.Now().UTC())
	if err != nil || len(trusts) != 1 {
		t.Fatalf("list live trusts: %v (%d)", err, len(trusts))
	}
	trust := trusts[0]
	if trust.SandboxGeneration != 1 {
		t.Fatalf("trust generation pin = %d, want 1", trust.SandboxGeneration)
	}
	useID := trust.Uses[0].UseID
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "kube.internal"); err != nil {
		t.Fatalf("the trust's use is not live for the run it was approved for: %v", err)
	}

	bumpSandboxGeneration(ctx, t, st)
	if _, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, useID, "kube.internal"); err == nil {
		t.Fatal("ApprovedUse() kept naming the rebuilt-away run's trust use")
	}
	live, err := st.ListLiveSandboxHostTrusts(ctx, "project-1", testSandboxID, time.Now().UTC())
	if err != nil {
		t.Fatalf("list live trusts: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live trusts = %#v, want none pinned to the old run", live)
	}
}

package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/layout"
)

// Credential retirements on the pool (ADR 26-10-15-871). The control plane's
// live grant/trust reads are the authority; this poller makes the pool drop
// local state promptly when an authorization ends early: the in-memory
// activations, the proxy's positive value cache, and — for a trust — its pin.
// It polls a pending list, applies each item independently, acknowledges the
// ones it dropped, and keeps any item it could not apply in a local backoff
// file: one target's failed retirement never blocks another's, and a restart
// re-drives the backlog because unacknowledged rows are listed again.

// retirementRefreshInterval is how long a retired use can remain in pool-local
// state beyond the server event. It is tighter than the trust poll: the
// retirements only drive local drops, and the window they bound is the one in
// which an old use could otherwise keep serving from cache with judging off.
const retirementRefreshInterval = 3 * time.Second

// retirementFailureBackoff bounds retries of a locally un-appliable item
// (essentially, a proxy ApplyConfig failure). It rides the poll tick.
const retirementFailureBackoff = 30 * time.Second

type retirementDoc struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	SandboxID string `json:"sandboxId"`
	TargetID  string `json:"targetId"`
}

type listRetirementsDoc struct {
	Retirements []retirementDoc `json:"retirements"`
}

type acknowledgeRetirementsDoc struct {
	RetirementIDs []string `json:"retirementIds"`
}

func (c *controlPlaneCredentials) listRetirements(ctx context.Context) ([]retirementDoc, error) {
	var out listRetirementsDoc
	if err := c.do(ctx, http.MethodGet, "credential-retirements", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Retirements, nil
}

func (c *controlPlaneCredentials) acknowledgeRetirements(ctx context.Context, ids []string) error {
	payload, err := json.Marshal(acknowledgeRetirementsDoc{RetirementIDs: ids})
	if err != nil {
		return err
	}
	rc, err := readResolveContext(c.contextPath)
	if err != nil || rc.Token == "" {
		return fmt.Errorf("%w: pool has no control-plane credential yet", agentcreds.ErrDenied)
	}
	endpoint := fmt.Sprintf("%s/api/pools/%s/credential-retirements", rc.ControlPlaneURL, url.PathEscape(rc.PoolID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return controlPlaneError(resp)
	}
	return nil
}

// useRetirements applies the pool's pending retirements on a ticker.
type useRetirements struct {
	root        layout.Root
	projectID   string
	poolID      string
	control     *controlPlaneCredentials
	activations *activations
	trusts      *hostTrusts
	server      proxyServerLike
	onError     func(error)

	statePath string

	mu          sync.Mutex
	failedRetry map[string]time.Time
}

// proxyServerLike is the one proxy.Server method the poller uses, declared
// narrowly so the controller's tests can stand in.
type proxyServerLike interface {
	InvalidateUse(useID string)
}

func newUseRetirements(root layout.Root, projectID, poolID string, control *controlPlaneCredentials, activations *activations, trusts *hostTrusts, server proxyServerLike, onError func(error)) *useRetirements {
	return &useRetirements{
		root:        root,
		projectID:   projectID,
		poolID:      poolID,
		control:     control,
		activations: activations,
		trusts:      trusts,
		server:      server,
		onError:     onError,
		statePath:   root.ProxyUseRetirementsFile(projectID, poolID),
		failedRetry: map[string]time.Time{},
	}
}

func (u *useRetirements) run(ctx context.Context) {
	ticker := time.NewTicker(retirementRefreshInterval)
	defer ticker.Stop()
	u.loadFailures()
	for {
		if err := u.refresh(ctx); err != nil && u.onError != nil {
			u.onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type retirementFailureState struct {
	// Failed maps item ID to the time its local application may next be tried.
	Failed map[string]time.Time `json:"failed"`
}

func (u *useRetirements) loadFailures() {
	data, err := os.ReadFile(u.statePath)
	if err != nil {
		return
	}
	var state retirementFailureState
	if err := json.Unmarshal(data, &state); err == nil && state.Failed != nil {
		u.mu.Lock()
		u.failedRetry = state.Failed
		u.mu.Unlock()
	}
}

func (u *useRetirements) saveFailuresLocked() {
	state := retirementFailureState{Failed: u.failedRetry}
	if len(state.Failed) == 0 {
		_ = os.Remove(u.statePath)
		return
	}
	if err := writeJSONAtomic(u.statePath, state); err != nil && u.onError != nil {
		u.onError(err)
	}
}

// pendingFailure reports whether an item is held in local backoff and returns
// true while its retry time has not arrived.
func (u *useRetirements) heldBack(id string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	until, ok := u.failedRetry[id]
	return ok && now.Before(until)
}

func (u *useRetirements) markFailed(id string, now time.Time) {
	u.mu.Lock()
	u.failedRetry[id] = now.Add(retirementFailureBackoff)
	u.saveFailuresLocked()
	u.mu.Unlock()
}

func (u *useRetirements) clearFailure(id string) {
	u.mu.Lock()
	if _, ok := u.failedRetry[id]; ok {
		delete(u.failedRetry, id)
		u.saveFailuresLocked()
	}
	u.mu.Unlock()
}

func (u *useRetirements) refresh(ctx context.Context) error {
	items, err := u.control.listRetirements(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	acked := make([]string, 0, len(items))
	for _, item := range items {
		if u.heldBack(item.ID, now) {
			continue
		}
		if err := u.apply(item); err != nil {
			if u.onError != nil {
				u.onError(fmt.Errorf("apply retirement %s: %w", item.ID, err))
			}
			u.markFailed(item.ID, now)
			continue
		}
		u.clearFailure(item.ID)
		acked = append(acked, item.ID)
	}
	if len(acked) > 0 {
		if err := u.control.acknowledgeRetirements(ctx, acked); err != nil {
			return err
		}
	}
	return nil
}

// apply drops the local state the item names. Each operation is local and
// idempotent; only the trust republish can fail.
func (u *useRetirements) apply(item retirementDoc) error {
	switch item.Kind {
	case "use":
		u.activations.RetireUse(item.TargetID)
		if u.server != nil {
			u.server.InvalidateUse(item.TargetID)
		}
	case "trust":
		if u.trusts != nil {
			return u.trusts.retireTrust(item.TargetID)
		}
	default:
		// An unknown kind is from a newer server; acknowledge rather than
		// retrying forever — the authoritative reads protect regardless.
	}
	return nil
}

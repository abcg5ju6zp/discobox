package proxyagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeProxyServer records the uses the retirement feed asked the proxy to drop
// from its positive cache. It is the narrow proxyServerLike seam.
type fakeProxyServer struct {
	mu          sync.Mutex
	invalidated []string
}

func (f *fakeProxyServer) InvalidateUse(useID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, useID)
}

func (f *fakeProxyServer) uses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.invalidated...)
}

// A use retirement drops the in-memory activation and the proxy's positive
// cache entry, and an item a newer server sent under an unknown kind is
// acknowledged rather than retried forever.
func TestRetirementAppliesAUseLocally(t *testing.T) {
	live := newActivations()
	record, err := live.mint("sb-1", "STABLE", "use-old", []string{"api.github.com"}, "ghp_{base62:36}", nil, 1)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	server := &fakeProxyServer{}
	u := &useRetirements{activations: live, server: server}

	if err := u.apply(retirementDoc{ID: "crt-1", Kind: "use", TargetID: "use-old"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := live.lookup(record.Sentinel); ok {
		t.Fatal("the retired use's activation still resolves")
	}
	if got := server.uses(); len(got) != 1 || got[0] != "use-old" {
		t.Fatalf("invalidated = %v, want [use-old]", got)
	}
	// Idempotent: a re-delivered item drops nothing the second time and still
	// succeeds, which is what lets an ack be retried.
	if err := u.apply(retirementDoc{ID: "crt-1", Kind: "use", TargetID: "use-old"}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	// An unknown kind from a newer server is applied (acknowledged), not held.
	if err := u.apply(retirementDoc{ID: "crt-2", Kind: "future-kind", TargetID: "x"}); err != nil {
		t.Fatalf("an unknown kind should apply, got %v", err)
	}
}

// End to end against a stand-in control plane: the pool lists pending items,
// drops each one's local state, and acknowledges exactly the IDs it applied.
func TestRetirementPollAppliesAndAcknowledges(t *testing.T) {
	root := withTestRoot(t)
	live := newActivations()
	var sentinelByUse = map[string]string{}
	for _, useID := range []string{"use-1", "use-2"} {
		record, err := live.mint("sb-1", "STABLE-"+useID, useID, []string{"api.github.com"}, "ghp_{base62:36}", nil, 1)
		if err != nil {
			t.Fatalf("mint %s: %v", useID, err)
		}
		sentinelByUse[useID] = record.Sentinel
	}
	server := &fakeProxyServer{}

	var mu sync.Mutex
	var acked []string
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(listRetirementsDoc{Retirements: []retirementDoc{
				{ID: "crt-1", Kind: "use", Reason: "credential_replaced", SandboxID: "sb-1", TargetID: "use-1"},
				{ID: "crt-2", Kind: "use", Reason: "sandbox_generation", SandboxID: "sb-1", TargetID: "use-2"},
			}})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var doc acknowledgeRetirementsDoc
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Errorf("ack body: %v", err)
			}
			mu.Lock()
			acked = append(acked, doc.RetirementIDs...)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer plane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, plane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	u := newUseRetirements(root, testProjectID, testPoolID,
		newControlPlaneCredentials(root, testProjectID, testPoolID), live, nil, server, nil)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(acked) != 2 || acked[0] != "crt-1" || acked[1] != "crt-2" {
		t.Fatalf("acked = %v, want [crt-1 crt-2]", acked)
	}
	for useID, sentinel := range sentinelByUse {
		if _, ok := live.lookup(sentinel); ok {
			t.Fatalf("use %s activation still live after retirement", useID)
		}
	}
	if got := server.uses(); len(got) != 2 {
		t.Fatalf("invalidated = %v, want both uses dropped from the cache", got)
	}
}

// An item that failed to apply is held in local backoff and is neither applied
// nor acknowledged, while the rest of the batch still goes through: one
// target's failure never blocks another's.
func TestRetirementBackoffHoldsOneItemAndAcksTheRest(t *testing.T) {
	root := withTestRoot(t)
	live := newActivations()
	held, err := live.mint("sb-1", "STABLE-held", "use-held", []string{"api.github.com"}, "ghp_{base62:36}", nil, 1)
	if err != nil {
		t.Fatalf("mint held: %v", err)
	}
	other, err := live.mint("sb-1", "STABLE-ok", "use-ok", []string{"api.github.com"}, "ghp_{base62:36}", nil, 1)
	if err != nil {
		t.Fatalf("mint other: %v", err)
	}

	var mu sync.Mutex
	var acked []string
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(listRetirementsDoc{Retirements: []retirementDoc{
				{ID: "crt-held", Kind: "use", TargetID: "use-held"},
				{ID: "crt-ok", Kind: "use", TargetID: "use-ok"},
			}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var doc acknowledgeRetirementsDoc
		_ = json.Unmarshal(body, &doc)
		mu.Lock()
		acked = append(acked, doc.RetirementIDs...)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, plane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	u := newUseRetirements(root, testProjectID, testPoolID,
		newControlPlaneCredentials(root, testProjectID, testPoolID), live, nil, &fakeProxyServer{}, nil)
	// Simulate a failed earlier application of crt-held; it rides the backoff.
	u.markFailed("crt-held", time.Now())

	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acked) != 1 || acked[0] != "crt-ok" {
		t.Fatalf("acked = %v, want only [crt-ok]", acked)
	}
	if _, ok := live.lookup(held.Sentinel); !ok {
		t.Fatal("the held-back item must not have been applied")
	}
	if _, ok := live.lookup(other.Sentinel); ok {
		t.Fatal("the other item must have been applied despite the held one")
	}
}

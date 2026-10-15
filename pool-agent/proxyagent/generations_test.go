package proxyagent

import (
	"testing"

	"github.com/discobox-ai/discobox/proxy"
)

// The activation run gate (ADR 26-10-15-871): an activation minted for a run
// resolves only while the generations record still names that run. An
// unpinned activation and an absent channel gate nothing; a moved or removed
// run denies.
func TestActivationGenerationGate(t *testing.T) {
	activation := func(gen int64) activation {
		return activation{SandboxID: "sb-1", SandboxGeneration: gen}
	}
	for _, tc := range []struct {
		name        string
		gen         int64
		known       bool
		generations map[string]int64
		want        bool
	}{
		{"no channel yet gates nothing", 1, false, nil, true},
		{"unpinned activation gates nothing", 0, true, map[string]int64{"sb-1": 9}, true},
		{"same run resolves", 1, true, map[string]int64{"sb-1": 1}, true},
		{"rebuilt run denies", 1, true, map[string]int64{"sb-1": 2}, false},
		{"archived run denies", 1, true, map[string]int64{}, false},
		{"foreign run does not answer", 1, true, map[string]int64{"sb-other": 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activation(tc.gen).generationLive(tc.generations, tc.known); got != tc.want {
				t.Fatalf("generationLive = %v, want %v", got, tc.want)
			}
		})
	}
}

// The resolver reads the generations file the pool agent's create/archive
// calls maintain, and refuses an activation the moment its run has moved or
// been archived.
func TestResolverRefusesAnActivationFromARebuiltOrArchivedRun(t *testing.T) {
	root := withTestRoot(t)
	live := newActivations()
	resolver := newSecretResolver(root, testProjectID, testPoolID, live)

	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", []string{"api.github.com"}, "", nil, 1)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.github.com"}

	// The gate is enabled but the generations record does not name the run
	// yet: a pinned activation fails closed. In production the broker only
	// stamps a positive pin once the record names the run, so this is a run
	// the channel cannot place.
	if _, ok := resolver.activation(req); ok {
		t.Fatal("a pinned activation resolved while the generations record did not name its run")
	}

	if err := UpsertSandboxGeneration(root, testProjectID, testPoolID, "sb-1", 1); err != nil {
		t.Fatalf("write generation: %v", err)
	}
	if _, ok := resolver.activation(req); !ok {
		t.Fatal("the current run's activation was refused")
	}

	// An activation minted before the run was known is pinned zero and rides
	// across the record like a pre-pinning one.
	ungated, err := live.mint("sb-1", "STABLE-UNGATED", "use-0", []string{"api.github.com"}, "", nil, 0)
	if err != nil {
		t.Fatalf("mint ungated: %v", err)
	}
	if _, ok := resolver.activation(proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: ungated.Sentinel, Host: "api.github.com"}); !ok {
		t.Fatal("a zero-pinned activation must not be gated by the generations record")
	}

	if err := UpsertSandboxGeneration(root, testProjectID, testPoolID, "sb-1", 1); err != nil {
		t.Fatalf("write generation: %v", err)
	}
	if _, ok := resolver.activation(req); !ok {
		t.Fatal("the current run's activation was refused")
	}

	// A rebuild moves the run. The old run's activation must not resolve.
	if err := UpsertSandboxGeneration(root, testProjectID, testPoolID, "sb-1", 2); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
	if _, ok := resolver.activation(req); ok {
		t.Fatal("an activation from the rebuilt-away run still resolved")
	}

	// Archive removes the entry outright. From a matching run to no entry is
	// the same refusal.
	if err := RemoveSandboxGeneration(root, testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatalf("remove generation: %v", err)
	}
	if _, ok := resolver.activation(req); ok {
		t.Fatal("an activation resolved after its run was archived")
	}
}

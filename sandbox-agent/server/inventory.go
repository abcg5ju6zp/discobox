package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
)

// inventoryScreensTimeout bounds asking every terminal's shim for its screen.
// The shims are asked together and share the one bound, so shims that do not
// answer cost the inventory their own screens and nothing else, however many
// of them there are: the caller's own timeout is not much longer than this.
const inventoryScreensTimeout = 2 * time.Second

// GetSandboxAgentInventory answers, in one document, what the exec, service
// and tool lists and each terminal's screen answer right now, by asking the
// handlers that serve them (ADR 26-10-11-657 §1). The control plane keeps the
// last one and answers those reads from it while the sandbox is not running,
// so a part is exactly its route's answer, and a part whose route fails is
// absent rather than empty: "no services" and "services not known" are
// different answers to whoever reads a stopped sandbox.
//
// Like the status, it is computed on each call and only ever answers an
// inbound request (ADR 0030).
func (h *handler) GetSandboxAgentInventory(ctx context.Context, params sandboxapi.GetSandboxAgentInventoryParams) (*sandboxapi.SandboxAgentInventory, error) {
	inventory := sandboxapi.SandboxAgentInventory{ObservedAt: time.Now().UTC()}
	if h.execs != nil {
		if execs, err := h.ListSandboxExecs(ctx, sandboxapi.ListSandboxExecsParams{}); err == nil {
			inventory.Execs = execs.Execs
			screens := sandboxapi.SandboxAgentInventoryScreens{}
			screenCtx, cancel := context.WithTimeout(ctx, inventoryScreensTimeout)
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, exec := range execs.Execs {
				if !exec.Tty {
					continue
				}
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					screen, err := h.GetSandboxExecScreen(screenCtx, sandboxapi.GetSandboxExecScreenParams{ExecId: id})
					if err != nil {
						// A terminal whose shim is gone, or is not answering,
						// has no screen to post.
						return
					}
					mu.Lock()
					screens[id] = *screen
					mu.Unlock()
				}(exec.ID)
			}
			wg.Wait()
			cancel()
			inventory.Screens = sandboxapi.NewOptSandboxAgentInventoryScreens(screens)
		}
	}
	if services, err := h.ListSandboxServices(ctx, sandboxapi.ListSandboxServicesParams{}); err == nil {
		inventory.Services = services.Services
	}
	var unavailable statusError
	if tools, err := h.ListSandboxTools(ctx, sandboxapi.ListSandboxToolsParams{}); err == nil {
		inventory.Tools = tools.Tools
	} else if errors.As(err, &unavailable) && unavailable.status == http.StatusNotImplemented {
		// A sandbox with no tool directories declares no tools, and that is
		// known rather than unknown: its clients already read "not available"
		// as an empty layer (ADR 0125 §7). Posted as that, a stopped one
		// still lists the tools of the layers above it.
		inventory.Tools = []sandboxapi.SandboxTool{}
	}
	digest, err := inventoryDigest(inventory)
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	inventory.Digest = digest
	if known, ok := params.Known.Get(); ok && known == digest {
		return &sandboxapi.SandboxAgentInventory{ObservedAt: inventory.ObservedAt, Digest: digest, Unchanged: sandboxapi.NewOptBool(true)}, nil
	}
	return &inventory, nil
}

// inventoryDigest hashes an inventory's parts and nothing else: not when it
// was taken, which differs on every call. The parts are hashed in a canonical
// encoding — the generated encoder writes a map in iteration order, which
// would give one inventory a different digest each time it is asked for.
func inventoryDigest(inventory sandboxapi.SandboxAgentInventory) (string, error) {
	inventory.ObservedAt = time.Time{}
	inventory.Digest = ""
	inventory.Unchanged = sandboxapi.OptBool{}
	encoded, err := inventory.MarshalJSON()
	if err != nil {
		return "", err
	}
	var canonical any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return "", err
	}
	// encoding/json writes a map's keys sorted.
	encoded, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

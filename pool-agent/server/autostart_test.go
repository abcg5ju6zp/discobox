package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// ensureRecorder is a runtime whose only behavior is EnsureSandboxRunning: it
// records whether the route asked to wait, and fails the way a sandbox with no
// container does.
type ensureRecorder struct {
	sandboxruntime.Runtime
	awaited []bool
	err     error
}

func (r *ensureRecorder) EnsureSandboxRunning(_ context.Context, _ string, awaitContainer bool) error {
	r.awaited = append(r.awaited, awaitContainer)
	return r.err
}

func TestAutoStartPreservesStartupFailure(t *testing.T) {
	bindMount := errors.New("invalid mount config: bind source path does not exist")
	for _, tc := range []struct {
		name    string
		need    sandboxNeed
		err     error
		status  int
		proxied bool
	}{
		{"missing bind mount", needsSandbox, bindMount, http.StatusInternalServerError, false},
		{"unknown sandbox", needsSandbox, sandboxruntime.ErrNotFound, http.StatusNotFound, false},
		{"archived sandbox", needsSandbox, sandboxruntime.ErrArchived, http.StatusConflict, false},
		{"missing container", needsSandbox, sandboxruntime.ErrNoContainer, http.StatusConflict, false},
		{"running sandbox", needsSandbox, nil, http.StatusNoContent, true},
		// The pool serves origins from its own files, so a source's origin can
		// be fetched while its sandbox cannot start.
		{"an origin of a sandbox that cannot start", servedByPool, bindMount, http.StatusNoContent, true},
		{"an origin of an archived sandbox", servedByPool, sandboxruntime.ErrArchived, http.StatusConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &sandboxService{runtime: &ensureRecorder{err: tc.err}}
			proxied := false
			router := chi.NewRouter()
			router.Get("/sandboxes/{sandboxId}/attach", service.autoStart(awaitContainer, tc.need, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxied = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandboxes/sbx_1/attach", nil))
			if rec.Code != tc.status || proxied != tc.proxied {
				t.Fatalf("status %d, proxied %v, body %q", rec.Code, proxied, rec.Body.String())
			}
			if !tc.proxied && !strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("startup failure lost: %q", rec.Body.String())
			}
		})
	}
}

// Only exec attach waits for a container to be rebuilt, because it is the only
// route the control plane waits on; everything else is answered at once, so a
// failed sandbox whose container is gone does not hold a transcript read,
// tunnels or git for the whole rebuild wait. (Listing execs, services or tools
// starts nothing at all: TestSandboxReadsNeverStartAStoppedSandbox.)
func TestAutoStartWaitsForAContainerOnlyWhereTheControlPlaneDoes(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, service)
	registerSandboxGitRoutes(router, service)

	base := "/api/project/p/pool/w/sandboxes/sbx_1"
	for path, wantWait := range map[string]bool{
		base + "/execs/ex_1/logs":         false,
		base + "/services/web/logs":       false,
		base + "/execs/ex_1/events":       false,
		base + "/execs/ex_1/attach":       true,
		base + "/tcp/attach":              false,
		base + "/http/8080/":              false,
		base + "/git-repositories/r/info": false,
	} {
		runtime.awaited = nil
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if len(runtime.awaited) != 1 || runtime.awaited[0] != wantWait {
			t.Errorf("%s: awaited = %v, want %v", path, runtime.awaited, wantWait)
		}
		// A sandbox with no container is a conflict the caller can act on,
		// not a missing sandbox.
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "needs repair") {
			t.Errorf("%s: %d %q, want 409 naming repair", path, recorder.Code, recorder.Body.String())
		}
	}
}

// bootingRuntime is a runtime holding one sandbox whose container Docker calls
// running, and whose boot may still be under way. Waiting the boot out ends
// it, leaving the sandbox in afterBoot.
type bootingRuntime struct {
	sandboxruntime.Runtime
	booting   bool
	afterBoot sandboxruntime.Status
	awaited   int
	status    sandboxruntime.Status
}

func (r *bootingRuntime) GetSandbox(context.Context, string) (*sandboxruntime.Sandbox, error) {
	status := r.status
	if status == "" {
		status = sandboxruntime.StatusRunning
	}
	return &sandboxruntime.Sandbox{SandboxID: "sbx_1", Status: status}, nil
}

func (r *bootingRuntime) SandboxBooting(string) bool { return r.booting }

func (r *bootingRuntime) AwaitSandboxBoot(context.Context, string) error {
	r.awaited++
	r.booting = false
	r.status = r.afterBoot
	return nil
}

// A read of what is inside a sandbox is refused, not proxied, while the
// sandbox is still booting: its agent is not answering yet, so passing the read
// through would come back a bare 502 instead of saying why.
func TestRequireRunningRefusesASandboxStillBooting(t *testing.T) {
	served := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
	for name, tc := range map[string]struct {
		booting    bool
		wantServed bool
	}{
		"booting": {booting: true},
		"running": {booting: false, wantServed: true},
	} {
		t.Run(name, func(t *testing.T) {
			served = false
			runtime := &bootingRuntime{booting: tc.booting}
			service := &sandboxService{runtime: runtime}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/screen", service.requireRunning(refuseBooting, next))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/screen", nil))
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (status %d, body %q)", served, tc.wantServed, rec.Code, rec.Body.String())
			}
			if !tc.wantServed && (rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is starting")) {
				t.Fatalf("status %d body %q, want 409 saying it is starting", rec.Code, rec.Body.String())
			}
			if !tc.wantServed && rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "starting" {
				t.Fatalf("%s = %q, want starting", workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
			}
			if runtime.awaited != 0 {
				t.Fatal("a route that refuses a booting sandbox waited its boot out")
			}
		})
	}
}

// The lists wait out a boot something else began, and start nothing: a client
// that has just created or started a discobox lists it next, and must get the
// discobox's own answer rather than what it posted before it last stopped. A
// boot can end without the sandbox up, and then the read is refused as for
// any sandbox that is not running.
func TestRequireRunningWaitsOutABootForTheLists(t *testing.T) {
	for name, tc := range map[string]struct {
		before     sandboxruntime.Status
		afterBoot  sandboxruntime.Status
		wantServed bool
	}{
		"the boot ends with the sandbox up": {afterBoot: sandboxruntime.StatusRunning, wantServed: true},
		// A boot is marked before its container is started.
		"the container has not started yet":   {before: sandboxruntime.StatusStopped, afterBoot: sandboxruntime.StatusRunning, wantServed: true},
		"the boot ends with the sandbox down": {afterBoot: sandboxruntime.StatusStopped},
	} {
		t.Run(name, func(t *testing.T) {
			served := false
			runtime := &bootingRuntime{booting: true, status: tc.before, afterBoot: tc.afterBoot}
			service := &sandboxService{runtime: runtime}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/execs", service.requireRunning(waitOutBoot, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/execs", nil))
			if runtime.awaited != 1 {
				t.Fatalf("boot awaited %d times, want once", runtime.awaited)
			}
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (status %d, body %q)", served, tc.wantServed, rec.Code, rec.Body.String())
			}
			if !tc.wantServed && (rec.Code != http.StatusConflict || rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "stopped") {
				t.Fatalf("status %d, %s = %q; want 409 marked stopped", rec.Code, workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
			}
		})
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/gitbackend"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
	"github.com/discobox-ai/discobox/pool-agent/githttp"
	"github.com/discobox-ai/discobox/sourcecache"
)

const (
	// pinRequestLimit bounds a pin's body. A pin names every ref of a
	// delivery, and a remote can have tens of thousands.
	pinRequestLimit = 32 << 20
	// deliveredTimeout bounds settling a namespace after a push: it runs
	// whether or not the client waited for its answer.
	deliveredTimeout = time.Minute
)

// registerSandboxCacheRoutes serves this pool's Git cache to the control
// plane, which forwards clients' deliveries (ADR 26-10-10-545 §3).
//
// These routes need no sandbox: the request names the cache, and the path
// the namespace, so a delivery lands while its sandbox's image is still being
// pulled. They are the control plane's alone, behind its own token; a sandbox
// reaches its namespace through the origin route, fetch-only.
func registerSandboxCacheRoutes(router chi.Router, service *sandboxService) {
	prefix := "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/"
	router.Handle(prefix+sourcecache.PoolOriginRoute+"/*", service.cacheOriginHandler())
	router.Handle(prefix+sourcecache.PoolRemoteRoute+"/*", service.cacheRemoteHandler())
}

// pinRequest is the refs of a delivery and the object each points at.
type pinRequest struct {
	Refs map[string]string `json:"refs"`
}

// cacheOriginHandler serves one source's namespace of a cache: the pin that
// creates what the cache already holds, and the push that brings the rest.
func (s *sandboxService) cacheOriginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug, suffix, ok := gitbackend.ParseRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		pin := suffix == sourcecache.PinSuffix
		verify := suffix == sourcecache.VerifySuffix
		if pin || verify {
			if r.Method != http.MethodPost {
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
		}
		if err := s.authorizeGitScope(r, func(r *http.Request, claims SignedTokenClaims) error {
			if pin {
				return requireScope(claims, ScopeSandboxWrite)
			}
			return worktreeScope(r, claims)
		}); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		key := strings.TrimSpace(r.Header.Get(sourcecache.KeyHeader))
		if key == "" {
			http.Error(w, "the request names no git cache", http.StatusBadRequest)
			return
		}
		sandboxID := chi.URLParam(r, "sandboxId")
		caller := strings.TrimSpace(r.Header.Get(sourcecache.CallerHeader))
		generation := strings.TrimSpace(r.Header.Get(sourcecache.GenerationHeader))
		store := s.runtime.GitCache()
		path, release, err := store.Use(r.Context(), key)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForCacheError(err))
			return
		}
		defer release()

		if pin {
			var request pinRequest
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, pinRequestLimit))
			if err == nil {
				err = json.Unmarshal(body, &request)
			}
			if err != nil {
				http.Error(w, fmt.Sprintf("read pin request: %v", err), http.StatusBadRequest)
				return
			}
			result, err := store.Pin(r.Context(), path, sandboxID, slug, generation, request.Refs, caller)
			if err != nil {
				http.Error(w, err.Error(), statusCodeForCacheError(err))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
			return
		}
		if verify {
			serveGenerationVerify(w, r, store, path, sandboxID, slug, generation)
			return
		}

		repo := githttp.Repository{Path: path, UID: -1, GID: -1, Namespace: gitcache.Namespace(sandboxID, slug)}
		push := gitbackend.IsReceivePack(r)
		var before gitcache.RemoteView
		switch {
		case push:
			if before, err = store.Receiving(r.Context(), path, sandboxID, slug); err != nil {
				http.Error(w, err.Error(), statusCodeForCacheError(err))
				return
			}
			repo.Config = gitcache.ReceiveConfig()
			// The gate is checked before receive-pack runs: a generation
			// delivery lands only in a namespace its pin fenced, fenced to
			// that same generation.
			if err := store.CheckGenerationPush(path, sandboxID, slug, generation); err != nil {
				http.Error(w, err.Error(), statusCodeForCacheError(err))
				return
			}
			if caller == "" {
				if generation == "" {
					repo.Config = append(repo.Config, gitcache.MemberReceiveConfig()...)
				} else {
					config, err := store.GenerationReceiveConfig()
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					repo.Config = append(repo.Config, config...)
					repo.Env = []string{gitcache.GenerationEnv + "=" + generation}
				}
				break
			}
			config, env, err := store.SandboxReceiveConfig(path, caller, sandboxID, slug, generation)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			repo.Config = append(repo.Config, config...)
			repo.Env = env
		case caller != "":
			// No sandbox role route fetches an origin; were one to arrive, it
			// is held to what the namespace advertises.
			repo.FetchOnly = true
		}
		githttp.ServeBackend(w, r, repo, suffix)
		if push && r.Method == http.MethodPost {
			// Settled whether or not the client is still there to hear of it:
			// the refs are in the cache either way.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deliveredTimeout)
			defer cancel()
			if err := store.Delivered(ctx, path, sandboxID, slug, caller == "", before); err != nil {
				slog.WarnContext(ctx, "settle a delivered git cache namespace", "sandboxId", sandboxID, "source", slug, "error", err)
			}
		}
	})
}

// cacheRemoteHandler serves what a cache remembers of its remote — the
// branches and tags project members last delivered — fetch-only, and none of
// any sandbox's namespace. It is what a client fetches before it fetches the
// remote itself, so that each leg carries only what the next host lacks.
func (s *sandboxService) cacheRemoteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeGitScope(r, worktreeScope); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		_, suffix, ok := gitbackend.ParseRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		// A project member's, never a sandbox's: the control plane names no
		// sandbox role route for it, and one named here anyway is refused.
		if strings.TrimSpace(r.Header.Get(sourcecache.CallerHeader)) != "" {
			http.Error(w, "a cache's record of its remote is not a sandbox's to fetch", http.StatusForbidden)
			return
		}
		key := strings.TrimSpace(r.Header.Get(sourcecache.KeyHeader))
		if key == "" {
			http.Error(w, "the request names no git cache", http.StatusBadRequest)
			return
		}
		path, release, err := s.runtime.GitCache().Use(r.Context(), key)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForCacheError(err))
			return
		}
		defer release()
		githttp.ServeBackend(w, r, githttp.Repository{Path: path, UID: -1, GID: -1, HideNamespaces: true}, suffix)
	})
}

// generationVerifyAnswer is the verify response body.
type generationVerifyAnswer struct {
	Generation string                 `json:"generation"`
	OK         bool                   `json:"ok"`
	Verdict    gitcache.VerifyVerdict `json:"verdict,omitempty"`
}

// serveGenerationVerify answers the control plane's read-only completion gate:
// 200 only when the namespace is exactly the frozen generation; a deficit is a
// 409 naming it, so the sandbox stays awaiting its source.
func serveGenerationVerify(w http.ResponseWriter, r *http.Request, store *gitcache.Store, path, sandboxID, slug, generation string) {
	if strings.TrimSpace(generation) == "" {
		http.Error(w, "verify requires the frozen delivery generation", http.StatusBadRequest)
		return
	}
	verdict, err := store.VerifyGeneration(r.Context(), path, sandboxID, slug, generation)
	if err != nil {
		http.Error(w, err.Error(), statusCodeForCacheError(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if !verdict.OK() {
		status = http.StatusConflict
	}
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(generationVerifyAnswer{Generation: generation, OK: verdict.OK(), Verdict: verdict}); err != nil {
		slog.WarnContext(r.Context(), "encode a generation verify answer", "error", err)
	}
}

func statusCodeForCacheError(err error) int {
	if errors.Is(err, gitcache.ErrInvalid) {
		return http.StatusBadRequest
	}
	if errors.Is(err, gitcache.ErrGenerationMismatch) || errors.Is(err, gitcache.ErrNoGenerationRecord) {
		return http.StatusConflict
	}
	return statusCodeForGitError(err)
}

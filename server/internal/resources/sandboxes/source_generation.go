package sandboxes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/sourcecache"
	"github.com/discobox-ai/discobox/sourcegeneration"
)

// createTokenTTL is how long an idempotent create record lives on the server.
// The client's own record lives the same span; after it a create is new
// again, so expired rows are reaped opportunistically rather than swept.
const createTokenTTL = 7 * 24 * time.Hour

// generationVerifyTimeout bounds one pool's answer to a verify call. The
// check is a local ref/connectivity read, so it is short; a pool that cannot
// answer leaves the completion to be retried.
const generationVerifyTimeout = 30 * time.Second

// resolveSourceGeneration recomputes the create's delivery generation and
// compares it with the one the request carried (ADR 26-10-15-943). A request
// without sources names none and may not name one; a request with sources but
// no generation is an old client on the pre-generation path.
func resolveSourceGeneration(input services.CreateSandboxBody, primary *model.GitSource, refs model.SourceCodeReferences) (*string, error) {
	requested := strings.TrimSpace(input.SourceGeneration.Or(""))
	hasSources := primary != nil || len(refs) > 0
	if !hasSources {
		if requested != "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"a sandbox with no source names no source generation")
		}
		return nil, nil
	}
	if requested == "" {
		return nil, nil
	}
	computed := modelSourceGeneration(primary, refs)
	if computed != requested {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"sourceGeneration does not match the sources this request names; recompute it from the request and create again")
	}
	return &requested, nil
}

// modelSourceGeneration is the generation of the model sources as slug
// defaulting left them: the request's fields, with the primary under its fixed
// slug and each reference under the key it was filed by. Server-only fields
// such as delivery and cache key are not read.
func modelSourceGeneration(primary *model.GitSource, refs model.SourceCodeReferences) string {
	sources := make([]sourcegeneration.Source, 0, 1+len(refs))
	add := func(source *model.GitSource) {
		if source == nil {
			return
		}
		slug := ""
		if source.Slug != nil {
			slug = strings.TrimSpace(*source.Slug)
		}
		record := sourcegeneration.Source{Slug: slug, Root: source.Root()}
		if source.Checkout != nil {
			record.RefType = strings.TrimSpace(orEmpty(source.Checkout.RefType))
			record.RefName = strings.TrimSpace(orEmpty(source.Checkout.RefName))
			record.Commit = strings.ToLower(strings.TrimSpace(orEmpty(source.Checkout.Commit)))
		}
		if source.Workspace != nil {
			record.WorkspaceMode = strings.TrimSpace(source.Workspace.Mode)
			record.WorkspaceBase = strings.ToLower(strings.TrimSpace(orEmpty(source.Workspace.BaseCommit)))
			record.WorkspaceSnapshot = strings.TrimSpace(orEmpty(source.Workspace.SnapshotRef))
		}
		if source.Destination != nil {
			record.Directory = strings.TrimSpace(orEmpty(source.Destination.Directory))
			record.WorkingDirectory = strings.TrimSpace(orEmpty(source.Destination.WorkingDirectory))
		}
		sources = append(sources, record)
	}
	add(primary)
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := refs[key]
		add(&ref)
	}
	return sourcegeneration.Digest(sources)
}

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// createBodyFingerprint is the deterministic fingerprint of a create body,
// computed on the server for both the first request and every replay: JSON
// with the generated struct's fixed field order, map keys sorted by
// encoding/json. It identifies the request, never the client.
func createBodyFingerprint(input services.CreateSandboxBody) (string, error) {
	// Through the pointer: the generated encoder is the only thing that leaves
	// an unset optional field out of a struct walked by value.
	data, err := json.Marshal(&input)
	if err != nil {
		return "", fmt.Errorf("fingerprint the create request: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// replayCreate returns the sandbox a token's first request made when this
// request carries the same token and an identical body. The same token with
// another body is 409; an unseen token is no replay. Expired records are
// reaped first, so an expired token creates again.
func (s *Service) replayCreate(ctx context.Context, projectID, token, fingerprint string) (*model.Sandbox, bool, error) {
	if token == "" {
		return nil, false, nil
	}
	if err := s.store.DeleteExpiredSandboxCreateTokens(ctx, time.Now().UTC().Add(-createTokenTTL)); err != nil {
		slog.WarnContext(ctx, "reap expired sandbox create tokens", "error", err)
	}
	record, err := s.store.GetSandboxCreateToken(ctx, projectID, token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if record.BodyFingerprint != fingerprint {
		return nil, false, apperrors.NewStatusError(http.StatusConflict,
			"create token has already been used with a different request body; send the identical request or mint a new token")
	}
	sandbox, err := s.store.GetSandbox(ctx, projectID, record.SandboxID)
	if err != nil {
		// The first sandbox is hard-deleted: its name and token are free
		// again. Drop the record so the retry creates instead of answering
		// 404 forever.
		if errors.Is(err, store.ErrNotFound) {
			if delErr := s.store.DeleteSandboxCreateToken(ctx, projectID, token); delErr != nil {
				slog.WarnContext(ctx, "delete a create token whose sandbox is gone", "error", delErr)
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	return sandbox, true, nil
}

// verifySourceGeneration asks the hosting pool to verify every cache-delivered
// source's namespace against the sandbox's frozen generation before completion
// is accepted (ADR 26-10-15-943 §4). A pool that finds a missing ref, bad
// snapshot or absent object answers 409 naming it; a pool that cannot answer
// is a 502 the client retries the completion against. Push-only sources of
// pre-cache sandboxes have no namespace here and are checked by their
// reported commits, as before.
func (s *Service) verifySourceGeneration(ctx context.Context, sb *model.Sandbox, generation string) error {
	type pendingSource struct {
		slug string
		key  string
	}
	var pending []pendingSource
	for _, entry := range clientDeliveredSources(sb) {
		if entry.source.Delivery != model.GitSourceDeliveryCache || entry.source.CacheKey == nil {
			continue
		}
		slug := strings.TrimSpace(orEmpty(entry.source.Slug))
		key := strings.TrimSpace(*entry.source.CacheKey)
		if slug == "" || key == "" {
			return apperrors.NewStatusError(http.StatusConflict,
				"a cache-delivered source has no slug or cache key to verify")
		}
		pending = append(pending, pendingSource{slug: slug, key: key})
	}
	if len(pending) == 0 {
		return nil
	}
	lease, leased, err := s.AcquireSandboxHTTPClientForServer(ctx, sb.ProjectID, sb.ID,
		[]string{poolagentauth.ScopeSandboxRead}, services.SandboxRead)
	if err != nil {
		return err
	}
	defer lease.Release()
	for _, source := range pending {
		suffix := fmt.Sprintf("/%s/%s.git%s", sourcecache.PoolOriginRoute, url.PathEscape(source.slug), sourcecache.VerifySuffix)
		target, err := sandboxagentclient.TargetURL(lease.BaseURL, leased.ProjectID, leased.PoolID, leased.ID, suffix)
		if err != nil {
			return err
		}
		if err := postGenerationVerify(ctx, lease, target.String(), source.key, generation); err != nil {
			return fmt.Errorf("verify source %q against its frozen delivery generation: %w", source.slug, err)
		}
	}
	return nil
}

// postGenerationVerify makes one read-only verify call to the pool.
func postGenerationVerify(ctx context.Context, lease *services.HTTPClientLease, target, cacheKey, generation string) error {
	ctx, cancel := context.WithTimeout(ctx, generationVerifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set(sourcecache.KeyHeader, cacheKey)
	req.Header.Set(sourcecache.GenerationHeader, generation)
	resp, err := sandboxagentclient.HTTPClient(lease).Do(req)
	if err != nil {
		return apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("the sandbox's pool could not verify the source: %v", err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	detail := strings.TrimSpace(string(data))
	if resp.StatusCode == http.StatusConflict {
		if detail == "" {
			detail = "the pool's cache does not yet hold the whole frozen delivery generation"
		}
		return apperrors.NewStatusError(http.StatusConflict, detail)
	}
	return apperrors.NewStatusError(http.StatusBadGateway,
		fmt.Sprintf("the sandbox's pool answered the verification with %s: %s", resp.Status, detail))
}

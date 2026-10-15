package sandboxruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

const generationTestGeneration = "abababababababababababababababababababababababababababababababab"

// cacheTestSource is a cache-delivered primary source keyed by key.
func cacheTestSource(key string) sandboxSource {
	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Slug:     workerclient.NewOptString("primary"),
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryCache),
		CacheKey: workerclient.NewOptString(key),
	}
	return sandboxSource{slug: "primary", git: source}
}

func generationWorkRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "--quiet", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "--quiet", "-m", "a")
	return repo, gitOutput(t, repo, "rev-parse", "HEAD")
}

// fetchToTempRef fetches work's main into a throwaway cache ref so a following
// pin sees its objects; the temp ref is removed once the namespace keeps them.
func fetchToTempRef(t *testing.T, cachePath, work string) {
	t.Helper()
	gitOutput(t, cachePath, "fetch", "--quiet", work, "main:refs/cache/test-temp")
}

// A fenced namespace releases the sandbox only once its frozen generation
// verifies; an unfenced namespace keeps the "holds a branch" fallback.
func TestCachedSourceLandedVerifiesTheFrozenGeneration(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := deliveryTestRuntime(t)
	cache := runtime.GitCache()
	key := "remote/github.com/acme/generation"
	path, release, err := cache.Use(ctx, key)
	if err != nil {
		t.Fatalf("use cache: %v", err)
	}
	defer release()

	repo, commit := generationWorkRepo(t)

	// First pin records the generation with nothing to pin yet.
	refs := map[string]string{"refs/heads/main": commit}
	result, err := cache.Pin(ctx, path, "sbx-gen", "primary", generationTestGeneration, refs, "")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if len(result.Missing) != 1 {
		t.Fatalf("missing = %v, want the branch", result.Missing)
	}
	source := cacheTestSource(key)
	if landed, err := runtime.cachedSourceLanded(ctx, key, "sbx-gen", source); err != nil || landed {
		t.Fatalf("landed before the objects arrived = %v, %v; want parked", landed, err)
	}

	// Objects arrive and the same-generation pin creates the frozen refs while
	// the temporary ref still holds them; it is dropped once the namespace
	// keeps them.
	fetchToTempRef(t, path, repo)
	if _, err := cache.Pin(ctx, path, "sbx-gen", "primary", generationTestGeneration, refs, ""); err != nil {
		t.Fatalf("second pin: %v", err)
	}
	gitOutput(t, path, "update-ref", "-d", "refs/cache/test-temp")
	if landed, err := runtime.cachedSourceLanded(ctx, key, "sbx-gen", source); err != nil || !landed {
		t.Fatalf("landed after the frozen generation was complete = %v, %v; want landed", landed, err)
	}

	// A pre-generation namespace with a branch still lands on the old rule.
	gitOutput(t, path, "update-ref", "refs/namespaces/sbx-old-primary/refs/heads/main", commit)
	if landed, err := runtime.cachedSourceLanded(ctx, key, "sbx-old", cacheTestSource(key)); err != nil || !landed {
		t.Fatalf("the old fallback = %v, %v; want landed", landed, err)
	}
}

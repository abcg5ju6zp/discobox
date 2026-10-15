package sandboxes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/sourcegeneration"
)

func strp(value string) *string { return &value }

func apiGitSource(commit string) apimodel.GitSource {
	source := apimodel.GitSource{Kind: apigen.GitSourceKindGit}
	source.SetSlug(apigen.NewOptString("primary"))
	source.SetURL(apigen.NewOptURI(url.URL{Scheme: "https", Host: "github.com", Path: "/example/repo.git"}))
	source.SetCheckout(apigen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apigen.NewOptString(commit),
		RefType: apigen.NewOptString("branch"),
		RefName: apigen.NewOptString("main"),
	}))
	return source
}

func TestResolveSourceGeneration(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	apiSource := apiGitSource(commit)
	body := services.CreateSandboxBody{}
	body.Config.SetSource(apigen.NewOptGitSource(apiSource))

	modelSource := &model.GitSource{
		Kind: "git", Slug: strp("primary"), URL: strp("https://github.com/example/repo.git"),
		Checkout: &model.GitSourceCheckout{Commit: strp(commit), RefType: strp("branch"), RefName: strp("main")},
	}
	want := modelSourceGeneration(modelSource, nil)

	t.Run("matching generation is accepted", func(t *testing.T) {
		body.SetSourceGeneration(apigen.NewOptString(want))
		got, err := resolveSourceGeneration(body, modelSource, nil)
		if err != nil || got == nil || *got != want {
			t.Fatalf("resolve = %v, %v, want %q", got, err, want)
		}
	})

	t.Run("a mismatched generation is refused", func(t *testing.T) {
		body.SetSourceGeneration(apigen.NewOptString(strings.Repeat("b", 64)))
		_, err := resolveSourceGeneration(body, modelSource, nil)
		if statusOf(err) != http.StatusBadRequest {
			t.Fatalf("err = %v, want 400", err)
		}
	})

	t.Run("an old client without one keeps the old path", func(t *testing.T) {
		body := services.CreateSandboxBody{}
		body.Config.SetSource(apigen.NewOptGitSource(apiSource))
		got, err := resolveSourceGeneration(body, modelSource, nil)
		if err != nil || got != nil {
			t.Fatalf("absent generation = %v, %v", got, err)
		}
	})

	t.Run("a no-source body may not name one", func(t *testing.T) {
		body := services.CreateSandboxBody{}
		body.SetSourceGeneration(apigen.NewOptString(want))
		_, err := resolveSourceGeneration(body, nil, nil)
		if statusOf(err) != http.StatusBadRequest {
			t.Fatalf("err = %v, want 400", err)
		}
	})
}

func TestModelSourceGenerationIgnoresServerDecisions(t *testing.T) {
	commit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bare := &model.GitSource{Kind: "git", Slug: strp("primary"), URL: strp("https://github.com/example/repo.git"),
		Checkout: &model.GitSourceCheckout{Commit: strp(commit), RefType: strp("branch"), RefName: strp("main")}}
	decided := *bare
	delivery := model.GitSourceDeliveryCache
	decided.Delivery = delivery
	key := "remote/github.com/example/repo"
	decided.CacheKey = &key
	if modelSourceGeneration(bare, nil) != modelSourceGeneration(&decided, nil) {
		t.Fatal("the server's delivery decision moved the frozen generation")
	}
}

func TestModelSourceGenerationOrdersAndCoversEverySource(t *testing.T) {
	commit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	primary := &model.GitSource{Kind: "git", Slug: strp("primary"), LocalDirectory: strp("/src/primary"),
		Checkout: &model.GitSourceCheckout{Commit: strp(commit)}}
	ref := model.GitSource{Kind: "git", Slug: strp("lib"), LocalDirectory: strp("/src/lib"),
		Checkout: &model.GitSourceCheckout{Commit: strp("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}}
	one := modelSourceGeneration(primary, model.SourceCodeReferences{"/src/lib": ref})
	two := modelSourceGeneration(primary, model.SourceCodeReferences{"/src/lib": ref})
	if one == "" || one != two || !sourcegeneration.Valid(one) {
		t.Fatalf("generation unstable or invalid: %q vs %q", one, two)
	}
	moved := ref
	moved.Checkout = &model.GitSourceCheckout{Commit: strp("cccccccccccccccccccccccccccccccccccccccc")}
	if modelSourceGeneration(primary, model.SourceCodeReferences{"/src/lib": moved}) == one {
		t.Fatal("moving a reference commit did not move the generation")
	}
}

func TestCreateBodyFingerprint(t *testing.T) {
	body := services.CreateSandboxBody{}
	body.Config.SetName("my-box")
	body.SetPoolId(apigen.NewOptString("pool-1"))
	first, err := createBodyFingerprint(body)
	if err != nil || len(first) != 64 {
		t.Fatalf("fingerprint = %q, %v", first, err)
	}
	if second, _ := createBodyFingerprint(body); second != first {
		t.Fatal("the same body fingerprinted differently")
	}
	body.SetPoolId(apigen.NewOptString("pool-2"))
	if other, _ := createBodyFingerprint(body); other == first {
		t.Fatal("changing the body did not change the fingerprint")
	}
}

func TestPostGenerationVerifyMapsPoolAnswers(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{"verified", http.StatusOK, 0},
		{"the namespace is short", http.StatusConflict, http.StatusConflict},
		{"the pool is broken", http.StatusInternalServerError, http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Discobox-Git-Cache"); got != "remote/x" {
					t.Errorf("cache header = %q", got)
				}
				if got := r.Header.Get("X-Discobox-Source-Generation"); got != strings.Repeat("a", 64) {
					t.Errorf("generation header = %q", got)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			lease := transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, server.URL, func() {})
			err := postGenerationVerify(t.Context(), lease, server.URL, "remote/x", strings.Repeat("a", 64))
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				return
			}
			if statusOf(err) != tc.wantStatus {
				t.Fatalf("err = %v, want %d", err, tc.wantStatus)
			}
		})
	}
}

func statusOf(err error) int {
	var status apperrors.StatusError
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

package sourcegeneration

import (
	"net/url"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func optString(value string) apigen.OptString { return apigen.NewOptString(value) }

func httpsURL(value string) url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(err)
	}
	return *parsed
}

func apiSource(commit string, mutates ...func(*apimodel.GitSource)) apimodel.GitSource {
	source := apimodel.GitSource{Kind: apigen.GitSourceKindGit}
	source.SetCheckout(apigen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  optString(commit),
		RefType: optString("branch"),
		RefName: optString("main"),
	}))
	source.SetURL(apigen.NewOptURI(httpsURL("https://github.com/example/repo.git")))
	for _, mutate := range mutates {
		mutate(&source)
	}
	return source
}

func TestDigestStableAndOrdered(t *testing.T) {
	primary := apiSource("1111111111111111111111111111111111111111")
	other := apiSource("2222222222222222222222222222222222222222", func(s *apimodel.GitSource) {
		s.SetSlug(optString("other"))
		s.SetURL(apigen.NewOptURI(httpsURL("https://github.com/example/other.git")))
	})
	primary2 := apiSource("1111111111111111111111111111111111111111")
	other2 := apiSource("2222222222222222222222222222222222222222", func(s *apimodel.GitSource) {
		s.SetSlug(optString("other"))
		s.SetURL(apigen.NewOptURI(httpsURL("https://github.com/example/other.git")))
	})

	first := DigestFromAPI(&primary, map[string]apimodel.GitSource{"other": other})
	again := DigestFromAPI(&primary2, map[string]apimodel.GitSource{"other": other2})
	if first == "" || first != again {
		t.Fatalf("digest not stable: %q vs %q", first, again)
	}
	if len(first) != 64 {
		t.Fatalf("digest is not hex SHA-256: %q", first)
	}

	// Map iteration order must not move the digest: build it repeatedly with
	// different reference sets and compare per-record order invariance.
	reversed := Digest([]Source{
		FromAPI(other),
		FromAPI(apiSource("1111111111111111111111111111111111111111")),
	})
	if reversed != first {
		t.Fatalf("digest depends on source order: %q vs %q", reversed, first)
	}
}

func TestDigestNoSources(t *testing.T) {
	if got := DigestFromAPI(nil, nil); got != "" {
		t.Fatalf("no sources must name no generation, got %q", got)
	}
}

func TestDigestMovesWithEveryFrozenField(t *testing.T) {
	base := DigestFromAPI(nil, map[string]apimodel.GitSource{
		"primary": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	cases := map[string]apimodel.GitSource{
		"commit": apiSource("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		"root": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
			s.SetURL(apigen.NewOptURI(httpsURL("https://github.com/example/fork.git")))
		}),
		"refName": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
			checkout, _ := s.Checkout.Get()
			checkout.SetRefName(optString("develop"))
			s.SetCheckout(apigen.NewOptGitSourceCheckout(checkout))
		}),
		"refType": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
			checkout, _ := s.Checkout.Get()
			checkout.SetRefType(optString("tag"))
			s.SetCheckout(apigen.NewOptGitSourceCheckout(checkout))
		}),
		"workspace": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
			workspace := apimodel.GitSourceWorkspace{}
			workspace.SetMode(apigen.NewOptGitSourceWorkspaceMode(apigen.GitSourceWorkspaceModeDirty))
			workspace.SetBaseCommit(optString("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
			workspace.SetSnapshotRef(optString("refs/discobox/run/snap_1"))
			s.SetWorkspace(apigen.NewOptGitSourceWorkspace(workspace))
		}),
		"destination": apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
			destination := apimodel.GitSourceDestination{}
			destination.SetDirectory(optString("/workspace/elsewhere"))
			s.SetDestination(apigen.NewOptGitSourceDestination(destination))
		}),
	}
	for name, source := range cases {
		if got := DigestFromAPI(nil, map[string]apimodel.GitSource{"primary": source}); got == base {
			t.Errorf("digest unchanged when %s moved", name)
		}
	}
}

func TestDigestIgnoresDeliveryAndCacheKey(t *testing.T) {
	source := apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	before := DigestFromAPI(nil, map[string]apimodel.GitSource{"primary": source})
	source.SetDelivery(apigen.NewOptGitSourceDelivery(apigen.GitSourceDeliveryCache))
	source.SetCacheKey(optString("remote/github.com/example/repo"))
	after := DigestFromAPI(nil, map[string]apimodel.GitSource{"primary": source})
	if before != after {
		t.Fatal("the server's delivery decision moved the client-computed generation")
	}
}

func TestPrimarySlugDefaulted(t *testing.T) {
	named := apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(s *apimodel.GitSource) {
		s.SetSlug(optString("primary"))
	})
	unnamed := apiSource("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if DigestFromAPI(&unnamed, nil) != DigestFromAPI(&named, nil) {
		t.Fatal("an unnamed primary source must digest as the primary slug")
	}
}

func TestValid(t *testing.T) {
	if !Valid("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Error("64 lowercase hex chars must be valid")
	}
	for _, value := range []string{"", "abc", "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
		"g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if Valid(value) {
			t.Errorf("invalid generation accepted: %q", value)
		}
	}
}

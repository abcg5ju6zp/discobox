package sandboxcreate

import (
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sourcegeneration"
)

// bodySourceGeneration is the frozen delivery generation of a built create
// body: every source the request names, ordered by slug, in the canonical
// form the server recomputes independently (ADR 26-10-15-943). It is empty for
// a no-source body.
func bodySourceGeneration(body *apimodel.CreateSandboxBody) string {
	var primary *apimodel.GitSource
	if source, ok := body.Config.Source.Get(); ok {
		primary = &source
	}
	var references map[string]apimodel.GitSource
	if refs, ok := body.Config.SourceCodeReferences.Get(); ok {
		references = map[string]apimodel.GitSource(refs)
	}
	return sourcegeneration.DigestFromAPI(primary, references)
}

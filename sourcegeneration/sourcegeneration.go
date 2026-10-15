// Package sourcegeneration is the frozen delivery generation of a sandbox's
// sources (ADR 26-10-15-943): the SHA-256 of the canonical source records a
// create accepted. The client computes it from the request it sends and the
// server recomputes it independently from the request it receives, so a
// digest that does not come back out of the request is refused.
//
// Only request fields take part: the source's slug and root (a local
// directory, else its remote URL), its checkout ref type/name/commit, its
// workspace mode with the base commit and snapshot ref, and the destination
// and working directories. How the source reaches the pool — delivery kind
// and cache key — is the server's later decision and never enters it.
//
// The digest is hex SHA-256 of one JSON document with a fixed version and
// one record per source in slug order. JSON with a fixed field order is the
// canonical form: its escaping removes every delimiter ambiguity and its
// encoding has not changed across Go versions.
package sourcegeneration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// canonicalVersion is the encoding this digest is defined over. A change to
// what a generation covers is a new version, never an edit to version one.
const canonicalVersion = 1

// workspaceClean is the mode a source with no workspace record names.
const workspaceClean = "clean"

// Source is one source's part of a generation: every request field the
// frozen source is pinned by, and nothing the server decides later.
type Source struct {
	Slug              string `json:"slug"`
	Root              string `json:"root"`
	RefType           string `json:"refType"`
	RefName           string `json:"refName"`
	Commit            string `json:"commit"`
	WorkspaceMode     string `json:"workspaceMode"`
	WorkspaceBase     string `json:"workspaceBase"`
	WorkspaceSnapshot string `json:"workspaceSnapshot"`
	Directory         string `json:"directory"`
	WorkingDirectory  string `json:"workingDirectory"`
}

// Digest is the frozen delivery generation of sources: the hex SHA-256 of
// their canonical records in slug order. It is empty for no sources, which a
// no-source sandbox carries and takes no part in fencing.
func Digest(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}
	ordered := append([]Source(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Slug < ordered[j].Slug })
	document := struct {
		Version int      `json:"version"`
		Sources []Source `json:"sources"`
	}{Version: canonicalVersion, Sources: ordered}
	data, err := json.Marshal(document)
	if err != nil {
		// Every field is a string in a fixed struct: this cannot fail.
		panic(fmt.Sprintf("sourcegeneration: canonical encode: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FromAPI reads one API GitSource's generation record. The slug is the
// source's own, defaulted to the primary source's fixed name: a create's
// primary source carries no slug, and the server records it as "primary"
// before it recomputes the generation, so both ends read it that way.
func FromAPI(source apimodel.GitSource) Source {
	slug := strings.TrimSpace(source.Slug.Or(""))
	if slug == "" {
		slug = sandboxconfig.PrimarySourceSlug
	}
	record := Source{
		Slug:          slug,
		Root:          apiRoot(source),
		WorkspaceMode: workspaceClean,
	}
	if checkout, ok := source.Checkout.Get(); ok {
		record.RefType = strings.TrimSpace(checkout.RefType.Or(""))
		record.RefName = strings.TrimSpace(checkout.RefName.Or(""))
		record.Commit = strings.ToLower(strings.TrimSpace(checkout.Commit.Or("")))
	}
	if workspace, ok := source.Workspace.Get(); ok {
		if mode := strings.TrimSpace(string(workspace.Mode.Or(""))); mode != "" {
			record.WorkspaceMode = mode
		}
		record.WorkspaceBase = strings.ToLower(strings.TrimSpace(workspace.BaseCommit.Or("")))
		record.WorkspaceSnapshot = strings.TrimSpace(workspace.SnapshotRef.Or(""))
	}
	if destination, ok := source.Destination.Get(); ok {
		record.Directory = strings.TrimSpace(destination.Directory.Or(""))
		record.WorkingDirectory = strings.TrimSpace(destination.WorkingDirectory.Or(""))
	}
	return record
}

// DigestFromAPI is the generation of a create's primary source and source
// code references, in one document. A nil source and no references is no
// sources and an empty generation.
func DigestFromAPI(primary *apimodel.GitSource, references map[string]apimodel.GitSource) string {
	var sources []Source
	if primary != nil {
		sources = append(sources, FromAPI(*primary))
	}
	for i := range references {
		sources = append(sources, FromAPI(references[i]))
	}
	return Digest(sources)
}

// apiRoot is the source's root as GitSource.Root reads it: the local
// directory when it names one, else its remote URL.
func apiRoot(source apimodel.GitSource) string {
	if directory := strings.TrimSpace(source.LocalDirectory.Or("")); directory != "" {
		return directory
	}
	if remote, ok := source.URL.Get(); ok {
		return strings.TrimSpace(remote.String())
	}
	return ""
}

// Valid reports whether generation is a well-formed hex SHA-256 digest. It
// validates the shape only: a digest's authority is that it recomputes.
func Valid(generation string) bool {
	generation = strings.TrimSpace(generation)
	if len(generation) != sha256.Size*2 {
		return false
	}
	return strings.Trim(generation, "0123456789abcdef") == ""
}

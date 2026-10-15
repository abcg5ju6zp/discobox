package sandboxcreate

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sourcegeneration"
)

// recordingCreator records every create body and answers each with a sandbox
// carrying the generation the body computed, until its conflicts run out.
type recordingCreator struct {
	bodies    []*apimodel.CreateSandboxBody
	conflicts int
}

func (c *recordingCreator) CreateSandbox(_ context.Context, body *apimodel.CreateSandboxBody, _ apiclientgen.CreateSandboxParams) (apiclientgen.CreateSandboxRes, error) {
	copied := *body
	c.bodies = append(c.bodies, &copied)
	if c.conflicts > 0 {
		c.conflicts--
		return &apiclientgen.ErrorModelStatusCode{StatusCode: 409}, nil
	}
	sandbox := &apimodel.Sandbox{ID: "sbx_generation01"}
	if generation, ok := body.SourceGeneration.Get(); ok {
		sandbox.SetSourceGeneration(apiclientgen.NewOptString(generation))
	}
	return sandbox, nil
}

func useTempStateDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func bodyJSON(t *testing.T, body *apimodel.CreateSandboxBody) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBuildSetsAFrozenSourceGeneration(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	opts := PromptOptions{Platform: platform.Platform{OS: "linux", Arch: "amd64"}, Source: repo}
	body, local, err := BuildPromptSandboxBody(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	generation, ok := body.SourceGeneration.Get()
	if !ok || !sourcegeneration.Valid(generation) {
		t.Fatalf("sourceGeneration = %q, %v", generation, ok)
	}
	// Rebuilding from the unchanged checkout names the same generation.
	again, localAgain, err := BuildPromptSandboxBody(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer localAgain.Close()
	if again.SourceGeneration.Or("") != generation {
		t.Fatal("the generation moved while the sources did not")
	}
}

func TestNoSourceBodyNamesNoGeneration(t *testing.T) {
	body, local, err := BuildPromptSandboxBody(context.Background(),
		PromptOptions{Platform: platform.Platform{OS: "linux", Arch: "amd64"}, Source: newRunSourceTestRepo(t), NoSource: true})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if body.SourceGeneration.IsSet() || body.CreateToken.IsSet() {
		t.Fatal("a no-source body must name no generation or token")
	}
}

func TestCreateReplaysTheRecordedRequestAndToken(t *testing.T) {
	useTempStateDir(t)
	repo := newRunSourceTestRepo(t)
	opts := PromptOptions{Platform: platform.Platform{OS: "linux", Arch: "amd64"}, Source: repo, ServerURL: "https://server.example"}
	creator := &recordingCreator{}

	first, local, err := CreatePromptSandbox(context.Background(), creator, "project-1", opts, nil)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	defer local.Close()
	if len(creator.bodies) != 1 || !creator.bodies[0].CreateToken.IsSet() {
		t.Fatal("the first create did not carry a token")
	}

	// A second create from the same checkout replays the recorded body and
	// token, including the generated name, instead of building a new request.
	second, _, err := CreatePromptSandbox(context.Background(), creator, "project-1", opts, nil)
	if err != nil {
		t.Fatalf("replayed create: %v", err)
	}
	if first.ID != second.ID || len(creator.bodies) != 2 {
		t.Fatalf("replay created a different sandbox: %q vs %q", first.ID, second.ID)
	}
	if got, want := bodyJSON(t, creator.bodies[1]), bodyJSON(t, creator.bodies[0]); got != want {
		t.Fatalf("the replay did not resend the recorded request:\n%s\nwant\n%s", got, want)
	}

	// The record is filed for the server, project and generation.
	files, err := replayTokenFiles()
	if err != nil || len(files) != 1 {
		t.Fatalf("create records = %v, %v", files, err)
	}
	DiscardCreateRecord(opts.ServerURL, "project-1", first.SourceGeneration.Or(""))
	if files, _ := replayTokenFiles(); len(files) != 0 {
		t.Fatalf("the settled create's record survived: %v", files)
	}
}

func TestNameConflictMintsANewTokenForTheNewBody(t *testing.T) {
	useTempStateDir(t)
	repo := newRunSourceTestRepo(t)
	opts := PromptOptions{Platform: platform.Platform{OS: "linux", Arch: "amd64"}, Source: repo, ServerURL: "https://server.example"}
	creator := &recordingCreator{conflicts: 1}

	sandbox, local, err := CreatePromptSandbox(context.Background(), creator, "project-1", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if len(creator.bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(creator.bodies))
	}
	first := creator.bodies[0].CreateToken.Or("")
	second := creator.bodies[1].CreateToken.Or("")
	if first == "" || second == "" || first == second {
		t.Fatalf("tokens %q and %q must differ after the body changed", first, second)
	}
	DiscardCreateRecord(opts.ServerURL, "project-1", sandbox.SourceGeneration.Or(""))
}

func TestAssertLocalGenerationMatchesTheFrozenObjects(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	ctx := context.Background()
	base := runSourceTestGit(t, repo)("rev-parse", "HEAD")

	source := apimodel.GitSource{}
	source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString(base),
		RefType: apiclientgen.NewOptString("branch"),
		RefName: apiclientgen.NewOptString("feature-foo"),
	}))
	if err := assertLocalGeneration(ctx, repo, source); err != nil {
		t.Fatalf("an unchanged branch checkout was refused: %v", err)
	}

	// Move the local branch off the pinned commit.
	cmd := exec.CommandContext(ctx, "git", "commit", "--quiet", "--allow-empty", "-m", "moved")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if err := assertLocalGeneration(ctx, repo, source); err == nil {
		t.Fatal("a branch moved off the pinned commit was accepted")
	}

	// A remote-URL source has no local repository to answer for.
	remote := source
	remote.SetLocalDirectory(apiclientgen.OptString{})
	remote.SetURL(apiclientgen.NewOptURI(parseTestURL(t, "https://github.com/example/repo.git")))
	if err := assertLocalGeneration(ctx, "", remote); err != nil {
		t.Fatalf("a remote source must be exempt: %v", err)
	}
}

func parseTestURL(t *testing.T, value string) (u url.URL) {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return *parsed
}

func TestAssertLocalGenerationRequiresTheSnapshotRef(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	base := git("rev-parse", "HEAD")

	source := apimodel.GitSource{}
	source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString(base),
		RefType: apiclientgen.NewOptString("commit"),
	}))
	workspace := apimodel.GitSourceWorkspace{}
	workspace.SetMode(apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeDirty))
	workspace.SetBaseCommit(apiclientgen.NewOptString(base))
	workspace.SetSnapshotRef(apiclientgen.NewOptString("refs/discobox/run/gone"))
	source.SetWorkspace(apiclientgen.NewOptGitSourceWorkspace(workspace))

	if err := assertLocalGeneration(context.Background(), repo, source); err == nil {
		t.Fatal("a missing dirty-workspace snapshot was accepted")
	}
}

func TestExpiredCreateRecordIsNotReplayed(t *testing.T) {
	useTempStateDir(t)
	repo := newRunSourceTestRepo(t)
	opts := PromptOptions{Platform: platform.Platform{OS: "linux", Arch: "amd64"}, Source: repo, ServerURL: "https://server.example"}
	creator := &recordingCreator{}
	sandbox, local, err := CreatePromptSandbox(context.Background(), creator, "project-1", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	// Age the one record past the TTL.
	files, err := replayTokenFiles()
	if err != nil || len(files) != 1 {
		t.Fatalf("records: %v %v", files, err)
	}
	dir, _ := createsDir()
	path := filepath.Join(dir, files[0])
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record createRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.CreatedAt = record.CreatedAt.Add(-createRecordTTL - 1)
	aged, err := json.Marshal(&record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, aged, 0o600); err != nil {
		t.Fatal(err)
	}

	creator2 := &recordingCreator{}
	again, _, err := CreatePromptSandbox(context.Background(), creator2, "project-1", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.SourceGeneration.Or("") != sandbox.SourceGeneration.Or("") {
		t.Fatal("the generation still identifies the same sources")
	}
	if !creator2.bodies[0].CreateToken.IsSet() {
		t.Fatal("an expired record should be replaced by a fresh token")
	}
}

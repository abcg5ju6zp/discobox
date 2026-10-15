package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func resumeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func resumeRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	resumeGit(t, dir, "init", "--initial-branch=main", "-q")
	resumeGit(t, dir, "config", "user.email", "test@example.com")
	resumeGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumeGit(t, dir, "add", "README.md")
	resumeGit(t, dir, "commit", "-qm", "base")
	return dir, resumeGit(t, dir, "rev-parse", "HEAD")
}

func staleSource(phase applyPhase, repoRoot, head string) *applyPlanSource {
	return &applyPlanSource{
		Slug:       "primary",
		Phase:      phase,
		Strategy:   strategyCherryPick,
		RepoRoot:   repoRoot,
		HeadAtPlan: head,
	}
}

func TestResumeStalenessAgainstARealRepository(t *testing.T) {
	withTempStateDir(t)
	ctx := context.Background()
	app := &App{}
	printer := newApplyPrinter(io.Discard, false)

	repo, head := resumeRepo(t)
	ps := &preparedSource{entry: applySourceEntry{slug: "primary"}, placed: true, repoRoot: repo, head: head}

	if got := app.planStaleness(ctx, printer, ps, staleSource(phaseFetched, repo, head), false); got.Kind != applyStaleNone {
		t.Fatalf("unchanged resume: %s", got.Kind)
	}

	// Local uncommitted work.
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := app.planStaleness(ctx, printer, ps, staleSource(phaseFetched, repo, head), false); got.Kind != applyStaleDirtyWorktree {
		t.Fatalf("dirty resume: got %s, want dirty", got.Kind)
	}
	if got := app.planStaleness(ctx, printer, ps, staleSource(phaseFetched, repo, head), true); got.Kind != applyStaleNone {
		t.Fatalf("--allow-dirty must acknowledge local dirt, got %s", got.Kind)
	}
	os.Remove(filepath.Join(repo, "wip.txt"))

	// Branch advanced with a new local commit.
	if err := os.WriteFile(filepath.Join(repo, "local.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumeGit(t, repo, "add", "local.txt")
	resumeGit(t, repo, "commit", "-qm", "local work")
	newHead := resumeGit(t, repo, "rev-parse", "HEAD")
	if got := app.planStaleness(ctx, printer, ps, staleSource(phaseFetched, repo, head), false); got.Kind != applyStaleBranchAdvanced {
		t.Fatalf("advanced resume: got %s, want branch-advanced", got.Kind)
	}

	// Already landed: work on top of the applied commits is expected, not
	// stale.
	landed := staleSource(phaseLanded, repo, head)
	landed.HostTip = newHead
	if got := app.planStaleness(ctx, printer, ps, landed, false); got.Kind != applyStaleNone {
		t.Fatalf("landed source must not be stale, got %s", got.Kind)
	}

	// Target directory gone.
	removed := t.TempDir()
	psGone := &preparedSource{entry: applySourceEntry{slug: "primary"}, placed: true, repoRoot: filepath.Join(removed, "gone")}
	if got := app.planStaleness(ctx, printer, psGone, staleSource(phaseFetched, psGone.repoRoot, head), false); got.Kind != applyStaleTargetReplaced {
		t.Fatalf("missing target: got %s, want target-replaced", got.Kind)
	}
}

func TestResumeRefusalSupersedesOnlyTheBranchAdvancedCase(t *testing.T) {
	cliDir := withTempStateDir(t)
	planPath := func(id string) string { return filepath.Join(cliDir, "apply", id, "plan.json") }
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	const sandboxID = "01JREFUSE000000000000001"

	repo, head := resumeRepo(t)
	plan := &applyPlan{
		SchemaVersion: applyPlanSchemaVersion,
		applyPlanIdentity: applyPlanIdentity{
			Server: "http://127.0.0.1:0", ProjectID: "default", SandboxID: sandboxID,
		},
		Sources: []*applyPlanSource{staleSource(phaseFetched, repo, head)},
	}
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	report := &applySourceReport{Slug: "primary"}

	// Dirty: the plan stays for --allow-dirty to act on.
	stale := (&App{}).planStaleness(ctx, printer, &preparedSource{repoRoot: repo, head: head}, plan.Sources[0], false)
	if stale.Kind != applyStaleNone {
		t.Fatalf("clean fixture: %s", stale.Kind)
	}
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale = (&App{}).planStaleness(ctx, printer, &preparedSource{repoRoot: repo, head: head}, plan.Sources[0], false)
	out, stop := (&App{}).refuseStalePlan(ctx, printer, plan, plan.Sources[0], stale, report)
	if !stop {
		t.Fatal("a stale refusal must stop the run")
	}
	if out.PlanSuperseded {
		t.Fatal("a dirty refusal must not supersede the plan")
	}
	if _, err := os.Stat(planPath(sandboxID)); err != nil {
		t.Fatalf("dirty refusal removed the plan: %v", err)
	}

	// Branch advanced: the refusal retires the plan in the same run. The
	// dirty file from above goes away so this reads as a clean advance.
	if err := os.Remove(filepath.Join(repo, "wip.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "local.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumeGit(t, repo, "add", "local.txt")
	resumeGit(t, repo, "commit", "-qm", "local work")
	stale = (&App{}).planStaleness(ctx, printer, &preparedSource{repoRoot: repo, head: head}, plan.Sources[0], false)
	out, stop = (&App{}).refuseStalePlan(ctx, printer, plan, plan.Sources[0], stale, report)
	if !stop || !out.PlanSuperseded {
		t.Fatalf("branch refusal: stop=%v superseded=%v, want both true", stop, out.PlanSuperseded)
	}
	if _, err := os.Stat(planPath(sandboxID)); !os.IsNotExist(err) {
		t.Fatalf("branch refusal left the plan in place: %v", err)
	}
	if len(out.NextSteps) != 1 || !strings.Contains(out.NextSteps[0].Commands[0], "discobox apply") {
		t.Fatalf("branch refusal next steps = %+v", out.NextSteps)
	}
}

func TestRefuseStalePlanDiscardsTheConflictWorktree(t *testing.T) {
	withTempStateDir(t)
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	const sandboxID = "01JREFUSE000000000000002"

	host, head := resumeRepo(t)
	wt := filepath.Join(t.TempDir(), "retained")
	resumeGit(t, host, "worktree", "add", "--detach", wt, head)
	resumeGit(t, wt, "commit", "--allow-empty", "-qm", "picked")

	plan := &applyPlan{
		SchemaVersion: applyPlanSchemaVersion,
		applyPlanIdentity: applyPlanIdentity{
			Server: "http://127.0.0.1:0", ProjectID: "default", SandboxID: sandboxID,
		},
		Sources: []*applyPlanSource{{
			Slug: "primary", Phase: phaseConflict, Strategy: strategyCherryPick,
			RepoRoot: host, HeadAtPlan: head, WorktreeDir: wt,
		}},
	}
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Advancing the host branch forces the rerun remedy, which discards the
	// retained conflict worktree as part of superseding.
	resumeGit(t, host, "commit", "--allow-empty", "-qm", "host moved")
	ps := &preparedSource{repoRoot: host, head: head}
	stale := (&App{}).planStaleness(ctx, printer, ps, plan.Sources[0], false)
	if stale.Kind != applyStaleBranchAdvanced {
		t.Fatalf("stale = %s", stale.Kind)
	}
	if _, stop := (&App{}).refuseStalePlan(ctx, printer, plan, plan.Sources[0], stale, &applySourceReport{}); !stop {
		t.Fatal("expected the refusal to stop the run")
	}
	if strings.Contains(resumeGit(t, host, "worktree", "list", "--porcelain"), "worktree "+wt) {
		t.Fatal("the conflict worktree survived the plan supersede")
	}
}

func TestConfirmAppliedVerifiesTheRecordedTuple(t *testing.T) {
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	const (
		projectID = "default"
		sandboxID = "01JCONFIRM00000000000001"
		slug      = "primary"
		tip       = "0123456789abcdef0123456789abcdef01234567"
		hostTip   = "fedcba9876543210fedcba9876543210fedcba98"
	)

	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/complete-apply") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"projectId":%q,"createdByUserId":"u","displayName":"alpha","createdAt":"2026-10-15T00:00:00Z","updatedAt":"2026-10-15T00:00:00Z","config":{"name":"alpha","image":"img"},"runtime":{"desiredState":"present","state":"ready","generation":1,"observedGeneration":1,"appliedCommits":[{"slug":%q,"commit":%q,"hostCommit":%q,"hostId":"h","hostPath":"/repo","appliedAt":"2026-10-15T00:00:00Z"}]}}`,
			sandboxID, projectID, slug, tip, hostTip)
	}))
	defer server.Close()
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	sandbox := &apimodel.Sandbox{}
	report := &applySourceReport{}
	updated, ok := (&App{}).confirmApplied(ctx, printer, client, sandbox, applyPlanIdentity{Server: server.URL, ProjectID: projectID, SandboxID: sandboxID}, "h", "/repo", slug, tip, hostTip, report)
	if !ok {
		t.Fatalf("confirmApplied did not verify the tuple: %s", report.Error)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("complete-apply posts = %d, want 1", posts)
	}
	if !appliedCommitRecorded(updated, slug, tip, hostTip) {
		t.Fatal("read-back did not contain the recorded tuple")
	}
}

func TestConfirmAppliedFailsWhenResponseLacksTuple(t *testing.T) {
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"x","projectId":"default","createdByUserId":"u","displayName":"alpha","createdAt":"2026-10-15T00:00:00Z","updatedAt":"2026-10-15T00:00:00Z","config":{"name":"alpha","image":"img"},"runtime":{"desiredState":"present","state":"ready","generation":1,"observedGeneration":1}}`))
	}))
	defer server.Close()
	client, _ := apiclientgen.NewClient(server.URL)

	_, ok := (&App{}).confirmApplied(ctx, printer, client, &apimodel.Sandbox{}, applyPlanIdentity{Server: server.URL, ProjectID: "default", SandboxID: "x"}, "h", "/repo",
		"primary", "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98", &applySourceReport{})
	if ok {
		t.Fatal("a response without the tuple must not count as confirmed")
	}
}

func TestConfirmAppliedSurfacesPostFailure(t *testing.T) {
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	client, _ := apiclientgen.NewClient(server.URL)

	report := &applySourceReport{}
	if _, ok := (&App{}).confirmApplied(ctx, printer, client, &apimodel.Sandbox{}, applyPlanIdentity{Server: server.URL, ProjectID: "default", SandboxID: "x"}, "h", "/repo",
		"primary", "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98", report); ok {
		t.Fatal("a failing POST must not count as confirmed")
	}
	if !strings.Contains(report.Error, "recording the apply") || len(report.NextSteps) != 1 {
		t.Fatalf("failure report = %+v, want the landed-safely wording and a re-run step", report)
	}
}

func TestApplyResumeCommandRebuildsThePlanFingerprint(t *testing.T) {
	id := "01JRESUME00000000000001"
	cases := []struct {
		name     string
		identity applyPlanIdentity
		slug     string
		dir      string
		extras   []string
		want     string
	}{
		{
			name:     "whole run, no options",
			identity: applyPlanIdentity{SandboxID: id},
			want:     "discobox apply " + id,
		},
		{
			name:     "single source keeps its narrowing",
			identity: applyPlanIdentity{SandboxID: id, applyPlanSelection: applyPlanSelection{OnlySlug: "lib"}},
			want:     "discobox apply " + id + " --source lib",
		},
		{
			name:     "all --dir overrides are carried in slug order",
			identity: applyPlanIdentity{SandboxID: id, applyPlanSelection: applyPlanSelection{DirOverrides: map[string]string{"lib": "/x/lib", "alpha": "/x/a"}}},
			want:     "discobox apply " + id + " --dir alpha=/x/a --dir lib=/x/lib",
		},
		{
			name:     "allow-dirty is carried",
			identity: applyPlanIdentity{SandboxID: id, applyPlanSelection: applyPlanSelection{AllowDirty: true}},
			want:     "discobox apply " + id + " --allow-dirty",
		},
		{
			name:     "a re-pointed --dir replaces only that slug",
			identity: applyPlanIdentity{SandboxID: id, applyPlanSelection: applyPlanSelection{DirOverrides: map[string]string{"lib": "/old", "alpha": "/a"}}},
			slug:     "lib", dir: applyDirPlaceholder,
			want: "discobox apply " + id + " --dir alpha=/a --dir lib=PATH",
		},
		{
			name:     "the dirty remedy appends the flag once",
			identity: applyPlanIdentity{SandboxID: id},
			extras:   []string{"--allow-dirty"},
			want:     "discobox apply " + id + " --allow-dirty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyResumeCommand(tc.identity, tc.slug, tc.dir, tc.extras...)
			if got != tc.want {
				t.Fatalf("applyResumeCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPendingSourceReportShape(t *testing.T) {
	ps := &preparedSource{entry: applySourceEntry{slug: "lib"}, placed: true, repoRoot: "/home/dev/lib", dirOrigin: hostDirFromSandboxOrigin}
	r := pendingSourceReport(ps)
	if r.Status != applyStatusPending || r.Slug != "lib" || r.HostPath != "/home/dev/lib" {
		t.Fatalf("pending report = %+v", r)
	}
}

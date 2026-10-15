package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// applyLandedFixture builds a real repository whose HEAD is a recorded,
// unconfirmed landing, plus the plan/source state for it.
func applyLandedFixture(t *testing.T) (repoRoot, sandboxID, tip string, plan *applyPlan, ps *preparedSource) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v %s", args, err, out)
		}
	}
	output := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %s: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--initial-branch=main", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(dir+"/README.md", []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-qm", "base")
	head := output("rev-parse", "HEAD")

	sandboxID = "01JLANDED00000000000001"
	tip = "0123456789abcdef0123456789abcdef01234567"
	plan = &applyPlan{
		SchemaVersion: applyPlanSchemaVersion,
		applyPlanIdentity: applyPlanIdentity{
			ProjectID: "default", SandboxID: sandboxID,
		},
		Sources: []*applyPlanSource{{
			Slug: "primary", Phase: phaseLanded, Strategy: strategyCherryPick,
			RepoRoot: dir, HeadAtPlan: head, HostBase: head, HostTip: head,
			SandboxTip: tip,
			Commits:    []applyPlanCommit{{SHA: tip, Subject: "base"}},
		}},
	}
	ps = &preparedSource{
		entry:    applySourceEntry{slug: "primary"},
		placed:   true,
		kind:     sourceKindPlaced,
		repoRoot: dir,
		hostDir:  dir,
		branch:   "main",
		head:     head,
	}
	return dir, sandboxID, tip, plan, ps
}

// TestLandedResumeConfirmsAndReportsAppliedWithoutRefetching covers AC-4 at
// the orchestration level: a source recorded landed-but-unconfirmed is
// verified on the branch, confirmed once, and reported applied. It never
// reaches git fetch, so no git server is needed.
func TestLandedResumeConfirmsAndReportsAppliedWithoutRefetching(t *testing.T) {
	withTempStateDir(t)
	ctx := context.Background()
	printer := newApplyPrinter(io.Discard, false)
	_, sandboxID, tip, plan, ps := applyLandedFixture(t)
	head := plan.Sources[0].HostTip
	const slug = "primary"

	var posts, other int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete-apply") {
			atomic.AddInt32(&posts, 1)
		} else {
			// Any other request (a fetch attempt) fails the recovery promise.
			atomic.AddInt32(&other, 1)
			t.Errorf("unexpected request during landed recovery: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"projectId":"default","createdByUserId":"u","displayName":"alpha","createdAt":"2026-10-15T00:00:00Z","updatedAt":"2026-10-15T00:00:00Z","config":{"name":"alpha","image":"img"},"runtime":{"desiredState":"present","state":"ready","generation":1,"observedGeneration":1,"appliedCommits":[{"slug":%q,"commit":%q,"hostCommit":%q,"hostId":"h","hostPath":"/repo","appliedAt":"2026-10-15T00:00:00Z"}]}}`,
			sandboxID, slug, tip, head)
	}))
	defer server.Close()
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	plan.Server = server.URL

	sandbox := &apimodel.Sandbox{}
	report, stop := (&App{}).applyPlannedSource(ctx, printer, client, sandbox, "default", sandboxID, "h", "",
		ps, plan, true, false, nil)
	if stop {
		t.Fatal("a confirmed landing must not stop the run")
	}
	if report.Status != applyStatusApplied {
		t.Fatalf("status = %s, want applied (AC-4)", report.Status)
	}
	if atomic.LoadInt32(&posts) != 1 || atomic.LoadInt32(&other) != 0 {
		t.Fatalf("complete-apply posts=%d other requests=%d, want 1/0", posts, other)
	}
	if plan.Sources[0].Phase != phaseConfirmed {
		t.Fatalf("phase = %s, want confirmed", plan.Sources[0].Phase)
	}
}

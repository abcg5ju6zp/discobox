package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempStateDir points the CLI state directory at a fresh temp dir for the
// test, on every platform: discoboxStateDir reads XDG_STATE_HOME first.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "discobox", "cli")
}

func samplePlan() *applyPlan {
	return &applyPlan{
		SchemaVersion: applyPlanSchemaVersion,
		applyPlanIdentity: applyPlanIdentity{
			Server:    "http://127.0.0.1:18080",
			ProjectID: "default",
			SandboxID: "01JAPLANSANDBOX0000000001",
			applyPlanSelection: applyPlanSelection{
				OnlySlug:     "",
				DirOverrides: map[string]string{"lib": "/home/dev/lib"},
				AllowDirty:   true,
			},
		},
		CreatedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second),
		Sources: []*applyPlanSource{
			{
				Slug:       "primary",
				Phase:      phaseFetched,
				Strategy:   strategyCherryPick,
				RepoRoot:   "/home/dev/repo",
				HeadAtPlan: "1111111111111111111111111111111111111111",
				Base:       "2222222222222222222222222222222222222222",
				SandboxTip: "3333333333333333333333333333333333333333",
				Commits:    []applyPlanCommit{{SHA: "3333333333333333333333333333333333333333", Subject: "work"}},
			},
		},
	}
}

func TestApplyPlanSaveLoadRoundTrip(t *testing.T) {
	withTempStateDir(t)
	plan := samplePlan()
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadApplyPlan(plan.SandboxID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Server != plan.Server || got.ProjectID != plan.ProjectID || got.SandboxID != plan.SandboxID {
		t.Fatalf("identity mismatch: %+v", got.applyPlanIdentity)
	}
	if !got.equal(plan.applyPlanSelection) {
		t.Fatalf("selection mismatch: %+v vs %+v", got.applyPlanSelection, plan.applyPlanSelection)
	}
	if len(got.Sources) != 1 || got.Sources[0].Slug != "primary" || got.Sources[0].Phase != phaseFetched {
		t.Fatalf("source mismatch: %+v", got.Sources)
	}
	if got.Sources[0].Commits[0].Subject != "work" {
		t.Fatalf("commit detail lost: %+v", got.Sources[0].Commits)
	}
	if !got.UpdatedAt.Before(time.Now().Add(time.Second)) || got.UpdatedAt.IsZero() {
		t.Fatalf("updatedAt not stamped: %s", got.UpdatedAt)
	}
}

func TestApplyPlanFileIsPrivate(t *testing.T) {
	withTempStateDir(t)
	plan := samplePlan()
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(applyPlanFilePath(plan.SandboxID))
	if err != nil {
		t.Fatalf("stat plan: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("plan perms = %o, want 0600", mode)
	}
}

func TestLoadApplyPlanMissingVersionCorrupt(t *testing.T) {
	stateDir := withTempStateDir(t)

	if _, err := loadApplyPlan("01JNOSUCHPLAN0000000000000"); !errors.Is(err, errNoApplyPlan) {
		t.Fatalf("missing plan: got %v, want errNoApplyPlan", err)
	}

	plan := samplePlan()
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	good := applyPlanFilePath(plan.SandboxID)

	// Unknown schema version: a different CLI wrote this.
	if err := os.WriteFile(good, []byte(`{"schemaVersion":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApplyPlan(plan.SandboxID); !errors.Is(err, errApplyPlanVersion) {
		t.Fatalf("version mismatch: got %v, want errApplyPlanVersion", err)
	}

	// A prefix a crash could never leave, but hand-editing can.
	if err := os.WriteFile(good, []byte(`{"schemaVersion":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApplyPlan(plan.SandboxID); err == nil || errors.Is(err, errApplyPlanVersion) {
		t.Fatalf("corrupt plan: got %v, want a parse error distinct from version error", err)
	}

	// The plan lives under the state dir, nowhere surprising.
	if filepath.Dir(good) != filepath.Join(stateDir, "apply", plan.SandboxID) {
		t.Fatalf("plan path = %s", good)
	}
}

func TestApplySelectionEqual(t *testing.T) {
	base := applyPlanSelection{OnlySlug: "lib", AllowDirty: true, DirOverrides: map[string]string{"a": "1", "b": "2"}}
	cases := []struct {
		name  string
		other applyPlanSelection
		want  bool
	}{
		{"identical different map order", applyPlanSelection{OnlySlug: "lib", AllowDirty: true, DirOverrides: map[string]string{"b": "2", "a": "1"}}, true},
		{"nil and empty maps", applyPlanSelection{OnlySlug: "lib", AllowDirty: true}, true},
		{"different slug", applyPlanSelection{OnlySlug: "primary", AllowDirty: true, DirOverrides: map[string]string{"a": "1", "b": "2"}}, false},
		{"different dirty", applyPlanSelection{OnlySlug: "lib", AllowDirty: false, DirOverrides: map[string]string{"a": "1", "b": "2"}}, false},
		{"different override value", applyPlanSelection{OnlySlug: "lib", AllowDirty: true, DirOverrides: map[string]string{"a": "1", "b": "3"}}, false},
		{"missing override", applyPlanSelection{OnlySlug: "lib", AllowDirty: true, DirOverrides: map[string]string{"a": "1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Normalize base to nil-empty so nil-vs-empty is not tested here.
			got := base.equal(tc.other)
			if tc.name == "nil and empty maps" {
				empty := applyPlanSelection{OnlySlug: "lib", AllowDirty: true, DirOverrides: map[string]string{}}
				got = empty.equal(tc.other)
			}
			if got != tc.want {
				t.Fatalf("equal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyLockSerializesAndReleases(t *testing.T) {
	withTempStateDir(t)
	const sandboxID = "01JLOCKSANDBOX000000000001"

	first, err := acquireApplyPlanLock(sandboxID)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := acquireApplyPlanLock(sandboxID); err == nil {
		t.Fatal("second lock while first held: got nil, want in-progress error")
	}
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := acquireApplyPlanLock(sandboxID)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

func TestPlanCompleteAndRemove(t *testing.T) {
	withTempStateDir(t)
	plan := samplePlan()
	if err := plan.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if plan.complete() {
		t.Fatal("plan with a fetched source is not complete")
	}
	plan.Sources[0].Phase = phaseConfirmed
	if !plan.complete() {
		t.Fatal("confirmed-only plan is complete")
	}
	empty := &applyPlan{SchemaVersion: applyPlanSchemaVersion}
	if empty.complete() {
		t.Fatal("a plan with no sources is never complete")
	}
	if err := plan.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := loadApplyPlan(plan.SandboxID); !errors.Is(err, errNoApplyPlan) {
		t.Fatalf("after remove: got %v, want errNoApplyPlan", err)
	}
	// Removing a plan that is already gone is fine: the end state is right.
	if err := plan.remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestPlanIdentityMatch(t *testing.T) {
	plan := samplePlan()
	same := plan.applyPlanIdentity
	if !plan.matchesIdentity(same) {
		t.Fatal("identical identity should match")
	}
	other := same
	other.Server = "http://127.0.0.1:9999"
	if plan.matchesIdentity(other) {
		t.Fatal("different server must not match")
	}
	other = same
	other.OnlySlug = "primary"
	if plan.matchesIdentity(other) {
		t.Fatal("different --source selection must not match")
	}
	other = same
	other.AllowDirty = false
	if plan.matchesIdentity(other) {
		t.Fatal("different --allow-dirty selection must not match")
	}
}

func TestSourceStaleness(t *testing.T) {
	const (
		oldHEAD = "1111111111111111111111111111111111111111"
		newHEAD = "4444444444444444444444444444444444444444"
	)
	source := func(phase applyPhase) *applyPlanSource {
		return &applyPlanSource{
			Slug:       "primary",
			Phase:      phase,
			RepoRoot:   "/home/dev/repo",
			HeadAtPlan: oldHEAD,
		}
	}

	cases := []struct {
		name   string
		phase  applyPhase
		state  applyRepoState
		want   applyStaleKind
		remedy applyStaleRemedy
	}{
		{"fresh still", phaseFetched, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: oldHEAD, Dirty: false}, applyStaleNone, ""},
		{"target gone", phaseFetched, applyRepoState{Exists: false}, applyStaleTargetReplaced, applyStaleRemedyDir},
		{"target another repository", phaseConflict, applyRepoState{Exists: true, SameRepository: false}, applyStaleTargetReplaced, applyStaleRemedyDir},
		{"dirty host tree", phaseFetched, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: oldHEAD, Dirty: true}, applyStaleDirtyWorktree, applyStaleRemedyAllowDirty},
		{"branch advanced", phaseFetched, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: newHEAD}, applyStaleBranchAdvanced, applyStaleRemedyRerun},
		{"target replaced wins over the rest", phaseFetched, applyRepoState{Exists: false, SameRepository: false, CurrentHEAD: newHEAD, Dirty: true}, applyStaleTargetReplaced, applyStaleRemedyDir},
		{"dirty reported before branch advance", phaseFetched, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: newHEAD, Dirty: true}, applyStaleDirtyWorktree, applyStaleRemedyAllowDirty},
		{"landed never stale even with branch on top", phaseLanded, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: newHEAD, Dirty: true}, applyStaleNone, ""},
		{"confirmed never stale", phaseConfirmed, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: newHEAD}, applyStaleNone, ""},
		{"up-to-date never stale", phaseUpToDate, applyRepoState{Exists: true, SameRepository: true, CurrentHEAD: newHEAD}, applyStaleNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := source(tc.phase).staleness(tc.state)
			if got.Kind != tc.want || got.Remedy != tc.remedy {
				t.Fatalf("staleness = (%q, %q), want (%q, %q); detail %q",
					got.Kind, got.Remedy, tc.want, tc.remedy, got.Detail)
			}
			if tc.want != applyStaleNone && got.Detail == "" {
				t.Fatal("stale verdict carries no human-readable detail")
			}
		})
	}
}

func TestApplyPlanLayout(t *testing.T) {
	withTempStateDir(t)
	plan := samplePlan()
	dir := applyPlanDir(plan.SandboxID)
	if got, want := applyPlanFilePath(plan.SandboxID), filepath.Join(dir, "plan.json"); got != want {
		t.Fatalf("plan path = %s, want %s", got, want)
	}
	if got, want := applyWorktreeDir(plan.SandboxID, "primary"), filepath.Join(dir, "worktrees", "primary"); got != want {
		t.Fatalf("worktree path = %s, want %s", got, want)
	}
	if got := applyScratchBranch(plan.SandboxID, "primary"); got != "discobox-apply/"+plan.SandboxID+"/primary" {
		t.Fatalf("scratch branch = %s", got)
	}
}

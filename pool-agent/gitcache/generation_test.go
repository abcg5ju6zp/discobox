package gitcache

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sourcecache"
)

const testGeneration = "1111111111111111111111111111111111111111111111111111111111111111"

// snapshotCommit records a new commit with the current tree on top of HEAD,
// the shape a dirty-workspace snapshot takes, and points the snapshot ref at
// it; it returns the base commit and the snapshot commit.
func (w *work) snapshotCommit() (base, snapshot string) {
	base = w.git("rev-parse", "HEAD")
	tree := w.git("rev-parse", "HEAD^{tree}")
	snapshot = strings.TrimSpace(w.git("commit-tree", tree, "-p", base, "-m", "workspace snapshot"))
	w.git("update-ref", "refs/discobox/run/snap-1", snapshot)
	return base, snapshot
}

// pushGeneration delivers as a project member carrying a frozen generation:
// the member hook configuration with the generation env.
func (w *work) pushGeneration(s *Store, path, sandboxID, slug, generation string, refspecs ...string) (string, error) {
	if _, err := s.Receiving(context.Background(), path, sandboxID, slug); err != nil {
		w.t.Fatal(err)
	}
	config, err := s.GenerationReceiveConfig()
	if err != nil {
		w.t.Fatal(err)
	}
	config = append(ReceiveConfig(), config...)
	env := []string{
		"GIT_NAMESPACE=" + Namespace(sandboxID, slug),
		GenerationEnv + "=" + generation,
	}
	receive := "git " + strings.Join(config, " ") + " receive-pack"
	out, err := w.try(env, append([]string{"push", "--receive-pack=" + receive, path}, refspecs...)...)
	if err == nil {
		if err := s.Delivered(context.Background(), path, sandboxID, slug, true, ""); err != nil {
			w.t.Fatal(err)
		}
	}
	return out, err
}

func TestGenerationPinFreezesTheNamespace(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	w.commit("a")
	base, snapshot := w.snapshotCommit()
	refs := map[string]string{
		"refs/heads/main": base,
		sourcecache.RunSnapshotRefPrefix + "snap-1": snapshot,
	}

	// The first pin records the generation and the complete ref set, with
	// nothing to pin yet.
	result, err := s.Pin(ctx, path, "sbx-gen", "primary", testGeneration, refs, "")
	if err != nil {
		t.Fatalf("first generation pin: %v", err)
	}
	if len(result.Pinned) != 0 || len(result.Missing) != 2 {
		t.Fatalf("first pin = %+v, want both refs missing", result)
	}
	record, err := readGenerationRecord(path, Namespace("sbx-gen", "primary"))
	if err != nil || record == nil || record.Generation != testGeneration || record.Checkout != base {
		t.Fatalf("generation record = %+v, %v", record, err)
	}

	// The same pin is idempotent, the resume path after an interrupted push.
	if _, err := s.Pin(ctx, path, "sbx-gen", "primary", testGeneration, refs, ""); err != nil {
		t.Fatalf("same-generation pin refused: %v", err)
	}
	// Another generation, another ref set, or no generation at all is refused.
	if _, err := s.Pin(ctx, path, "sbx-gen", "primary", "2222222222222222222222222222222222222222222222222222222222222222", refs, ""); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("different generation = %v, want ErrGenerationMismatch", err)
	}
	other := map[string]string{"refs/heads/main": base}
	if _, err := s.Pin(ctx, path, "sbx-gen", "primary", testGeneration, other, ""); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("different ref set = %v, want ErrGenerationMismatch", err)
	}
	if _, err := s.Pin(ctx, path, "sbx-gen", "primary", "", refs, ""); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("generation pin without the generation = %v, want ErrGenerationMismatch", err)
	}

	// The delivery push lands the frozen refs at their recorded objects.
	if out, err := w.pushGeneration(s, path, "sbx-gen", "primary", testGeneration,
		base+":refs/heads/main",
		snapshot+":"+sourcecache.RunSnapshotRefPrefix+"snap-1"); err != nil {
		t.Fatalf("generation push: %v\n%s", err, out)
	}

	// The frozen namespace now verifies.
	verdict, err := s.VerifyGeneration(ctx, path, "sbx-gen", "primary", testGeneration)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verdict.OK() {
		t.Fatalf("verdict = %+v, want a pass", verdict)
	}
}

func TestGenerationPushGateRefusesWhatTheGenerationDoesNotName(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	w.commit("a")
	base, snapshot := w.snapshotCommit()
	refs := map[string]string{
		"refs/heads/main": base,
		sourcecache.RunSnapshotRefPrefix + "snap-1": snapshot,
	}
	if _, err := s.Pin(ctx, path, "sbx-gate", "primary", testGeneration, refs, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := w.pushGeneration(s, path, "sbx-gate", "primary", testGeneration,
		base+":refs/heads/main",
		snapshot+":"+sourcecache.RunSnapshotRefPrefix+"snap-1"); err != nil {
		t.Fatalf("generation push: %v\n%s", err, out)
	}

	// A newer local commit must not move the frozen branch.
	newer := w.commit("b")
	if out, err := w.pushGeneration(s, path, "sbx-gate", "primary", testGeneration,
		newer+":refs/heads/main"); err == nil {
		t.Fatalf("a push moving a frozen branch was accepted\n%s", out)
	}

	// A ref the generation never named is refused too.
	if out, err := w.pushGeneration(s, path, "sbx-gate", "primary", testGeneration,
		base+":refs/heads/other"); err == nil {
		t.Fatalf("a push creating an unrecorded ref was accepted\n%s", out)
	}

	// A push without the generation keeps the ordinary push rules, so a later
	// discobox push can move the branch after delivery.
	if out, err := w.push(s, path, "sbx-gate", "primary", "", newer+":refs/heads/main"); err != nil {
		t.Fatalf("a later branch move without a generation was refused: %v\n%s", err, out)
	}

	// A generation push before any pin is refused at the HTTP gate.
	if err := s.CheckGenerationPush(path, "sbx-ungated", "primary", testGeneration); !errors.Is(err, ErrNoGenerationRecord) {
		t.Fatalf("gate = %v, want ErrNoGenerationRecord", err)
	}
	if err := s.CheckGenerationPush(path, "sbx-gate", "primary", "2222222222222222222222222222222222222222222222222222222222222222"); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("gate = %v, want ErrGenerationMismatch", err)
	}
}

func TestGenerationVerifyNamesmesTheDeficit(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	w.commit("a")
	base, snapshot := w.snapshotCommit()
	refs := map[string]string{
		"refs/heads/main": base,
		sourcecache.RunSnapshotRefPrefix + "snap-1": snapshot,
	}
	if _, err := s.Pin(ctx, path, "sbx-bad", "primary", testGeneration, refs, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := w.pushGeneration(s, path, "sbx-bad", "primary", testGeneration,
		base+":refs/heads/main",
		snapshot+":"+sourcecache.RunSnapshotRefPrefix+"snap-1"); err != nil {
		t.Fatalf("generation push: %v\n%s", err, out)
	}
	second := w.commit("b")

	// A recorded ref pointed at an object that is not there: missing ref and
	// missing object.
	record, err := readGenerationRecord(path, Namespace("sbx-bad", "primary"))
	if err != nil {
		t.Fatal(err)
	}
	record.Refs["refs/heads/main"] = second
	if err := writeGenerationRecord(path, Namespace("sbx-bad", "primary"), record); err != nil {
		t.Fatal(err)
	}
	verdict, err := s.VerifyGeneration(ctx, path, "sbx-bad", "primary", testGeneration)
	if err != nil {
		t.Fatal(err)
	}
	// The ref is what names the deficit: an object only the moved ref reached
	// cannot be enumerated, but the ref no longer resolves to what it froze.
	if !slices.Contains(verdict.MissingRefs, "refs/heads/main") {
		t.Fatalf("verdict = %+v, want the moved branch ref named", verdict)
	}

	// A snapshot whose parent is not the checkout is a bad snapshot.
	record.Refs["refs/heads/main"] = base
	record.Checkout = second
	if err := writeGenerationRecord(path, Namespace("sbx-bad", "primary"), record); err != nil {
		t.Fatal(err)
	}
	verdict, err = s.VerifyGeneration(ctx, path, "sbx-bad", "primary", testGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(verdict.BadSnapshots, sourcecache.RunSnapshotRefPrefix+"snap-1") {
		t.Fatalf("verdict = %+v, want the snapshot named", verdict)
	}
}

func TestPreGenerationNamespaceKeepsTheOldPath(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/legacy")
	w := newWork(t)
	base := w.commit("a")
	refs := map[string]string{"refs/heads/main": base}

	// No generation on the pin: no record, no fence.
	if _, err := s.Pin(ctx, path, "sbx-old", "primary", "", refs, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.NamespaceReady(ctx, path, "sbx-old", "primary"); !errors.Is(err, ErrNoGenerationRecord) {
		t.Fatalf("namespace readiness = %v, want ErrNoGenerationRecord", err)
	}
	if err := s.CheckGenerationPush(path, "sbx-old", "primary", ""); err != nil {
		t.Fatalf("an ordinary push into an unfenced namespace was refused: %v", err)
	}
	if out, err := w.push(s, path, "sbx-old", "primary", "", base+":refs/heads/main"); err != nil {
		t.Fatalf("ordinary push: %v\n%s", err, out)
	}
}

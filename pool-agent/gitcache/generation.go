package gitcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/sourcecache"
)

// A namespace fenced to a frozen delivery generation (ADR 26-10-15-943).
var (
	// ErrGenerationMismatch is a request fenced to a generation the namespace
	// did not accept, or refs that are not the ones its generation recorded.
	ErrGenerationMismatch = errors.New("the namespace is fenced to a different delivery generation")
	// ErrNoGenerationRecord is a generation request against a namespace that
	// records none: a pre-ADR namespace, or a push a pin did not precede.
	ErrNoGenerationRecord = errors.New("the namespace records no delivery generation")
)

// generationFileSuffix follows the namespace name in discobox-namespaces for
// the record of its frozen generation.
const generationFileSuffix = ".generation"

// GenerationRecord is what a namespace's first generation pin froze: the
// generation, the pinned checkout commit, and every ref of the delivery with
// the object each points at. It is written before any of the refs is created,
// so a pool that dies between the record and the refs fences everything it
// survives with.
type GenerationRecord struct {
	Generation string            `json:"generation"`
	Checkout   string            `json:"checkout"`
	Refs       map[string]string `json:"refs"`
}

// VerifyVerdict names everything that keeps a namespace from being exactly its
// frozen generation. An empty verdict is a pass.
type VerifyVerdict struct {
	// MissingRefs are recorded refs that do not resolve to their recorded
	// objects.
	MissingRefs []string `json:"missingRefs,omitempty"`
	// BadSnapshots are dirty-workspace snapshot refs whose parent is not the
	// pinned checkout.
	BadSnapshots []string `json:"badSnapshots,omitempty"`
	// MissingObjects are objects the recorded refs reach that are not in the
	// cache.
	MissingObjects []string `json:"missingObjects,omitempty"`
}

// OK reports the verdict a pass.
func (v VerifyVerdict) OK() bool {
	return len(v.MissingRefs) == 0 && len(v.BadSnapshots) == 0 && len(v.MissingObjects) == 0
}

func generationRecordPath(path, namespace string) string {
	return filepath.Join(path, namespacesDir, namespace+generationFileSuffix)
}

// readGenerationRecord reads a namespace's generation record; no file is no
// record (a pre-ADR namespace).
func readGenerationRecord(path, namespace string) (*GenerationRecord, error) {
	data, err := os.ReadFile(generationRecordPath(path, namespace))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record GenerationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("read generation record for namespace %q: %w", namespace, err)
	}
	if record.Refs == nil {
		record.Refs = map[string]string{}
	}
	return &record, nil
}

// writeGenerationRecord persists a namespace's generation record atomically,
// beside its ownership marker.
func writeGenerationRecord(path, namespace string, record *GenerationRecord) error {
	if err := os.MkdirAll(filepath.Join(path, namespacesDir), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	target := generationRecordPath(path, namespace)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// checkGenerationRequest validates the generation a pin or push carries
// against the namespace's record. No generation and no record is the
// pre-ADR path; every other mismatch is refused.
func checkGenerationRequest(record *GenerationRecord, generation string) error {
	if record == nil {
		if generation == "" {
			return nil
		}
		return fmt.Errorf("%w: a generation push must follow the pin that records it", ErrNoGenerationRecord)
	}
	if generation == "" {
		return fmt.Errorf("%w: the namespace already accepted generation %s", ErrGenerationMismatch, record.Generation)
	}
	if generation != record.Generation {
		return fmt.Errorf("%w: namespace has %s, request names %s", ErrGenerationMismatch, record.Generation, generation)
	}
	return nil
}

// ensureGenerationRecord freezes a pin's generation on its first call and
// requires every later pin to name the same generation and the same complete
// ref set. It returns the record in effect for the pin.
func ensureGenerationRecord(path, namespace, generation string, refs map[string]string, checkout string) (*GenerationRecord, error) {
	record, err := readGenerationRecord(path, namespace)
	if err != nil {
		return nil, err
	}
	if record != nil {
		if generation != record.Generation {
			return nil, fmt.Errorf("%w: namespace has %s, pin names %s", ErrGenerationMismatch, record.Generation, generation)
		}
		if !sameRefSet(record.Refs, refs) {
			return nil, fmt.Errorf("%w: the pin does not name the frozen generation's complete ref set", ErrGenerationMismatch)
		}
		return record, nil
	}
	if generation == "" {
		// A pre-ADR namespace, and a pre-ADR pin: nothing is frozen.
		return nil, nil
	}
	record = &GenerationRecord{Generation: generation, Checkout: checkout, Refs: cloneRefs(refs)}
	if err := writeGenerationRecord(path, namespace, record); err != nil {
		return nil, err
	}
	return record, nil
}

// pinnedCheckout is the object the delivery's single branch points at: the
// commit the sandbox checks out and every snapshot's parent must equal.
func pinnedCheckout(refs map[string]string) (string, error) {
	var object string
	for ref, tip := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		if object != "" {
			return "", fmt.Errorf("%w: a delivery generation names more than one branch", ErrInvalid)
		}
		object = tip
	}
	if object == "" {
		return "", fmt.Errorf("%w: a delivery generation must name the source's branch", ErrInvalid)
	}
	return object, nil
}

func sameRefSet(want, got map[string]string) bool {
	if len(want) != len(got) {
		return false
	}
	for ref, object := range want {
		if got[ref] != object {
			return false
		}
	}
	return true
}

func cloneRefs(refs map[string]string) map[string]string {
	out := make(map[string]string, len(refs))
	for ref, object := range refs {
		out[ref] = object
	}
	return out
}

// CheckGenerationPush is the HTTP-layer gate run before a receive-pack: a
// push carrying a generation may land only in the namespace its pin recorded,
// fenced to that generation. A push without one is a later discobox push and
// keeps the ordinary rules, into a fenced namespace or otherwise.
func (s *Store) CheckGenerationPush(path, sandboxID, slug, generation string) error {
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return nil
	}
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return fmt.Errorf("%w: %q", ErrInvalid, namespace)
	}
	record, err := readGenerationRecord(path, namespace)
	if err != nil {
		return err
	}
	return checkGenerationRequest(record, generation)
}

// enforceGenerationGate is the pre-receive hook's generation check: every ref
// a delivery push updates is a recorded ref landing at its recorded object. It
// runs before receive-pack applies anything, so a late or partial delivery
// cannot move the frozen namespace. Later discobox pushes carry no generation
// env and never reach this.
func enforceGenerationGate(gitDir string, commands []hookCommand) error {
	generation := strings.TrimSpace(os.Getenv(GenerationEnv))
	namespace := strings.TrimSpace(os.Getenv("GIT_NAMESPACE"))
	if !validNamespace(namespace) {
		return fmt.Errorf("refuse the generation push: no valid namespace")
	}
	record, err := readGenerationRecord(gitDir, namespace)
	if err != nil {
		return err
	}
	if record == nil {
		return errors.New("the delivery generation was never pinned into this namespace, so the push is refused")
	}
	if record.Generation != generation {
		return fmt.Errorf("%w: this push carries %s, the namespace accepted %s", ErrGenerationMismatch, generation, record.Generation)
	}
	for _, command := range commands {
		if zeroObjectID(command.newID) {
			return fmt.Errorf("the frozen delivery generation never deletes ref %s", command.ref)
		}
		recorded, ok := record.Refs[command.ref]
		if !ok {
			return fmt.Errorf("%w: ref %s is not part of the frozen delivery", ErrGenerationMismatch, command.ref)
		}
		if recorded != command.newID {
			return fmt.Errorf("%w: ref %s must land at %s, not %s", ErrGenerationMismatch, command.ref, recorded, command.newID)
		}
	}
	return nil
}

// NamespaceReady verifies a namespace against whatever generation it itself
// recorded: the runtime's landing gate, which knows the generation only
// through the namespace. No record is ErrNoGenerationRecord, and the caller
// falls back to the pre-generation "holds a branch" check.
func (s *Store) NamespaceReady(ctx context.Context, path, sandboxID, slug string) (*GenerationRecord, VerifyVerdict, error) {
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return nil, VerifyVerdict{}, fmt.Errorf("%w: %q", ErrInvalid, namespace)
	}
	record, err := readGenerationRecord(path, namespace)
	if err != nil {
		return nil, VerifyVerdict{}, err
	}
	if record == nil {
		return nil, VerifyVerdict{}, ErrNoGenerationRecord
	}
	verdict, err := s.verifyRecord(ctx, path, namespace, record)
	if err != nil {
		return nil, VerifyVerdict{}, err
	}
	return record, verdict, nil
}

// VerifyGeneration checks a fenced namespace against the generation it is asked
// to verify, then runs the same check as NamespaceReady: every recorded ref
// resolves to its recorded object, every dirty-workspace snapshot's parent is
// the pinned checkout, and every object the refs reach is present in the cache.
// It is a local connectivity check, never a rehash.
func (s *Store) VerifyGeneration(ctx context.Context, path, sandboxID, slug, generation string) (VerifyVerdict, error) {
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return VerifyVerdict{}, fmt.Errorf("%w: %q", ErrInvalid, namespace)
	}
	record, err := readGenerationRecord(path, namespace)
	if err != nil {
		return VerifyVerdict{}, err
	}
	if record == nil {
		return VerifyVerdict{}, ErrNoGenerationRecord
	}
	if generation == "" || generation != record.Generation {
		return VerifyVerdict{}, fmt.Errorf("%w: namespace has %s, verify names %s", ErrGenerationMismatch, record.Generation, generation)
	}
	return s.verifyRecord(ctx, path, namespace, record)
}

// verifyRecord is the connectivity verification one frozen record defines.
func (s *Store) verifyRecord(ctx context.Context, path, namespace string, record *GenerationRecord) (VerifyVerdict, error) {
	verdict := VerifyVerdict{}

	refs := make([]string, 0, len(record.Refs))
	for ref := range record.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	tips := make([]string, 0, len(refs))
	for _, ref := range refs {
		object := resolve(ctx, path, namespaceRef(namespace, ref))
		if object != record.Refs[ref] {
			verdict.MissingRefs = append(verdict.MissingRefs, ref)
			continue
		}
		tips = append(tips, object)
	}

	for _, ref := range refs {
		if !strings.HasPrefix(ref, sourcecache.RunSnapshotRefPrefix) {
			continue
		}
		if parent := resolve(ctx, path, namespaceRef(namespace, ref)+"^"); parent != record.Checkout {
			verdict.BadSnapshots = append(verdict.BadSnapshots, ref)
		}
	}

	if len(tips) > 0 {
		missing, err := missingObjects(ctx, path, tips)
		if err != nil {
			return VerifyVerdict{}, err
		}
		verdict.MissingObjects = missing
	}
	sort.Strings(verdict.MissingRefs)
	sort.Strings(verdict.BadSnapshots)
	sort.Strings(verdict.MissingObjects)
	return verdict, nil
}

// missingObjects lists the tips' reachable objects the cache does not hold:
// one rev-list walk over every tip, checked in a single cat-file batch.
func missingObjects(ctx context.Context, path string, tips []string) ([]string, error) {
	var stdin bytes.Buffer
	for _, tip := range tips {
		stdin.WriteString(tip)
		stdin.WriteByte('\n')
	}
	list, err := git(ctx, path, nil, nil, "rev-list", "--objects", "--stdin")
	if err != nil {
		return nil, err
	}
	var objects bytes.Buffer
	seen := map[string]struct{}{}
	for _, line := range strings.Split(list, "\n") {
		object, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if object == "" {
			continue
		}
		if _, ok := seen[object]; ok {
			continue
		}
		seen[object] = struct{}{}
		objects.WriteString(object)
		objects.WriteByte('\n')
	}
	if objects.Len() == 0 {
		return nil, nil
	}
	checked, err := git(ctx, path, objects.Bytes(), nil, "cat-file", "--batch-check=%(objectname) %(objectname)")
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(checked, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "missing" {
			missing = append(missing, fields[0])
		}
	}
	return missing, nil
}

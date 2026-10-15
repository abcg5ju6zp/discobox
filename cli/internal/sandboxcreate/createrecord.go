package sandboxcreate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// createRecordTTL is how long a pending create is replayable on the client.
// The server keeps its own record the same span; after it the create is new
// again on both sides.
const createRecordTTL = 7 * 24 * time.Hour

// createsDirName is the state-directory subdirectory holding one JSON record
// per pending create.
const createsDirName = "creates"

// createRecord is the exact request a create sent and the token that makes it
// idempotent, filed so a lost response or a restarted client reattaches to the
// one sandbox the first request made (ADR 26-10-15-943 §2).
type createRecord struct {
	ServerURL  string          `json:"serverUrl"`
	ProjectID  string          `json:"projectId"`
	Generation string          `json:"generation"`
	Token      string          `json:"token"`
	Body       json.RawMessage `json:"body"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// mintCreateToken is the opaque token the server keys its idempotency record
// by. It is the client's, needs no shape the server recognizes, and is never
// derived from the sources.
func mintCreateToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func createsDir() (string, error) {
	root := cliStateDir()
	if root == "" {
		return "", fmt.Errorf("no state directory for create records")
	}
	dir := filepath.Join(root, createsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// recordKey identifies one pending create by the server, project and frozen
// generation: a retry that resolves to the same sources reattaches, a
// different generation is a different sandbox.
func recordKey(serverURL, projectID, generation string) string {
	identity := strings.Join([]string{strings.TrimSpace(serverURL), strings.TrimSpace(projectID), strings.TrimSpace(generation)}, "|")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]) + ".json"
}

// loadCreateRecord reads the replay record for one generation, deleting one
// that has outlived the TTL. Missing or disabled storage is no record.
func loadCreateRecord(serverURL, projectID, generation string) (*createRecord, error) {
	dir, err := createsDir()
	if err != nil {
		// No state directory means replay is disabled, not failed.
		return nil, nil //nolint:nilerr // replay is optional storage
	}
	path := filepath.Join(dir, recordKey(serverURL, projectID, generation))
	if fileIsMissing(path) {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record createRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if time.Since(record.CreatedAt) > createRecordTTL {
		_ = os.Remove(path)
		return nil, nil
	}
	return &record, nil
}

// saveCreateRecord writes one create's replay record atomically, private to
// this user.
func saveCreateRecord(record *createRecord) error {
	dir, err := createsDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, recordKey(record.ServerURL, record.ProjectID, record.Generation))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// discardCreateRecord removes a settled create's replay record. A missing
// record or disabled storage is not an error: cleanup is best effort.
func discardCreateRecord(serverURL, projectID, generation string) error {
	dir, err := createsDir()
	if err != nil {
		// Replay storage is unavailable, so discarding nothing succeeds.
		return nil //nolint:nilerr // nothing was recorded where there is no state directory
	}
	if err := os.Remove(filepath.Join(dir, recordKey(serverURL, projectID, generation))); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// sweepCreateRecords deletes records older than the TTL, opportunistically so
// the directory needs no background job.
func sweepCreateRecords() {
	dir, err := createsDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-createRecordTTL)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record createRecord
		if json.Unmarshal(data, &record) != nil || record.CreatedAt.Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

// fileIsMissing reports whether path does not exist. Other stat errors read
// as present so the following read surfaces them.
func fileIsMissing(path string) bool {
	_, err := os.Stat(path)
	return err != nil && os.IsNotExist(err)
}

// recordedCreateBody decodes the exact body a replay record stored.
func recordedCreateBody(raw json.RawMessage) (*apimodel.CreateSandboxBody, error) {
	var body apimodel.CreateSandboxBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return &body, nil
}

// replayTokenFiles returns the stored record files in name order, for tests
// and sweeps.
func replayTokenFiles() ([]string, error) {
	dir, err := createsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

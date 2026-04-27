package wiki

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CheckpointVersion is the on-disk schema version for checkpoint.json.
// Bumps on breaking changes; new optional fields don't bump it.
const CheckpointVersion = 1

// Checkpoint is the on-disk shape of <ContentDir>/checkpoint.json. The
// JSON layout is the contract — field names and types are versioned by
// CheckpointVersion. Pointers on the SHA fields so they marshal as null
// when the corresponding repo is absent or dirty.
type Checkpoint struct {
	Version          int     `json:"version"`
	LastIngestAt     string  `json:"last_ingest_at"`
	LastIngestRun    string  `json:"last_ingest_run"`
	BureaucracySHA   *string `json:"bureaucracy_sha"`
	Project          string  `json:"project"`
	ProjectRepoSHA   *string `json:"project_repo_sha"`
}

// CheckpointPath returns the absolute path to checkpoint.json given a
// ContentDir.
func CheckpointPath(contentDir string) string {
	return filepath.Join(contentDir, "checkpoint.json")
}

// LogPath returns the absolute path to log.md given a ContentDir.
func LogPath(contentDir string) string {
	return filepath.Join(contentDir, "log.md")
}

// IndexPath returns the absolute path to index.md given a ContentDir.
func IndexPath(contentDir string) string {
	return filepath.Join(contentDir, "index.md")
}

// WriteCheckpoint marshals cp as canonical pretty-printed JSON and
// writes it to <contentDir>/checkpoint.json, creating the directory
// if needed. Trailing newline so the file plays nicely with diff
// tools and editor-on-save behavior.
func WriteCheckpoint(contentDir string, cp Checkpoint) error {
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return fmt.Errorf("wiki: mkdir %s: %w", contentDir, err)
	}
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("wiki: marshal checkpoint: %w", err)
	}
	body = append(body, '\n')
	path := CheckpointPath(contentDir)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("wiki: write %s: %w", path, err)
	}
	return nil
}

// ReadCheckpoint reads <contentDir>/checkpoint.json. Returns
// (Checkpoint{}, false, nil) if the file doesn't exist — a fresh wiki
// has no checkpoint yet, and that's the normal first-ingest path. Any
// other I/O or parse error is returned as-is.
func ReadCheckpoint(contentDir string) (Checkpoint, bool, error) {
	path := CheckpointPath(contentDir)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Checkpoint{}, false, nil
		}
		return Checkpoint{}, false, fmt.Errorf("wiki: read %s: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return Checkpoint{}, false, fmt.Errorf("wiki: parse %s: %w", path, err)
	}
	return cp, true, nil
}

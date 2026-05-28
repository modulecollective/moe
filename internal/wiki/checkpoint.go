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
	Version        int     `json:"version"`
	LastIngestAt   string  `json:"last_ingest_at"`
	LastIngestRun  string  `json:"last_ingest_run"`
	BureaucracySHA *string `json:"bureaucracy_sha"`
	Project        string  `json:"project"`
	ProjectRepoSHA *string `json:"project_repo_sha"`
}

// topicsSubdir is the basename of the directory under ContentDir that
// holds topic docs. index.md, log.md, checkpoint.json and .wiki-ops
// stay at the top of ContentDir; per-topic *.md files live inside
// topicsSubdir so the corpus catalog (index.md) sits clean above the
// topic-doc dump.
const topicsSubdir = "topics"

// checkpointPath returns the absolute path to checkpoint.json given a
// ContentDir.
func checkpointPath(contentDir string) string {
	return filepath.Join(contentDir, "checkpoint.json")
}

// topicsDir returns the absolute path to the topics subdirectory given
// a ContentDir.
func topicsDir(contentDir string) string {
	return filepath.Join(contentDir, topicsSubdir)
}

// logPath returns the absolute path to log.md given a ContentDir.
func logPath(contentDir string) string {
	return filepath.Join(contentDir, "log.md")
}

// indexPath returns the absolute path to index.md given a ContentDir.
func indexPath(contentDir string) string {
	return filepath.Join(contentDir, "index.md")
}

// historySummaryName is the basename of the rolling-summary doc the
// agent maintains during reflect. Exempted from the closed-schema
// invariants alongside log.md — engine-aware but agent-written, so it's
// neither a managed doc with a ReflectPrompt nor a stray .md the
// schema-drift check should reject.
const historySummaryName = "history-summary.md"

// historySummaryPath returns the absolute path to history-summary.md
// given a ContentDir. The summary is reflect's rolling compressed memory
// of project history before the current checkpoint SHA — maintained by
// the agent at the end of each reflect pass, alongside the verbatim
// "events since last reflect" block.
func historySummaryPath(contentDir string) string {
	return filepath.Join(contentDir, historySummaryName)
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
	path := checkpointPath(contentDir)
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
	path := checkpointPath(contentDir)
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

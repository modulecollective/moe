// Package claude is a thin seam over the `claude` CLI's on-disk artifacts.
//
// The only artifact today is the per-session transcript Claude Code writes
// to <CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<session-id>.jsonl every
// turn. Each stage turn copies that file into the document's thread.jsonl
// so the conversation lives in the bureaucracy repo alongside the document
// it produced. See designs/conversation-saving.md.
package claude

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ConfigDir returns the effective Claude Code config root — $CLAUDE_CONFIG_DIR
// when set, else ~/.claude. Empty string when neither is available (no home
// dir; unusual but possible in hermetic environments).
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// TranscriptPath returns the filesystem path of Claude Code's session log
// for sessionID, or "" if no log has been written yet. The lookup globs
// <config>/projects/*/<sessionID>.jsonl because sessionID is a UUID — which
// dir it lands under depends on Claude Code's cwd-encoding scheme, and we
// shouldn't depend on reproducing that scheme.
func TranscriptPath(sessionID string) (string, error) {
	root := ConfigDir()
	if root == "" {
		return "", nil
	}
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("claude: glob transcripts: %w", err)
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// CopyTranscript copies Claude Code's session transcript for sessionID to
// dest, creating dest's parent dir if needed. Returns (found, err): found is
// false when no transcript exists yet — a legitimate state (operator aborted
// before claude wrote anything, or ran on a different machine) that callers
// should treat as a no-op rather than an error.
func CopyTranscript(sessionID, dest string) (bool, error) {
	src, err := TranscriptPath(sessionID)
	if err != nil {
		return false, err
	}
	if src == "" {
		return false, nil
	}
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("claude: open transcript: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, fmt.Errorf("claude: mkdir thread dir: %w", err)
	}
	out, err := os.Create(dest)
	if err != nil {
		return false, fmt.Errorf("claude: create thread.jsonl: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, fmt.Errorf("claude: copy transcript: %w", err)
	}
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("claude: close thread.jsonl: %w", err)
	}
	return true, nil
}

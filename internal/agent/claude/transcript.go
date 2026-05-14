// transcript.go is the thin seam over the `claude` CLI's on-disk
// session artifacts. Claude Code writes per-turn JSONL to
// <CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<session-id>.jsonl; each
// stage turn copies that file into the document's per-agent thread
// file so the conversation lives in the bureaucracy repo alongside
// the document it produced. See designs/conversation-saving.md.
package claude

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// EncodeCwd returns the directory name Claude Code uses to bucket
// per-session JSONLs under <ConfigDir>/projects. Claude encodes absCwd
// by replacing both path separators (`/`) and `.` with `-`; an absolute
// POSIX path's leading `/` becomes a leading `-`, and a `/.moe/` segment
// collapses to `--moe-` (double dash, no literal dot). The scheme has
// been stable across recent versions; if it ever drifts, callers
// `Stat`ing the returned path will see ErrNotExist and can fall back
// to a glob lookup.
func EncodeCwd(absCwd string) string {
	s := strings.ReplaceAll(absCwd, string(os.PathSeparator), "-")
	return strings.ReplaceAll(s, ".", "-")
}

// CanonicalTranscriptPath is the path Claude Code reads when you pass
// `--resume sessionID` from cwd: <ConfigDir>/projects/<EncodeCwd(cwd)>/<sessionID>.jsonl.
// Returned regardless of whether the file exists; callers stat it
// themselves to drive the migrate-or-re-mint decision. Returns "" when
// ConfigDir is unavailable (no $HOME and no $CLAUDE_CONFIG_DIR), which
// callers should treat the same as "transcript not found here."
func CanonicalTranscriptPath(cwd, sessionID string) string {
	root := ConfigDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "projects", EncodeCwd(cwd), sessionID+".jsonl")
}

// TranscriptPath returns the filesystem path of Claude Code's session log
// for sessionID, or "" if no log has been written yet. The lookup globs
// <config>/projects/*/<sessionID>.jsonl rather than reconstructing the
// encoded-cwd path: sessionID is a UUID so collisions across project
// dirs are impossible, and the glob keeps the per-turn save into
// thread.jsonl resilient to any drift in claude's cwd-encoding scheme.
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

// transcript.go is the thin seam over the `claude` CLI's on-disk
// session artifacts. Claude Code writes per-turn JSONL to
// <CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<session-id>.jsonl; each
// stage turn copies that file into the document's per-agent thread
// file so the conversation lives in the bureaucracy repo alongside
// the document it produced. See designs/conversation-saving.md.
package claude

import (
	"bufio"
	"encoding/json"
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
// thread-<agent>.jsonl resilient to any drift in claude's cwd-encoding scheme.
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
		return false, fmt.Errorf("claude: create thread file: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, fmt.Errorf("claude: copy transcript: %w", err)
	}
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("claude: close thread file: %w", err)
	}
	return true, nil
}

// RestoreFromCache stages a transcript copy from src into the canonical
// path `--resume sessionID` will read from cwd, rewriting each line's
// top-level "cwd" field to absCwd so claude's resume doesn't trip on the
// mismatch. Source file is left in place (the design's "copy, don't move"
// decision so `moe claude-cache gc` can reap the orphan separately).
// Returns the source directory's basename so the caller can surface
// "recovered from <bucket>" on stderr.
//
// Only top-level cwd is rewritten — tool-call paths nested inside message
// content are not touched. Doc-only stages (the only callers that hit this
// path; code stages already have a stable cwd) rarely embed absolute paths
// in past tool output, and stale absolute references in past assistant text
// don't block resume.
func RestoreFromCache(src, absCwd, sessionID string) (string, error) {
	dest := CanonicalTranscriptPath(absCwd, sessionID)
	if dest == "" {
		return "", fmt.Errorf("claude: no config dir for canonical path")
	}
	if src == dest {
		return filepath.Base(filepath.Dir(src)), nil
	}
	if err := copyTranscriptWithCwdRewrite(src, dest, absCwd); err != nil {
		return "", err
	}
	return filepath.Base(filepath.Dir(src)), nil
}

// RestoreFromMirror is the cross-machine / cache-wipe fallback: copy the
// bureaucracy-side mirror at mirrorPath into the canonical cache slot for
// (absCwd, sessionID), rewriting top-level cwd lines on the way. Returns
// (true, nil) on success, (false, nil) when mirrorPath doesn't exist —
// the legitimate "nothing to fall back to" state.
func RestoreFromMirror(mirrorPath, absCwd, sessionID string) (bool, error) {
	if mirrorPath == "" {
		return false, nil
	}
	if _, err := os.Stat(mirrorPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("claude: stat mirror: %w", err)
	}
	dest := CanonicalTranscriptPath(absCwd, sessionID)
	if dest == "" {
		return false, fmt.Errorf("claude: no config dir for canonical path")
	}
	if err := copyTranscriptWithCwdRewrite(mirrorPath, dest, absCwd); err != nil {
		return false, err
	}
	return true, nil
}

// copyTranscriptWithCwdRewrite streams src to dest line-by-line, rewriting
// each JSON record's top-level "cwd" field to absCwd. Non-JSON lines (rare;
// claude's writer always emits well-formed lines but defensive matters here)
// and lines without a top-level cwd pass through verbatim. The output writer
// uses bufio.Writer plus a final Flush so a half-written file never lands
// at dest — same partial-write tolerance the existing CopyTranscript path
// has via os.Create's truncate semantics.
func copyTranscriptWithCwdRewrite(src, dest, absCwd string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("claude: open transcript src: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("claude: mkdir canonical dir: %w", err)
	}
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("claude: create canonical file: %w", err)
	}
	w := bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		rewritten := rewriteTopLevelCwd(line, absCwd)
		if _, err := w.Write(rewritten); err != nil {
			out.Close()
			return fmt.Errorf("claude: write canonical: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			out.Close()
			return fmt.Errorf("claude: write canonical: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		out.Close()
		return fmt.Errorf("claude: scan transcript: %w", err)
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return fmt.Errorf("claude: flush canonical: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("claude: close canonical: %w", err)
	}
	return nil
}

// rewriteTopLevelCwd parses line as JSON, replaces a top-level "cwd"
// string value with absCwd, and returns the re-serialized bytes. Lines
// that don't parse as a JSON object, or that lack a top-level "cwd",
// pass through unchanged so the rewrite is structure-preserving.
//
// Uses encoding/json's map[string]any decode rather than a string-replace
// because cwd values may share prefixes with other paths in the same line
// (tool-call output references); a substring sweep would over-rewrite.
func rewriteTopLevelCwd(line []byte, absCwd string) []byte {
	trimmed := bytesTrimLeftWhitespace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return line
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line
	}
	raw, ok := obj["cwd"]
	if !ok {
		return line
	}
	// Only rewrite when cwd was a string; preserve odd shapes verbatim.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return line
	}
	if s == absCwd {
		return line
	}
	enc, err := json.Marshal(absCwd)
	if err != nil {
		return line
	}
	obj["cwd"] = enc
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	return out
}

// bytesTrimLeftWhitespace trims leading ASCII whitespace from b without
// allocating. Kept local so the rewrite path doesn't pull in bytes for
// one helper.
func bytesTrimLeftWhitespace(b []byte) []byte {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b[i:]
		}
	}
	return nil
}

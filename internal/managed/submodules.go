package managed

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExpandSubmodules reads <clonePath>/.gitmodules and returns one
// Submodule per entry, with SHA populated from `git -C <clonePath>
// ls-tree HEAD <path>`. A project with no submodules (no .gitmodules
// file, or an empty one) yields (nil, nil) — not an error.
//
// The token arg is applied uniformly to every returned Submodule. In
// the common case all submodules share a GitHub org and PAT; callers
// that need per-submodule auth can override entries afterwards.
func ExpandSubmodules(clonePath, token string) ([]Submodule, error) {
	entries, err := parseGitmodules(filepath.Join(clonePath, ".gitmodules"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]Submodule, 0, len(entries))
	for _, e := range entries {
		sha, err := pinnedSHA(clonePath, e.path)
		if err != nil {
			return nil, fmt.Errorf("managed: pinned sha for submodule %s: %w", e.path, err)
		}
		out = append(out, Submodule{
			Path:  e.path,
			URL:   e.url,
			SHA:   sha,
			Token: token,
		})
	}
	return out, nil
}

// gitmoduleEntry is one [submodule] section from a .gitmodules file.
// We only read the two fields we need (path, url); other keys like
// branch, shallow, fetchRecurseSubmodules are preserved on the remote
// without our involvement.
type gitmoduleEntry struct {
	name string
	path string
	url  string
}

// parseGitmodules is a minimal INI-style parser for .gitmodules. We
// avoid shelling out to `git config --file .gitmodules --list` so the
// code works before the submodules are initialized on disk (e.g. in
// tests with a hand-written .gitmodules and no real submodule checkouts).
func parseGitmodules(path string) ([]gitmoduleEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("managed: open %s: %w", path, err)
	}
	defer f.Close()

	var entries []gitmoduleEntry
	var cur *gitmoduleEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			// New [submodule "name"] section. Commit the previous
			// entry if it had the fields we need; skip it otherwise
			// so a malformed stanza doesn't poison later entries.
			if cur != nil && cur.path != "" && cur.url != "" {
				entries = append(entries, *cur)
			}
			cur = nil
			header := strings.TrimSpace(line[1 : len(line)-1])
			if strings.HasPrefix(header, "submodule ") {
				name := strings.Trim(strings.TrimPrefix(header, "submodule "), "\"")
				cur = &gitmoduleEntry{name: name}
			}
			continue
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "path":
			cur.path = val
		case "url":
			cur.url = val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("managed: scan %s: %w", path, err)
	}
	if cur != nil && cur.path != "" && cur.url != "" {
		entries = append(entries, *cur)
	}
	return entries, nil
}

// pinnedSHA returns the commit SHA the parent repo has recorded for
// the submodule at subPath. Implementation note: `git ls-tree HEAD
// <path>` prints a line like
//
//	160000 commit <sha>\t<path>
//
// for a submodule (mode 160000 is git's "gitlink"). We extract the SHA
// in field [2].
func pinnedSHA(clonePath, subPath string) (string, error) {
	cmd := exec.Command("git", "-C", clonePath, "ls-tree", "HEAD", subPath)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-tree HEAD %s: %w", subPath, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("git ls-tree HEAD %s returned no entry", subPath)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", fmt.Errorf("git ls-tree: unexpected output %q", line)
	}
	if fields[0] != "160000" {
		return "", fmt.Errorf("git ls-tree: %s is not a submodule (mode %s)", subPath, fields[0])
	}
	return fields[2], nil
}

// Package project registers target repos as submodules of the bureaucracy.
//
// Registration is a single atomic operation: detect the remote's default
// branch, add it as a submodule under projects/<id>/src/, write the
// project.json schema described in README §"Project (Target Repo)", and
// commit both on main. The command lives on main because the README
// treats project registration as settled state, not a run.
//
// The submodule nests one level deep (projects/<id>/src/) so that
// projects/<id>/ itself is a plain bureaucracy-tracked directory that
// can hold project.json and the runs/ tree alongside the submodule
// checkout. A submodule at projects/<id>/ directly would prevent git
// from tracking any sibling files under the same path — the whole
// directory would be a single gitlink entry.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Metadata is the on-disk shape of projects/<id>/project.json.
//
// The id doubles as the project's display name — there is no separate Name
// field. One name, derived from the URL, used everywhere.
type Metadata struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Submodule     string `json:"submodule"`
	Remote        string `json:"remote"`
	DefaultBranch string `json:"default_branch"`
	DeployURL     string `json:"deploy_url,omitempty"`
	Created       string `json:"created"`
}

// Options carries optional user-supplied fields for Register.
type Options struct {
	// Now is injected for deterministic tests. Defaults to time.Now.
	Now func() time.Time
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// SubmoduleDir returns the path (relative to the bureaucracy root) of
// the project's submodule checkout — projects/<id>/src/. One function so
// callers don't re-spell the "src" convention.
func SubmoduleDir(id string) string {
	return filepath.Join("projects", id, "src")
}

// Dir returns the path (relative to the bureaucracy root) of the
// project's state directory — projects/<id>/, which holds project.json,
// runs/, and the submodule subdirectory.
func Dir(id string) string {
	return filepath.Join("projects", id)
}

// Register adds the repo at url as a submodule of the bureaucracy at root and
// writes projects/<id>/project.json. Returns the resolved Metadata.
func Register(root, url string, opts Options) (*Metadata, error) {
	id, err := deriveID(url)
	if err != nil {
		return nil, err
	}
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("project: derived id %q from %q must match %s", id, url, idPattern)
	}

	submodulePath := SubmoduleDir(id)
	projectJSONPath := filepath.Join(Dir(id), "project.json")

	if _, err := os.Stat(filepath.Join(root, submodulePath)); err == nil {
		return nil, fmt.Errorf("project: %s already exists", submodulePath)
	}
	if _, err := os.Stat(filepath.Join(root, projectJSONPath)); err == nil {
		return nil, fmt.Errorf("project: %s already exists", projectJSONPath)
	}

	branch, err := detectDefaultBranch(url)
	if err != nil {
		return nil, err
	}

	if err := runGit(root, "submodule", "add", "-b", branch, url, submodulePath); err != nil {
		return nil, fmt.Errorf("project: git submodule add: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	md := &Metadata{
		ID:            id,
		Status:        "incubating",
		Submodule:     submodulePath,
		Remote:        normalizeRemote(url),
		DefaultBranch: branch,
		Created:       now().UTC().Format("2006-01-02"),
	}

	if err := writeJSON(filepath.Join(root, projectJSONPath), md); err != nil {
		return nil, err
	}

	if err := runGit(root, "add", ".gitmodules", submodulePath, projectJSONPath); err != nil {
		return nil, fmt.Errorf("project: git add: %w", err)
	}
	msg := fmt.Sprintf("Register project %s", id)
	if err := runGit(root, "commit", "-m", msg); err != nil {
		return nil, fmt.Errorf("project: git commit: %w", err)
	}

	return md, nil
}

// Load reads projects/<id>/project.json and returns the resolved Metadata.
// Used by commands that operate against a registered project (e.g. push)
// to resolve Remote, DefaultBranch, and Submodule without re-deriving.
func Load(root, id string) (*Metadata, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("project: id %q must match %s", id, idPattern)
	}
	path := filepath.Join(root, Dir(id), "project.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("project: read %s: %w", path, err)
	}
	md := &Metadata{}
	if err := json.Unmarshal(b, md); err != nil {
		return nil, fmt.Errorf("project: parse %s: %w", path, err)
	}
	if md.Remote == "" {
		return nil, fmt.Errorf("project: %s has no remote", path)
	}
	return md, nil
}

// deriveID extracts a project id from the last path component of a repo URL,
// stripping a trailing .git.
func deriveID(url string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	// Handle both scp-style (git@host:owner/repo) and URL-style remotes.
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	trimmed = strings.ToLower(trimmed)
	if trimmed == "" {
		return "", fmt.Errorf("project: cannot derive id from %q", url)
	}
	return trimmed, nil
}

// normalizeRemote stores the remote as given. A future improvement could
// canonicalize scp-style to https, but round-tripping those is lossy (needs
// credentials context) — store-as-typed is safer.
func normalizeRemote(url string) string { return url }

// detectDefaultBranch asks the remote which ref HEAD points at, so we don't
// have to guess "main" vs "master" vs something else.
func detectDefaultBranch(url string) (string, error) {
	out, err := exec.Command("git", "ls-remote", "--symref", url, "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("project: ls-remote %s: %w", url, err)
	}
	// First line for a normal repo: "ref: refs/heads/<branch>\tHEAD"
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := fields[1]
		const prefix = "refs/heads/"
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix), nil
		}
	}
	return "", fmt.Errorf("project: no symbolic HEAD in ls-remote output for %s", url)
}

// runGit invokes git with stdio wired to the user's terminal so credential
// helpers and SSH prompts can complete. Capturing stderr would hide those
// prompts and make the command appear to hang.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Unregister is the inverse of Register: remove the submodule, delete
// projects/<id>/, and commit. Refuses if projects/<id>/ holds a runs/
// tree — that signals active work the caller probably didn't mean to
// throw away.
func Unregister(root, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("project: id %q must match %s", id, idPattern)
	}
	submodulePath := SubmoduleDir(id)
	projectDir := Dir(id)
	projectJSONPath := filepath.Join(projectDir, "project.json")

	if _, err := os.Stat(filepath.Join(root, projectJSONPath)); err != nil {
		return fmt.Errorf("project: %s not registered (%s missing)", id, projectJSONPath)
	}

	// Refuse if any run dir exists under projects/<id>/runs/ — active
	// work should be cleaned up (or the run scrapped) before tearing the
	// project down.
	runsDir := filepath.Join(root, projectDir, "runs")
	if entries, err := os.ReadDir(runsDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("project: %s has %d run(s) — remove them manually first", filepath.Join(projectDir, "runs"), len(entries))
	}

	// `git rm` handles both .gitmodules bookkeeping and the working-tree
	// removal of the submodule in one shot. `submodule deinit` clears the
	// active-config entry so the submodule isn't left half-registered.
	if err := runGit(root, "submodule", "deinit", "-f", "--", submodulePath); err != nil {
		return fmt.Errorf("project: git submodule deinit: %w", err)
	}
	if err := runGit(root, "rm", "-f", submodulePath); err != nil {
		return fmt.Errorf("project: git rm submodule: %w", err)
	}
	// Leftover git metadata for the submodule; not tracked, so git won't
	// clean it for us.
	if err := os.RemoveAll(filepath.Join(root, ".git", "modules", "projects", id, "src")); err != nil {
		return fmt.Errorf("project: remove .git/modules/projects/%s/src: %w", id, err)
	}

	if err := runGit(root, "rm", "-f", projectJSONPath); err != nil {
		return fmt.Errorf("project: git rm project.json: %w", err)
	}
	// projects/<id>/ is now empty; git doesn't track directories, so delete it ourselves.
	if err := os.Remove(filepath.Join(root, projectDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("project: remove %s: %w", projectDir, err)
	}

	msg := fmt.Sprintf("Unregister project %s", id)
	if err := runGit(root, "commit", "-m", msg); err != nil {
		return fmt.Errorf("project: git commit: %w", err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

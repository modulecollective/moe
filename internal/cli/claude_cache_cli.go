package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/agent/claude"
	"github.com/modulecollective/moe/internal/git"
)

// `moe claude-cache gc` reaps the per-session JSONL buckets claude
// strands under `<CLAUDE_CONFIG_DIR>/projects/` when a bureaucracy
// session worktree is removed. Claude keys session storage by encoded
// cwd; MoE used a per-turn worktree-UUID path as cwd for document-only
// stages until the stable-cwd switch, and every pre-switch session
// JSONL still sits under the old encoded-cwd dir today. Once the
// owning worktree is gone, the bucket has no reachable referent and
// only takes up space.
//
// Sibling to `moe clone gc` and `moe session gc` rather than a
// generalised "moe gc" umbrella — the three primitives have different
// orphan rules and run-state interactions, and an aggregator earns its
// way in later if "tidy everything" becomes the operator's frequent
// move.
//
// Scope is intentionally narrow: only directories whose encoded segment
// matches the MoE worktree shape `*worktrees-<UUID>` are considered. A
// stray `~/.claude/projects/<some-other-cwd-encoding>` from non-MoE
// claude usage stays untouched. The UUID test against the live
// `git worktree list` is what proves "no reachable referent" — a
// matching live worktree means a current session is using this bucket
// and the dir must be left alone.

// claudeCacheUUIDPattern captures a worktree UUID embedded in an
// encoded project dir name. UUID format is 8-4-4-4-12 lowercase hex
// with `-` separators; the leading `worktrees-` anchor pins the
// match to MoE-shaped paths only. The (?:[-/]|$) tail ensures we
// match the *end* of the UUID and don't latch onto a longer suffix.
var claudeCacheUUIDPattern = regexp.MustCompile(
	`worktrees-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:[^0-9a-f]|$)`,
)

func init() {
	g := NewCommandGroup("claude-cache", "garbage-collect orphan claude session caches under "+claudeProjectsDescription())
	g.Register(&Command{
		Name:    "gc",
		Summary: "remove worktree-encoded claude project dirs whose worktree is gone",
		Run:     runClaudeCacheGC,
	})
	RegisterGroup(g)
}

// claudeProjectsDescription is the usage-string fragment that names the
// directory the verb operates on. Computed at registration time so the
// `--help` line shows the actual config dir rather than a generic
// "$CLAUDE_CONFIG_DIR/projects" placeholder.
func claudeProjectsDescription() string {
	if d := claude.ConfigDir(); d != "" {
		return filepath.Join(d, "projects")
	}
	return "$CLAUDE_CONFIG_DIR/projects"
}

func runClaudeCacheGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claude-cache gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe claude-cache gc")
		moePrintln(stderr, "")
		moePrintln(stderr, "Removes per-session JSONL buckets under "+claudeProjectsDescription())
		moePrintln(stderr, "whose encoded directory name embeds a `worktrees-<UUID>` segment from a")
		moePrintln(stderr, "MoE session worktree that `git worktree list` no longer reports. Non-MoE")
		moePrintln(stderr, "claude usage (encoded paths that don't carry a worktree UUID) is left alone.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	configDir := claude.ConfigDir()
	if configDir == "" {
		moePrintln(stderr, "claude-cache gc: no $CLAUDE_CONFIG_DIR and no $HOME — nothing to scan")
		return 1
	}
	projectsDir := filepath.Join(configDir, "projects")

	liveUUIDs, err := liveWorktreeUUIDs(root)
	if err != nil {
		moePrintf(stderr, "claude-cache gc: %v\n", err)
		return 1
	}

	orphans, err := findOrphanClaudeCacheDirs(projectsDir, liveUUIDs)
	if err != nil {
		moePrintf(stderr, "claude-cache gc: %v\n", err)
		return 1
	}
	if len(orphans) == 0 {
		moePrintln(stdout, "claude-cache gc: no orphan dirs")
		return 0
	}

	failed := 0
	for _, dir := range orphans {
		if err := os.RemoveAll(dir); err != nil {
			moePrintf(stderr, "claude-cache gc: %s: %v\n", filepath.Base(dir), err)
			failed++
			continue
		}
		moePrintf(stdout, "removed %s\n", filepath.Base(dir))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// liveWorktreeUUIDs returns the set of worktree UUIDs git currently
// reports for the bureaucracy repo. The UUID is parsed off the
// basename of each `worktree <path>` line whose path lives under the
// canonical `.moe/worktrees/` directory — anything else (the main
// worktree, ad-hoc worktrees outside .moe/worktrees) doesn't share an
// id namespace with claude's encoded-cwd dirs and can't false-positive
// the orphan check.
func liveWorktreeUUIDs(root string) (map[string]bool, error) {
	out, err := git.Output(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worktree list: %w", err)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		base := filepath.Base(p)
		if uuidRe.MatchString(base) {
			set[base] = true
		}
	}
	return set, nil
}

// findOrphanClaudeCacheDirs scans projectsDir for entries matching the
// `worktrees-<UUID>` pattern and returns the full paths of those whose
// UUID is not in liveUUIDs. Non-matching entries are skipped — leaving
// non-MoE claude usage untouched is the whole point of the narrow rule.
// Result is sorted so the verb's output is stable.
func findOrphanClaudeCacheDirs(projectsDir string, liveUUIDs map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", projectsDir, err)
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		match := claudeCacheUUIDPattern.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		uuid := match[1]
		if liveUUIDs[uuid] {
			continue
		}
		orphans = append(orphans, filepath.Join(projectsDir, e.Name()))
	}
	sort.Strings(orphans)
	return orphans, nil
}

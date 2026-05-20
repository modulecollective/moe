package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TwinDirRel is the wiki-relative path under
// `projects/<project>/` where a project's closed-schema digital twin
// lives. Hard-coded — every project's twin sits in the same place so
// stage prompts and tools can compute the path without consulting
// per-project config.
const TwinDirRel = "digital-twin"

// TwinDir returns the absolute path to the project's digital-twin
// directory under the bureaucracy root.
func TwinDir(root, projectID string) string {
	return filepath.Join(root, "projects", projectID, TwinDirRel)
}

// TwinReferenceSection emits a system-prompt block that names the
// project's digital twin and tells the agent to read it before doing
// substantive work. Reference, not splice — the agent decides what to
// read each turn rather than the engine inflating every prompt with
// the full twin contents.
//
// Returns "" when the project has no digital-twin/ dir on disk.
// Stages don't need to branch on this: empty input concatenates
// cleanly into buildSystemPrompt's section join.
func TwinReferenceSection(cfg Config) string {
	return TwinReferenceSectionAt(cfg.BureaucracyPath, cfg.Project)
}

// TwinReferenceSectionAt is the path-driven variant of
// TwinReferenceSection. Useful for callers that don't have a wiki
// Config in hand (e.g. stage sessions whose wiki is the kb, but who
// still want the twin reference for context).
func TwinReferenceSectionAt(root, projectID string) string {
	if root == "" || projectID == "" {
		return ""
	}
	dir := TwinDir(root, projectID)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	// Empty dir → no twin to reference. (Bootstrapped twins always
	// have the managed-doc set stubbed on disk.)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	hasManagedDoc := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if e.Name() == "log.md" {
			continue
		}
		hasManagedDoc = true
		break
	}
	if !hasManagedDoc {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Project digital twin\n\n")
	fmt.Fprintf(&b, "This project has a digital twin under\n  %s\n", dir)
	b.WriteString(`
It is the durable layer of intent — read it before doing
substantive work:

- vision.md — what this project is trying to be; the bets, the
  problem, the non-goals.
- architecture.md — components, boundaries, load-bearing
  decisions.
- patterns.md — named patterns and anti-patterns; the project's
  prose-form eval suite.
- operations.md — how the project runs day-to-day.
- roadmap.md — what's next: prioritized intent across near, mid,
  long term, directions, and parked.
- glossary.md — project-specific vocabulary; terse entries
  pointing back to the home doc where each term is anchored.

When your work would contradict a recorded decision in
architecture.md, name the conflict before continuing. When you'd
deviate from a recorded pattern, name the deviation. The twin
records intent; code is implementation. When they conflict, the
twin wins until a decided edit updates it (` + "`moe twin claim`" + `).

If you notice something that should edit one of these docs, leave
a note via the ` + "`moe-bureaucracy`" + ` skill.
`)
	return b.String()
}

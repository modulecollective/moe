// Package twin holds the path helpers, the doc-set declaration, and the
// structural scan for a project's digital twin — the closed-schema doc
// set under projects/<p>/digital-twin/.
//
// It was once an engine called internal/wiki: an ingest loop, a
// checkpoint, an events window, a session-end finalize that appended to
// log.md, and a schema mode generic over two wikis. Both wikis are gone
// as workflows. A project's knowledge/ tree is a plain doc tree checked
// by internal/knowledge; the twin is a plain doc tree any sdlc stage may
// write, checked by Scan here and gated at stage exit. What's left is
// the scan, the doc set it scans against, and the prompt section that
// points an agent at the tree.
package twin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirRel is the path under `projects/<project>/` where a project's
// closed-schema digital twin lives. Hard-coded — every project's twin
// sits in the same place so stage prompts and tools can compute the
// path without consulting per-project config.
const DirRel = "digital-twin"

// managedDocs is the hard-fixed set of docs every project's twin
// carries, in the order findings render. Project-agnostic:
// closed-schema means "opinions are the product", so a new doc joins
// the set by a code change here, not per-project config. What each doc
// is *for* is taught to the agent by the moe-twin skill, which is the
// one place that prose has to stay in step with a real twin.
//
// One reader now that the reflect ladder is gone: Scan, called from the
// project-doc hygiene gate after any stage commit that touched the
// tree.
var managedDocs = []string{
	"vision.md",
	"architecture.md",
	"patterns.md",
	"operations.md",
	"glossary.md",
}

// Dir returns the absolute path to the project's digital-twin
// directory under the bureaucracy root.
func Dir(root, projectID string) string {
	return filepath.Join(root, "projects", projectID, DirRel)
}

// docBlurbs is the one-line purpose each managed doc gets in the stage
// prompt's twin section. The moe-twin skill carries the long form.
var docBlurbs = map[string]string{
	"vision.md":       "what this project is trying to be; the bets, the problem, the non-goals.",
	"architecture.md": "components, boundaries, load-bearing decisions.",
	"patterns.md":     "named patterns and anti-patterns; the project's prose-form eval suite.",
	"operations.md":   "how the project runs day-to-day.",
	"glossary.md":     "project-specific vocabulary; terse entries pointing back to the home doc where each term is anchored.",
}

// ReferenceSectionAt emits a system-prompt block that names the
// project's digital twin and tells the agent to read it before doing
// substantive work. Reference, not splice — the agent decides what to
// read each turn rather than every prompt carrying the full twin.
//
// Each managed doc on disk is listed by absolute path with its line
// count. Both exist because of what the 2026-11 transcript audit found:
// agents typed twin paths relative to the wrong root, and Codex `cat`ed
// the whole twin in one call and silently lost its middle to the tool
// result cap. A doc missing from disk is not listed — pointing at it
// only manufactures a not-found error, and the twin gate already
// reports it. agent is the backend name; codex additionally gets the
// cap spelled out, since that backend is the one that truncates.
//
// Returns "" when the project has no managed twin doc on disk. Stages
// don't need to branch on this: empty input concatenates cleanly into
// buildSystemPrompt's section join.
func ReferenceSectionAt(root, projectID, agent string) string {
	if root == "" || projectID == "" {
		return ""
	}
	dir := Dir(root, projectID)
	var docs strings.Builder
	for _, name := range managedDocs {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&docs, "- %s — %s (%d lines)\n  %s\n",
			name, docBlurbs[name], lineCount(data), filepath.Join(dir, name))
	}
	if docs.Len() == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Project digital twin\n\n")
	b.WriteString(`This project has a digital twin. It is the durable layer of intent —
read it before doing substantive work. Read one doc per tool call, by
the absolute path given:

`)
	b.WriteString(docs.String())
	if agent == "codex" {
		b.WriteString(`
Your tool results are capped at about 10k tokens, and anything longer
is cut in the middle with a "truncated output" warning. A single ` + "`cat`" + `
of several docs is not a read; read a long doc in line ranges.
`)
	}
	b.WriteString(`
When your work would contradict a recorded decision in
architecture.md, name the conflict before continuing. When you'd
deviate from a recorded pattern, name the deviation. The twin
records intent; code is implementation. When they conflict, the
twin wins until a run updates it.

If a twin edit is out of scope for this run, leave a note via the
` + "`moe-bureaucracy`" + ` skill. To read what prior runs have observed
about the twin (and how to slice the journal for twin-touching
work), use the sibling ` + "`moe-context`" + ` skill.
`)
	return b.String()
}

// lineCount counts lines the way `wc -l` would, plus a final
// unterminated line — the number an agent sizing a `sed -n` range wants.
func lineCount(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

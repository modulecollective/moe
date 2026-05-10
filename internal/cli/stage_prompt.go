package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// buildSystemPrompt assembles the `--append-system-prompt` payload in the
// order described in README §"Agent Context Assembly":
//
//	soul.md                → global philosophy / quality bar
//	stages/<stage>.md      → lifecycle-phase lens (for the doc being edited)
//	operational core       → what specifically this invocation is doing
//	upstream-change banner → prereq docs that moved since last turn
//
// Per-document fragments, overrides, and upstream-document assembly are
// expected later passes; each new source of guidance slots in as another
// (string, error)-returning block below.
func buildSystemPrompt(root string, md *run.Metadata, docID, clonePath string, wikiCfg *wiki.Config) (string, error) {
	var sections []string

	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}

	if frag := moe.Stage(md.Workflow, docID); frag != "" {
		sections = append(sections, frag)
	}

	// Twin-as-context: every wiki-aware stage gets a reference block
	// pointing at the project's digital-twin/ dir (when one exists).
	// Lands before any wiki-specific section so an ingest agent reads
	// the twin first, then sees the wiki it's working on.
	if ref := wiki.TwinReferenceSectionAt(root, md.Project); ref != "" {
		sections = append(sections, ref)
	}

	sections = append(sections, operationalCore(root, md, docID, clonePath))

	if wikiCfg != nil {
		sections = append(sections, wiki.IngestPromptSection(*wikiCfg))
	}

	banner, err := upstreamChangeBanner(root, md, docID)
	if err != nil {
		return "", err
	}
	if banner != "" {
		sections = append(sections, banner)
	}

	return strings.Join(sections, "\n---\n\n"), nil
}

// upstreamChangeBanner returns a system-prompt section listing prerequisite
// documents that were re-committed after this document's most recent work
// turn, or "" if there is nothing to surface. The banner names each
// prerequisite, the absolute path to its content.md, and the SHA the agent
// last ran on, so the agent can `git -C <root> diff <sha>..HEAD -- <relpath>`
// to see what changed.
//
// Conditions for firing:
//   - docID has prerequisites declared by the run's workflow. design
//     has none in sdlc, so this is a no-op there.
//   - There has been at least one prior work turn for docID. First-turn
//     sessions get no banner — the agent will read prerequisites fresh on
//     its own; there is no "since" to compute against.
//   - At least one prerequisite document had its latest `work: update`
//     commit land *after* the active doc's last work turn.
//
// The banner is advisory. Per stages/code.md "Match the design" the
// contract is still social — we're just making the social cue legible
// instead of trusting the agent to notice on its own.
func upstreamChangeBanner(root string, md *run.Metadata, docID string) (string, error) {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", err
	}
	deps := wf.Prereqs(docID)
	if len(deps) == 0 {
		return "", nil
	}

	lastSHA, lastWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, docID)
	if err != nil {
		return "", err
	}
	if lastSHA == "" {
		return "", nil
	}

	type move struct {
		doc     string
		when    time.Time
		relPath string
	}
	var moved []move
	for _, dep := range deps {
		_, depWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, dep)
		if err != nil {
			return "", err
		}
		if depWhen.IsZero() || !depWhen.After(lastWhen) {
			continue
		}
		moved = append(moved, move{
			doc:     dep,
			when:    depWhen,
			relPath: run.ContentPath(md.Project, md.ID, dep),
		})
	}
	if len(moved) == 0 {
		return "", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Since your last turn on %q (bureaucracy commit %s),\n", docID, lastSHA)
	b.WriteString("the following prerequisite document(s) were updated and may have\n")
	b.WriteString("changed under you:\n\n")
	for _, m := range moved {
		fmt.Fprintf(&b, "- %s (updated %s)\n", m.doc, m.when.Format(time.RFC3339))
		fmt.Fprintf(&b, "  document: %s\n", filepath.Join(root, m.relPath))
		fmt.Fprintf(&b, "  diff:     git -C %s diff %s..HEAD -- %s\n", root, lastSHA, m.relPath)
	}
	b.WriteString("\nRe-read the prerequisite document(s) and reconcile your in-progress work\n")
	b.WriteString("before continuing. If the change invalidates the approach, surface it to\n")
	b.WriteString("the operator rather than smuggling a deviation in.\n")
	return b.String(), nil
}

// operationalCore is the "what are you doing right now" framing: canvas
// file, clone workspace (if any), run title. It's the one section
// that's always present — everything else in the prompt is optional
// guidance layered on top.
func operationalCore(root string, md *run.Metadata, docID, clonePath string) string {
	// Absolute path so it resolves regardless of where Claude Code's cwd
	// lands — document-only runs sit at the bureaucracy root, code-editing
	// runs sit inside the sandbox clone.
	content := filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))
	out := fmt.Sprintf(`You are collaborating with the operator on the %q document
for run %q (project %q) in a Ministry of Everything bureaucracy repo.

Your canvas for this document is the single file:
  %s

Treat the conversation as exploratory, and the file as the compressed
artifact. When the operator asks for edits, write them directly to that
file (create it if it doesn't exist). Keep the file tidy — it becomes
upstream context for downstream agents once the operator moves on.

Run title: %s
`, docID, md.ID, md.Project, content, md.Title)

	if clonePath != "" {
		out += fmt.Sprintf(`
Your working directory is a private copy-on-write clone of the target
project's submodule:
  %s
That's your code workspace — read and edit files there. The clone is
yours for the lifetime of this run; your edits are isolated from
other concurrent activities and from the canonical submodule until the
run is pushed.
`, clonePath)
	}

	// Capture-as-you-go: the close-time harvester turns each unchecked
	// line of this file into an idea run. Worded so the agent appends
	// rather than rewrites — the file accumulates across stages.
	followups := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	out += "\n" +
		"If you notice something worth doing but out of scope for this cycle —\n" +
		"adjacent cleanup, a deferred investigation, a reference to chase —\n" +
		"append a line to:\n" +
		"  " + followups + "\n" +
		"Format: - [ ] `slug` — Title (lowercase hyphenated slug, em-dash,\n" +
		"terse title, no body). The operator harvests unchecked entries into\n" +
		"ideas at close.\n"
	return out
}

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

	// Stage-location header lands before the stage fragment so the
	// agent reads "you are at code, downstream is test, then push"
	// *before* the code lens. The lens then answers "given that
	// location, here's the job." Lets every fragment stay on-topic
	// (the lens) and keeps neighbor-command names out of hand-written
	// prose — see TestFragmentsDoNotMentionNeighborCommands.
	if loc := stageLocationSection(md, docID); loc != "" {
		sections = append(sections, loc)
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

// stageLocationSection renders the generated "Stage location" block that
// tells the agent where this stage sits in the workflow ladder and, when
// applicable, the exact invocation the operator's chain prompt will
// offer once this turn closes. Sourced from the workflow registry — the
// DAG is canonical, the prose fragments stay on the lens (what to do at
// this stage), not the location.
//
// Returns "" for unknown workflows or unregistered docIDs so an upstream
// data bug fails by producing no header rather than a wrong one.
// buildSystemPrompt drops empty sections the same way it drops a missing
// stage fragment.
func stageLocationSection(md *run.Metadata, docID string) string {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return ""
	}
	stages := wf.Stages()
	inLadder := false
	for _, s := range stages {
		if s == docID {
			inLadder = true
			break
		}
	}
	if !inLadder {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Stage location\n\n")
	fmt.Fprintf(&b, "Workflow: %s — %s\n", wf.Name, renderStageLadder(stages, docID))
	fmt.Fprintf(&b, "You are at: %s\n", docID)

	prereqs := wf.Prereqs(docID)
	if len(prereqs) > 0 {
		fmt.Fprintf(&b, "\nPrevious stage: %s\n", prereqs[0])
	}

	if next := wf.Successor(docID); next != "" {
		// Same gate promptNextStage uses: only render the invocation
		// hint when the paired CommandGroup actually has a runnable
		// command for the successor. Stage names that live in the DAG
		// without a matching verb (idea today) get a bare stage name
		// and no hint.
		fmt.Fprintf(&b, "Next stage: %s\n", next)
		if g, err := LookupGroup(wf.Name); err == nil {
			if cmd := g.Lookup(next); cmd != nil {
				fmt.Fprintf(&b,
					"  When this turn closes, the chain prompt will offer\n  `moe %s %s %s %s`.\n",
					wf.Name, next, md.Project, md.ID)
			}
		}
	}
	return b.String()
}

// renderStageLadder returns the workflow's stages joined with → arrows,
// with current emphasised in **bold**. The current stage is always
// present in stages by stageLocationSection's caller — callers that
// can't guarantee it must filter first.
func renderStageLadder(stages []string, current string) string {
	parts := make([]string, len(stages))
	for i, s := range stages {
		if s == current {
			parts[i] = "**" + s + "**"
		} else {
			parts[i] = s
		}
	}
	return strings.Join(parts, " → ")
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
	// Code-bearing stages (clonePath != "") route every agent write —
	// canvas, followups, twin feedback — through `./.moe-run/` inside
	// the clone. The shuttle in clone_canvas.go owns pre-turn mirror
	// bureaucracy → clone and post-turn copy clone → bureaucracy; the
	// agent only ever touches in-cwd files, which keeps codex's
	// apply_patch project-scope check happy. For document-only stages
	// (clonePath == "") cwd is the bureaucracy root and the absolute
	// bureaucracy paths are still the right answer.
	var content, twinFeedback, followups string
	if clonePath != "" {
		runRel := filepath.Join(".", CloneRunDir)
		content = filepath.Join(runRel, "documents", docID, "content.md")
		twinFeedback = filepath.Join(runRel, "feedback", "twin.md")
		followups = filepath.Join(runRel, "followups.md")
	} else {
		content = filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))
		twinFeedback = filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "twin"))
		followups = filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	}

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

Your run-state mirror lives at `+"`./%s/`"+` inside this clone. MoE
pre-syncs only the agent-writable run files there before each turn and
copies those files back after the turn commits. Write the canvas,
`+"`followups.md`"+`, and `+"`feedback/twin.md`"+` under this
directory only — they're the agent-writable surface. Run metadata,
prior canvases, digital-twin docs, and other bureaucracy paths are
read-only context; do not edit those paths.
`, clonePath, CloneRunDir)
	}

	// Twin-feedback channel comes first so the more specific case
	// gets read while the agent is still classifying — followups is
	// the fallback. Trigger is mechanical (would acting on this edit
	// a digital-twin doc?) rather than philosophical, because the
	// philosophical phrasing ("a decision the doc doesn't reflect")
	// requires an abstraction the agent doesn't always perform.
	out += "\n" +
		"If you notice something about the project that belongs in the digital\n" +
		"twin — would acting on this note edit `digital-twin/<project>/`\n" +
		"(architecture.md, vision.md, patterns.md, operations.md, roadmap.md)? —\n" +
		"append a note to:\n" +
		"  " + twinFeedback + "\n" +
		"Free-form prose; separate notes with `---`. Name the twin doc and\n" +
		"any file:line refs so the next `moe twin reflect` knows where to\n" +
		"look. Example:\n" +
		"\n" +
		"  architecture.md says the universal gate is the only path into\n" +
		"  claim/, but cli/claim.go:84 takes an explicit-path shortcut that\n" +
		"  bypasses it. Either the gate isn't universal anymore, or claim.go\n" +
		"  needs to route through it.\n" +
		"\n" +
		"  ---\n" +
		"\n" +
		"  patterns.md \"fail loud\" claims handlers panic on bad input, but\n" +
		"  cli/foo.go:42 silently returns nil now. Decide which is canon.\n" +
		"\n" +
		"The next `moe twin reflect` picks these up as kickoff context — the\n" +
		"note arrives where the work actually happens.\n"

	// Capture-as-you-go: the close-time harvester turns each unchecked
	// entry of this file into an idea run, threading any indented body
	// into the new idea's seed canvas. Worded so the agent appends
	// rather than rewrites — the file accumulates across stages — and
	// so the body steer is "only when it would save a future agent
	// real work," to avoid replacing bare-line junk with body-padded
	// junk. The closing backward link catches the agent who drafted a
	// followup before reading the twin paragraph above.
	out += "\n" +
		"If you notice something worth doing but out of scope for this cycle —\n" +
		"adjacent cleanup, a deferred investigation, a reference to chase —\n" +
		"append an entry to:\n" +
		"  " + followups + "\n" +
		"Format: - [ ] `slug` — Title (lowercase hyphenated slug, em-dash,\n" +
		"terse title), optionally followed by an indented body of one or\n" +
		"more paragraphs (two-space indent, blank lines between paragraphs):\n" +
		"\n" +
		"  - [ ] `cleanup-foo` — Clean up foo helper\n" +
		"\n" +
		"    Why: bar/baz both reach into foo's internals; foo.go:42 is\n" +
		"    the load-bearing assumption. Fix sketch: <one sentence>.\n" +
		"\n" +
		"Use the body only when context would save a future agent real\n" +
		"work — the *why*, file:line refs, or a one-sentence approach\n" +
		"sketch. Skip the body when the title is self-explanatory. The\n" +
		"operator reviews and prunes these at termination; unchecked\n" +
		"entries become idea runs with the body carried into the seed\n" +
		"canvas.\n" +
		"\n" +
		"If acting on this entry would edit a digital-twin doc, it belongs\n" +
		"in `feedback/twin.md` above instead.\n"
	return out
}

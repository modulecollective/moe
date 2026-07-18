package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// buildSystemPrompt assembles the `--append-system-prompt` payload in the
// order described in docs/concepts.md §"How Agents Are Steered":
//
//	soul.md                → global philosophy / quality bar
//	stages/<stage>.md      → lifecycle-phase lens (for the doc being edited)
//	operational core       → what specifically this invocation is doing
//	project AGENTS.md      → project-specific guidance from the clone
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

	// Intents-as-context: the project's open intents, the operator's
	// standing direction. Lands between the twin (what the project *is*)
	// and the lore catalog (project-agnostic operational facts) — the
	// same ordering argument the lore placement records, one level up:
	// project-specific direction reads before project-agnostic facts.
	// Empty when the project has no open intents, so most turns pay
	// nothing.
	if ref := intentsReferenceSection(root, md.Project); ref != "" {
		sections = append(sections, ref)
	}

	// Lore-as-context: portable, cross-project facts catalog. Lands
	// right after the intents so project-specific intent is read before
	// the project-agnostic operational facts that build on it.
	if ref := wiki.LoreReferenceSectionAt(root); ref != "" {
		sections = append(sections, ref)
	}

	// Followups has no read-shaped reference block of its own, so a
	// nudge section names the per-run path and points at the skill.
	// Sibling of twin/lore: each of the three trace channels gets one
	// recognise-and-contribute cue before operationalCore.
	sections = append(sections, followupsReferenceSection(root, md))

	sections = append(sections, operationalCore(root, md, docID, clonePath))

	// Project-specific AGENTS.md / CLAUDE.md from the clone. Codex /
	// claude both walk from cwd up to the git root looking for these
	// files; under the cwd-inversion shape cwd = bureaucracy worktree,
	// so the project's clone-rooted AGENTS.md no longer auto-loads.
	// Read it explicitly and append as a section so project-specific
	// guidance still reaches the agent.
	if guidance := projectAgentsGuidance(clonePath); guidance != "" {
		sections = append(sections, guidance)
	}

	// Prior-runs lineage: when md is a reopen, name the prior run(s)
	// and the artifacts on disk so the agent reads what was tried
	// previously before substantive work. Lands after project guidance
	// (project rules first, then run-specific lineage) and is empty
	// for non-reopen runs, so the common case pays zero prompt cost.
	if priors := priorRunsSection(root, md); priors != "" {
		sections = append(sections, priors)
	}

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
					"  When this turn closes, the chain prompt will offer\n  `moe %s %s %s/%s`.\n",
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

// intentsReferenceSection emits a catalog of the project's open intents
// — the operator's standing direction — in the same lore-style grammar:
// one line per intent (backtick slug, em-dash, title from the canvas's
// first heading), each pointing at its canvas path, with a framing that
// tells the agent to read the ones that bear on what it's deciding.
// Bodies stay on disk; the catalog is the budgeted summary, and a lazy
// agent can skip the read everywhere except the pulse (whose fragment
// mandates it) — the accepted lore trade, ambient cheapness over
// guaranteed ingestion.
//
// Returns "" when the project has no open intents (or on any scan
// error), so the empty case slots cleanly into buildSystemPrompt's
// section join and most turns pay nothing.
func intentsReferenceSection(root, projectID string) string {
	if root == "" || projectID == "" {
		return ""
	}
	entries, err := scanOpenIntents(root, projectID)
	if err != nil || len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].slug < entries[j].slug })

	var b strings.Builder
	b.WriteString("## Project intents\n\n")
	b.WriteString(`This project has open intents — short, operator-authored statements
of where it's going: a theme, a bet, a direction it's heading. Read the
ones that bear on what you're deciding before substantive work. They
aim the discretionary calls (what to propose, what to prioritise); they
don't fence what's allowed.

`)
	for _, e := range entries {
		title := intentTitle(root, e.project, e.slug)
		path := filepath.Join(root, run.ContentPath(e.project, e.slug, dash.IntentDocID))
		fmt.Fprintf(&b, "- `%s` — %s\n  %s\n", e.slug, title, path)
	}
	b.WriteString(`
Intents are operator-authored — you read them, you never create or edit
them. If a theme looks missing, name it in a report; the operator
decides whether to park it.
`)
	return b.String()
}

// followupsReferenceSection emits a one-paragraph nudge naming the
// run's followups path and pointing at the moe-bureaucracy skill.
// Sibling of TwinReferenceSection / LoreReferenceSection: each of the
// three trace channels gets one recognise-and-contribute cue in the
// stage prompt so the agent has *capture* as a live category, while
// the skill body retains the *how* (format, inclusion bar) and only
// loads when the agent reaches for it.
func followupsReferenceSection(root string, md *run.Metadata) string {
	if root == "" || md == nil || md.Project == "" || md.ID == "" {
		return ""
	}
	path := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	var b strings.Builder
	b.WriteString("## Out-of-scope work\n\n")
	b.WriteString("If you spot work worth doing but out of scope for this canvas,\n")
	b.WriteString("leave it via the `moe-bureaucracy` skill at:\n")
	fmt.Fprintf(&b, "  %s\n", path)
	// Inline the one-line grammar so an agent that never opens the skill
	// still writes the shape the close-time harvest can parse. A wrong
	// shape is now rejected loud at close, not silently dropped.
	b.WriteString("Each entry is one line — `- [ ] `slug` — Title` (backticked\n")
	b.WriteString("lowercase slug, em-dash separator); the skill has the full format.\n")
	b.WriteString("A wrong shape is rejected at close, not silently dropped.\n")
	// Routing, not format: an entry filed here is harvested into an idea
	// and promoted to a run, so a twin-doc edit filed as a followup ends
	// up in a workflow forbidden to make it. The rule has to travel with
	// the inlined grammar for the same reason the grammar is inlined —
	// an agent that never opens the skill still has to route correctly.
	b.WriteString("Check the channel before you file: if acting on the entry would\n")
	b.WriteString("edit a digital-twin doc, it belongs in `feedback/twin.md` instead;\n")
	b.WriteString("if it's a portable fact that applies to other projects,\n")
	b.WriteString("`feedback/lore.md`. Only what's left is a followup.\n")
	b.WriteString("To check what prior runs have already filed for this project (so\n")
	b.WriteString("you don't duplicate a followup or re-decide a settled question),\n")
	b.WriteString("use `moe-context`.\n")
	return b.String()
}

// operationalCore is the "what are you doing right now" framing: canvas
// file, clone workspace (if any), run title. It's the one section
// that's always present — everything else in the prompt is optional
// guidance layered on top.
//
// Trace-recording guidance (twin observations, portable lore,
// followups) used to live here too, repeating ~90 lines on every
// turn even when nothing got recorded. That block moved to the
// moe-bureaucracy skill (skills/moe-bureaucracy/SKILL.md, materialised
// into each backend's skills/ tree — claude under sessionCwd, codex
// under the session worktree — at session open) so both backends load
// it via progressive disclosure and pay the prose cost only on turns
// where the agent actually reaches for it.
func operationalCore(root string, md *run.Metadata, docID, clonePath string) string {
	// Every agent-writable path is now its natural absolute bureaucracy
	// path. Code-bearing stages run with cwd = bureaucracy session
	// worktree and reach the project clone via --add-dir; document-only
	// stages run with cwd = sessionCwd under .moe/sessions/ and reach
	// the bureaucracy root via --add-dir. Either way, writes to canvas,
	// followups, and twin feedback land at the same absolute paths
	// MoE's commit-turn logic reads back from.
	content := filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))

	out := fmt.Sprintf(`You are collaborating with the operator on the %q document
for run %q (project %q) in a Ministry of Everything bureaucracy repo.

Your canvas for this document is the single file:
  %s

Treat the conversation as exploratory, and the file as the compressed
artifact. When the operator asks for edits, write them directly to that
file (create it if it doesn't exist). Keep the file tidy — it becomes
upstream context for downstream agents once the operator moves on.

Prior runs of this project (their canvases, their stage transcripts,
the journal sliced by run / doc / workflow) are reachable via the
`+"`moe-context`"+` skill. Reach for it before asking the operator a
question whose answer might already be in a prior run.
`, docID, md.ID, md.Project, content)

	if clonePath != "" {
		out += fmt.Sprintf(`
Your project's source tree is exposed as an additional writable
directory at:
  %s
That's where you read and edit the project's code — a private
per-run clone of the target project's submodule. Your edits there
are isolated from other concurrent activities and from the canonical
submodule until the run is pushed.

Your working directory is the bureaucracy session worktree, where the
canvas above lives at its natural path. Edit code under the clone
path, edit run artifacts (canvas, followups, twin feedback) at the
absolute bureaucracy paths named in this prompt. Run metadata, prior
canvases, digital-twin docs, and other bureaucracy paths are
read-only context; do not edit those paths.
`, clonePath)
	}
	return out
}

// projectAgentsGuidance emits a path-mention section pointing the agent
// at the project's AGENTS.md / CLAUDE.md inside the clone. Under the
// cwd-inversion shape cwd is the bureaucracy session worktree, not the
// clone, so codex's / claude's native cwd-walk discovery doesn't reach
// these files; the prompt tells the agent where they live and trusts
// it to read them on its first relevant action.
//
// Only paths that actually exist on disk are mentioned, so a project
// without these files gets no prompt cost and no false instruction.
// Returns "" when clonePath is empty or neither file exists — the
// caller drops empty sections.
//
// This replaces the prior body-inline approach: AGENTS.md / CLAUDE.md
// can run hundreds of lines (moe's own AGENTS.md is the existence
// proof) and inlining it on every turn paid the cost even when the
// guidance never got read. The path-mention is one short paragraph
// the agent reads once.
func projectAgentsGuidance(clonePath string) string {
	if clonePath == "" {
		return ""
	}
	var existing []string
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(clonePath, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		existing = append(existing, path)
	}
	if len(existing) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Project guidance\n\n")
	b.WriteString("The project's source tree is at:\n")
	fmt.Fprintf(&b, "  %s\n", clonePath)
	b.WriteString("A project-specific agent guidance file exists at:\n")
	for _, p := range existing {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	b.WriteString("Read it before substantive project work — it carries\n")
	b.WriteString("project-specific rules (build conventions, internal seams,\n")
	b.WriteString("etc.) that override general defaults.\n")
	return b.String()
}

// priorRunsDepthCap bounds the reopen chain walk. Realistic chains
// are 1–2 deep; the cap is a defensive ceiling so a pathological
// chain (or a loop, though Metadata.ReopenOf is set once at open and
// never edited) can't blow the prompt.
const priorRunsDepthCap = 8

// priorRunsSection emits a "## Prior runs" block when md is a reopen,
// naming each prior run in the chain (most-recent first) and listing
// the existing document content.md paths plus followups.md so the
// agent reads what was tried previously before substantive work.
//
// Chain walk: load md's prior via Metadata.ReopenOf, then its prior,
// repeating until a run with empty ReopenOf, a load error (treated
// as a chain terminator — don't take down the prompt over a missing
// or unreadable prior), or the depth cap.
//
// Returns "" when md.ReopenOf is empty, so non-reopen runs pay zero
// prompt cost.
func priorRunsSection(root string, md *run.Metadata) string {
	if root == "" || md == nil || md.ReopenOf == "" || md.Project == "" {
		return ""
	}
	type priorEntry struct {
		slug   string
		status string
		paths  []string
	}
	var priors []priorEntry
	slug := md.ReopenOf
	for depth := 0; depth < priorRunsDepthCap && slug != ""; depth++ {
		pmd, err := run.Load(root, md.Project, slug)
		if err != nil {
			break
		}
		priors = append(priors, priorEntry{
			slug:   slug,
			status: pmd.Status,
			paths:  priorRunArtifactPaths(root, md.Project, slug),
		})
		slug = pmd.ReopenOf
	}
	if len(priors) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Prior runs\n\n")
	b.WriteString("This run is a reopen. Read the prior runs' artifacts before\n")
	b.WriteString("substantive work — they record what was tried previously and\n")
	b.WriteString("why this run exists at all. Listed most-recent first:\n")
	for _, p := range priors {
		b.WriteString("\n")
		fmt.Fprintf(&b, "`%s` (status: %s):\n", p.slug, p.status)
		for _, path := range p.paths {
			fmt.Fprintf(&b, "  %s\n", path)
		}
	}
	return b.String()
}

// priorRunArtifactPaths returns absolute paths to a prior run's
// document content.md files (one per documents/<doc>/content.md that
// exists on disk) and followups.md if present. Sorted so prompt
// output is deterministic across runs. Returns nil when nothing on
// disk matches — the caller still emits the slug line so the agent
// knows the prior existed.
func priorRunArtifactPaths(root, projectID, slug string) []string {
	runDir := filepath.Join(root, run.Dir(projectID, slug))
	matches, _ := filepath.Glob(filepath.Join(runDir, "documents", "*", "content.md"))
	sort.Strings(matches)
	paths := matches
	followups := filepath.Join(root, run.FollowupsPath(projectID, slug))
	if _, err := os.Stat(followups); err == nil {
		paths = append(paths, followups)
	}
	return paths
}

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/knowledge"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// knowledge_gate.go holds the close-time hygiene gate over a project's
// `projects/<p>/knowledge/` tree.
//
// The tree used to be the kb workflow's deliverable, and its structural
// check was that workflow's lint pre-scan — a separate session the
// operator had to remember to run. It is now an ordinary sdlc write
// target (see projectCommitDirs), so the check moved to where the
// writing happens: a turn that commits a knowledge edit and leaves the
// tree structurally broken doesn't get to close clean. The agent that
// broke the index fixes it with its own context still loaded, instead of
// a survey block noticing weeks later and an eventual fix run paying for
// it twice.
//
// Trigger precision is the cost story: the gate reads the turn's own
// commit, so a turn that didn't touch knowledge pays nothing.

// knowledgeSubdir is the directory name under projects/<p>/ the gate
// watches. Matches the entry projectCommitDirs whitelists for sdlc.
const knowledgeSubdir = "knowledge"

// dispatchKnowledgeFixTurn re-enters runStageSession for the gate's one
// bounded fix turn. Assigned in init() rather than referenced directly:
// runStageSession is a var, and a function it calls referencing it back
// is an initialization cycle. Same shape (and same reason) as
// openKickbackSession in stage_next.go.
var dispatchKnowledgeFixTurn func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int

func init() {
	dispatchKnowledgeFixTurn = func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
		return runStageSession(projectID, runID, docID, opts, stdout, stderr)
	}
}

// knowledgeRel is the bureaucracy-relative, slash-separated path to a
// project's knowledge tree — the shape git pathspecs want.
func knowledgeRel(projectID string) string {
	return filepath.ToSlash(filepath.Join(project.Dir(projectID), knowledgeSubdir))
}

// commitTouchedKnowledge reports whether the commit at HEAD in dir
// changed anything under the project's knowledge tree. Called from
// inside the session worktree right after commitTurn, where HEAD *is*
// this turn's commit.
//
// An error reads as "didn't touch it": the gate is a hygiene check, and
// failing a turn because a git probe hiccuped would be worse than
// missing one scan.
func commitTouchedKnowledge(dir, projectID string) bool {
	out, err := git.Output(dir, "show", "--name-only", "--format=", "HEAD", "--", knowledgeRel(projectID))
	return err == nil && strings.TrimSpace(out) != ""
}

// enforceKnowledgeHygiene scans the project's knowledge tree after the
// turn's commit has landed and decides how the turn ends.
//
// Clean tree: 0, nothing printed. Findings: they print verbatim, and
// then the two dispositions split on whether anyone is watching.
//
//   - Interactive — refuse (exit 1). The commit stays (the agent's topic
//     edits are real work, unlike twin's reflect gate they don't get
//     thrown away), but the cascade prompt never fires. The operator
//     reads the findings and re-runs the stage; the session resumes with
//     the agent's context intact, one message from the fix.
//   - Headless — one bounded fix turn, then park. Same session, same
//     stage, re-dispatched with the findings as the prompt, then a
//     re-scan. `retries >= 1` in shape and in spirit: a real finding no
//     longer dies silently in a chain, and if the fix doesn't stick the
//     run parks exactly as it would have.
//
// Operator hand-edits to knowledge/ never reach here — the gate polices
// agent writers, and the tree is the operator's.
func enforceKnowledgeHygiene(root string, md *run.Metadata, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
	dir := filepath.Join(root, knowledgeRel(md.Project))
	findings, err := knowledge.Scan(dir)
	if err != nil {
		moePrintf(stderr, "%s: knowledge scan: %v\n", docID, err)
		return 1
	}
	if findings.IsEmpty() {
		return 0
	}

	moePrintf(stderr, "%s: this turn edited %s and left %d structural finding(s).\n",
		docID, knowledgeRel(md.Project), findings.Count())
	moePrintln(stderr, knowledge.Render(findings))

	if !opts.Headless {
		moePrintln(stderr, "The edits are committed. Re-run this stage and have the agent "+
			"clear the findings above — the session resumes with its context.")
		return 1
	}

	moePrintf(stdout, "%s: knowledge hygiene (headless fix turn)\n", docID)
	fixOpts := opts
	fixOpts.knowledgeFixTurn = true
	fixOpts.InitialPrompt = knowledgeFixKickoff(findings, knowledgeRel(md.Project))
	// The kickoff is the findings; a builder would overwrite it with the
	// stage's ordinary framing.
	fixOpts.InitialPromptBuilder = nil
	if code := dispatchKnowledgeFixTurn(md.Project, md.ID, docID, fixOpts, stdout, stderr); code != 0 {
		return code
	}

	after, err := knowledge.Scan(dir)
	if err != nil {
		moePrintf(stderr, "%s: knowledge scan: %v\n", docID, err)
		return 1
	}
	if after.IsEmpty() {
		return 0
	}
	moePrintf(stderr, "%s: knowledge findings survived the fix turn; parking.\n", docID)
	moePrintln(stderr, knowledge.Render(after))
	return 1
}

// knowledgeFixKickoff is the prompt the headless fix turn opens on. It
// carries the findings verbatim and nothing else — the agent already has
// the stage's framing from the turn that produced them.
//
// The canvas line is load-bearing rather than housekeeping: session
// close refuses a turn whose canvas blob is unchanged from main, so a
// fix turn that only edits index.md would fail on the way out.
func knowledgeFixKickoff(f knowledge.Findings, rel string) string {
	var b strings.Builder
	b.WriteString("Your knowledge-tree edits committed, but the structural check " +
		"found problems the stage won't close on. Fix them now — nothing else. " +
		"Then add one line to your canvas naming what you fixed, so the turn " +
		"has something to land.\n\n")
	b.WriteString(knowledge.Render(f))
	fmt.Fprintf(&b, "\nThe tree is at %s. Topic docs live flat under topics/; "+
		"index.md is the catalog and every topic must appear in it.\n", rel)
	return b.String()
}

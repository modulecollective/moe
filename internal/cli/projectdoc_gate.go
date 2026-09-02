package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/knowledge"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// projectdoc_gate.go holds the close-time hygiene gate over the project
// doc trees an sdlc stage may write: `projects/<p>/knowledge/` and
// `projects/<p>/digital-twin/`.
//
// Both trees used to be a workflow's deliverable, and each had its
// structural check welded to that workflow's own session — kb's lint
// pre-scan, reflect's post-flight seal gate. Both are now ordinary sdlc
// write targets (see projectCommitDirs), so the check moved to where
// the writing happens: a turn that commits an edit and leaves the tree
// structurally broken doesn't get to close clean. The agent that broke
// the index or invented a cross-reference fixes it with its own context
// still loaded, instead of a survey block noticing weeks later and an
// eventual fix run paying for it twice.
//
// One gate, two scanners — not two gates. The dispositions, the fix
// turn, and the re-scan are the expensive parts and they are identical;
// only the scan differs.
//
// Trigger precision is the cost story: the gate reads the turn's own
// commit, so a turn that touched neither tree pays nothing, and a turn
// that touched only one scans only that one.

// projectDocScanner is one watched dir plus the scan that checks it.
// scan returns the finding count and the block to print; zero means
// clean and the block is never rendered.
type projectDocScanner struct {
	// subdir is the directory name under projects/<p>/. Matches an
	// entry projectCommitDirs whitelists for sdlc.
	subdir string
	// fixHint is the one-line orientation appended to the fix turn's
	// kickoff — what shape the tree is supposed to have.
	fixHint string
	scan    func(dir string) (count int, rendered string, err error)
}

// projectDocScanners is the watch list, in the order findings render.
// Adding a third doc tree is a scanner here plus its entry in
// projectCommitDirs.
var projectDocScanners = []projectDocScanner{
	{
		subdir:  knowledgeSubdir,
		fixHint: "Topic docs live flat under topics/; index.md is the catalog and every topic must appear in it.",
		scan: func(dir string) (int, string, error) {
			f, err := knowledge.Scan(dir)
			if err != nil {
				return 0, "", err
			}
			return f.Count(), knowledge.Render(f), nil
		},
	},
	{
		subdir:  wiki.TwinDirRel,
		fixHint: "The doc set is fixed (vision, architecture, patterns, operations, glossary); a cross-reference must name a heading that exists in the doc it cites.",
		scan: func(dir string) (int, string, error) {
			f, err := wiki.Scan(wiki.Config{ContentDir: dir, ManagedDocs: twinManagedDocs})
			if err != nil {
				return 0, "", err
			}
			return f.Count(), wiki.RenderFindings(f), nil
		},
	},
}

// knowledgeSubdir is the knowledge tree's directory name under
// projects/<p>/. Named rather than inlined because the prompt and the
// gate both reach for it.
const knowledgeSubdir = "knowledge"

// dispatchProjectDocFixTurn re-enters runStageSession for the gate's one
// bounded fix turn. Assigned in init() rather than referenced directly:
// runStageSession is a var, and a function it calls referencing it back
// is an initialization cycle. Same shape (and same reason) as
// openKickbackSession in stage_next.go.
var dispatchProjectDocFixTurn func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int

func init() {
	dispatchProjectDocFixTurn = func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
		return runStageSession(projectID, runID, docID, opts, stdout, stderr)
	}
}

// projectDocRel is the bureaucracy-relative, slash-separated path to one
// of a project's watched doc trees — the shape git pathspecs want.
func projectDocRel(projectID, subdir string) string {
	return filepath.ToSlash(filepath.Join(project.Dir(projectID), subdir))
}

// commitTouchedProjectDocs returns the watched subdirs the commit at
// HEAD in dir changed something under. Called from inside the session
// worktree right after commitTurn, where HEAD *is* this turn's commit.
//
// An error reads as "didn't touch it": the gate is a hygiene check, and
// failing a turn because a git probe hiccuped would be worse than
// missing one scan.
func commitTouchedProjectDocs(dir, projectID string) []string {
	var touched []string
	for _, s := range projectDocScanners {
		out, err := git.Output(dir, "show", "--name-only", "--format=", "HEAD", "--", projectDocRel(projectID, s.subdir))
		if err == nil && strings.TrimSpace(out) != "" {
			touched = append(touched, s.subdir)
		}
	}
	return touched
}

// projectDocFindings scans each named subdir and returns the total
// finding count plus the rendered blocks, joined in watch-list order.
func projectDocFindings(root, projectID string, subdirs []string) (int, string, error) {
	var total int
	var blocks []string
	for _, s := range projectDocScanners {
		if !slices.Contains(subdirs, s.subdir) {
			continue
		}
		count, rendered, err := s.scan(filepath.Join(root, projectDocRel(projectID, s.subdir)))
		if err != nil {
			return 0, "", fmt.Errorf("%s scan: %w", s.subdir, err)
		}
		total += count
		if count > 0 {
			blocks = append(blocks, rendered)
		}
	}
	return total, strings.Join(blocks, "\n"), nil
}

// enforceProjectDocHygiene scans the doc trees this turn's commit
// touched and decides how the turn ends.
//
// Clean: 0, nothing printed. Findings: they print verbatim, and then
// the two dispositions split on whether anyone is watching.
//
//   - Interactive — refuse (exit 1). The commit stays (the agent's doc
//     edits are real work, and unlike reflect's old seal gate they don't
//     get thrown away), but the cascade prompt never fires. The operator
//     reads the findings and re-runs the stage; the session resumes with
//     the agent's context intact, one message from the fix.
//   - Headless — one bounded fix turn, then park. Same session, same
//     stage, re-dispatched with the findings as the prompt, then a
//     re-scan. `retries >= 1` in shape and in spirit: a real finding no
//     longer dies silently in a chain, and if the fix doesn't stick the
//     run parks exactly as it would have.
//
// Operator hand-edits never reach here — the gate polices agent
// writers, and the trees are the operator's.
func enforceProjectDocHygiene(root string, md *run.Metadata, touched []string, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
	count, rendered, err := projectDocFindings(root, md.Project, touched)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", docID, err)
		return 1
	}
	if count == 0 {
		return 0
	}

	moePrintf(stderr, "%s: this turn edited %s and left %d structural finding(s).\n",
		docID, touchedRels(md.Project, touched), count)
	moePrintln(stderr, rendered)

	if !opts.Headless {
		moePrintln(stderr, "The edits are committed. Re-run this stage and have the agent "+
			"clear the findings above — the session resumes with its context.")
		return 1
	}

	moePrintf(stdout, "%s: project-doc hygiene (headless fix turn)\n", docID)
	fixOpts := opts
	fixOpts.projectDocFixTurn = true
	fixOpts.InitialPrompt = projectDocFixKickoff(md.Project, touched, rendered)
	// The kickoff is the findings; a builder would overwrite it with the
	// stage's ordinary framing.
	fixOpts.InitialPromptBuilder = nil
	if code := dispatchProjectDocFixTurn(md.Project, md.ID, docID, fixOpts, stdout, stderr); code != 0 {
		return code
	}

	after, afterRendered, err := projectDocFindings(root, md.Project, touched)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", docID, err)
		return 1
	}
	if after == 0 {
		return 0
	}
	moePrintf(stderr, "%s: findings survived the fix turn; parking.\n", docID)
	moePrintln(stderr, afterRendered)
	return 1
}

// touchedRels renders the watched paths this turn touched, for the
// gate's one-line "this turn edited X" header.
func touchedRels(projectID string, subdirs []string) string {
	rels := make([]string, 0, len(subdirs))
	for _, s := range projectDocScanners {
		if slices.Contains(subdirs, s.subdir) {
			rels = append(rels, projectDocRel(projectID, s.subdir))
		}
	}
	return strings.Join(rels, " and ")
}

// projectDocFixKickoff is the prompt the headless fix turn opens on. It
// carries the findings verbatim plus each touched tree's shape hint —
// the agent already has the stage's framing from the turn that produced
// them.
//
// The canvas line is load-bearing rather than housekeeping: session
// close refuses a turn whose canvas blob is unchanged from main, so a
// fix turn that only edits index.md would fail on the way out.
func projectDocFixKickoff(projectID string, touched []string, rendered string) string {
	var b strings.Builder
	b.WriteString("Your project-doc edits committed, but the structural check " +
		"found problems the stage won't close on. Fix them now — nothing else. " +
		"Then add one line to your canvas naming what you fixed, so the turn " +
		"has something to land.\n\n")
	b.WriteString(rendered)
	b.WriteString("\n")
	for _, s := range projectDocScanners {
		if !slices.Contains(touched, s.subdir) {
			continue
		}
		fmt.Fprintf(&b, "%s is at %s. %s\n", s.subdir, projectDocRel(projectID, s.subdir), s.fixHint)
	}
	return b.String()
}

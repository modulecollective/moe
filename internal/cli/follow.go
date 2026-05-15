package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
)

// `moe follow` resolves which run/doc/diff-base is most worth watching
// right now and prints it. It is a printf-and-exit: no spawn, no
// watcher, no poll loop. The operator's shell wraps the resolver to
// drive `less +F` and `hunk diff --watch` from their own panes — moe
// owns "what should I watch?", less and hunk own "tail this file" and
// "show this diff."
//
// Default form is human-readable (workflow:stage, canvas path, base,
// dir). Single-value forms (--path, --base, --dir) print one absolute
// line apiece for `$(moe follow --path)` capture. --shell emits
// eval-able MOE_FOLLOW_* assignments so a wrapper can capture the
// whole tuple atomically. With no live target moe prints a one-line
// idle status to stderr and exits non-zero so shell loops terminate
// cleanly.
//
// A candidate is an in-progress run with an open stage session — work-
// being-done. Parked runs are work-to-do; `dash` is the surface for
// those. Most-recent journal activity tie-breaks. --run pins to a
// specific id; --project narrows the candidate pool. Pinned-but-no-
// live-session falls through to idle.
//
// Pieces reused from `dash`: same Scan, same journal index, same
// session.List for liveness. follow is the across-runs counterpart to
// dash's within-run liveness picker.

func init() {
	Register(&Command{
		Name:    "follow",
		Summary: "resolve and print the run/doc/diff-base currently in play",
		Run:     runFollow,
	})
}

func runFollow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathOnly := fs.Bool("path", false, "print only the canvas path")
	baseOnly := fs.Bool("base", false, "print only the diff base")
	dirOnly := fs.Bool("dir", false, "print only the workspace dir for hunk")
	shellF := fs.Bool("shell", false, "print eval-able MOE_FOLLOW_* assignments")
	projectF := fs.String("project", "", "restrict to runs in this project")
	runF := fs.String("run", "", "pin to a specific run id (matches across projects)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe follow [--path | --base | --dir | --shell] [--project <id>] [--run <id>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Resolves the run most worth watching: the most recent in-progress run")
		moePrintln(stderr, "with an open stage session. Code sessions resolve to the run's sandbox")
		moePrintln(stderr, "clone diffed against the project's default branch; every other stage")
		moePrintln(stderr, "resolves to the session's bureaucracy worktree diffed against")
		moePrintln(stderr, "merge-base(HEAD, main).")
		moePrintln(stderr, "")
		moePrintln(stderr, "Default form prints a multi-line human summary. --path/--base/--dir")
		moePrintln(stderr, "print one absolute line for $(...) capture; --shell prints eval-able")
		moePrintln(stderr, "MOE_FOLLOW_* assignments. Mutually exclusive.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Idle (no live session) prints to stderr and exits 1 so shell loops stop:")
		moePrintln(stderr, "    while p=$(moe follow --path); do less +F \"$p\"; done")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if countTrue(*pathOnly, *baseOnly, *dirOnly, *shellF) > 1 {
		moePrintln(stderr, "moe follow: --path, --base, --dir, --shell are mutually exclusive")
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	target, summary, err := pickFollowTarget(root, *projectF, *runF)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if target.Dir == "" {
		moePrintln(stderr, idleLine(summary))
		return 1
	}

	switch {
	case *pathOnly:
		fmt.Fprintln(stdout, target.Canvas)
	case *baseOnly:
		fmt.Fprintln(stdout, target.Base)
	case *dirOnly:
		fmt.Fprintln(stdout, target.Dir)
	case *shellF:
		printFollowShell(stdout, target)
	default:
		printFollowHuman(stdout, target)
	}
	return 0
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// followTarget is the (workspace dir, diff base) tuple plus the
// identity fields the human and shell printers need. Canvas is the
// absolute path of the content.md the stage session is editing — it
// sits under the session worktree, not under the operator's
// bureaucracy checkout, so code stages (where Dir is the sandbox
// clone) still report a Canvas under the bureaucracy worktree where
// the agent's edits actually land. Empty Dir means no candidate —
// the caller renders idle.
type followTarget struct {
	Dir      string
	Canvas   string
	Base     string
	Workflow string
	Stage    string
	Project  string
	Run      string
}

// followSummary captures the figures the idle line reports: total
// active run count and the single most-recently-active run, when any.
type followSummary struct {
	activeCount int
	last        *followLast // nil when nothing's active.
}

type followLast struct {
	project, run, state string
}

// pickFollowTarget resolves the workspace hunk should diff, plus the
// idle-line summary for when no run is in play. Returns an empty
// followTarget when no candidate exists; the caller renders the
// summary instead.
//
// A candidate is an in-progress run with an open stage session — work-
// being-done. Parked runs (work-to-do but untouched) deliberately
// don't surface here; that's `dash`'s job. Most-recent journal
// activity wins. projectFilter, when non-empty, restricts the
// candidate pool to runs in that project. runFilter, when non-empty,
// locks to that run id — the operator's pin overrides recency, but a
// pinned run with no open session still falls through to idle so the
// caller (or the operator's wrapper) can re-check until one opens.
func pickFollowTarget(root, projectFilter, runFilter string) (followTarget, followSummary, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return followTarget{}, followSummary{}, err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return followTarget{}, followSummary{}, err
	}
	// session.List is read-only; a worktree-list error shouldn't
	// suppress the idle summary, so swallow and proceed with no
	// liveness signal — auto-pick will simply find no candidates.
	sessionsByRun := make(map[string][]*session.Session)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionsByRun[s.Run] = append(sessionsByRun[s.Run], s)
		}
	}
	summary := buildFollowSummary(root, mds, idx)

	if runFilter != "" {
		for _, md := range mds {
			if md.ID != runFilter {
				continue
			}
			if projectFilter != "" && md.Project != projectFilter {
				continue
			}
			sess := pickSessionForRun(sessionsByRun[md.ID])
			if sess == nil {
				return followTarget{}, summary, nil
			}
			target, err := resolveFollowTarget(root, md, sess)
			if err != nil {
				return followTarget{}, summary, err
			}
			return target, summary, nil
		}
		return followTarget{}, summary, nil
	}

	type cand struct {
		target followTarget
		when   time.Time
	}
	var cands []cand
	for _, md := range mds {
		if md.Status != run.StatusInProgress {
			continue
		}
		if projectFilter != "" && md.Project != projectFilter {
			continue
		}
		sess := pickSessionForRun(sessionsByRun[md.ID])
		if sess == nil {
			continue
		}
		target, err := resolveFollowTarget(root, md, sess)
		if err != nil {
			return followTarget{}, summary, err
		}
		if target.Dir == "" {
			continue
		}
		cands = append(cands, cand{
			target: target,
			when:   idx.LastActivity[md.ID],
		})
	}
	if len(cands) == 0 {
		return followTarget{}, summary, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].when.After(cands[j].when)
	})
	return cands[0].target, summary, nil
}

// pickSessionForRun chooses a single session per run when more than
// one happens to be open. A run normally has at most one open session
// at a time, but a botched close could leave an orphan around;
// preferring code over design over alphabetical produces a
// deterministic answer that biases toward the workspace nearest the
// run's likely live stage.
func pickSessionForRun(sessions []*session.Session) *session.Session {
	if len(sessions) == 0 {
		return nil
	}
	if len(sessions) == 1 {
		return sessions[0]
	}
	sort.Slice(sessions, func(i, j int) bool {
		ri, rj := stageRank(sessions[i].Doc), stageRank(sessions[j].Doc)
		if ri != rj {
			return ri < rj
		}
		return sessions[i].Doc < sessions[j].Doc
	})
	return sessions[0]
}

// stageRank biases pickSessionForRun toward the doc most likely to be
// the live workspace. Lower wins.
func stageRank(doc string) int {
	switch doc {
	case "code":
		return 0
	case "design":
		return 1
	default:
		return 2
	}
}

// resolveFollowTarget routes a session to the workspace tuple hunk
// runs in. Both code sessions (sandbox worktree) and document
// sessions (bureaucracy worktree) diff against
// `merge-base(HEAD, <default-branch>)` — the commit at which the
// session branch diverged from the default branch. That anchor moves
// with the session: on a fresh first turn it sits on the open commit
// (so the diff shows the session-start commit plus the agent's
// edits); on a resumed turn it sits on whatever the default-branch's
// HEAD was when the session opened (so unrelated commits that landed
// after open are excluded — they're below the merge base). The dir
// is stat'd as defense-in-depth so an orphaned session record
// (worktree dir gone, or sandbox not yet attached) idles instead of
// resolving to a non-existent cwd.
func resolveFollowTarget(root string, md *run.Metadata, sess *session.Session) (followTarget, error) {
	if followDocUsesSandbox(sess.Doc) {
		dir := sandbox.Path(root, md.Project, md.ID)
		if _, err := os.Stat(dir); err != nil {
			return followTarget{}, nil
		}
		proj, err := project.Load(root, md.Project)
		if err != nil {
			return followTarget{}, err
		}
		// merge-base, not the default branch directly: under the
		// worktree primitive the sandbox shares the canonical's ref
		// DB, so a moving default-branch tip would otherwise drag
		// unrelated commits into the diff. The merge base is fixed at
		// session-open and stable across turns.
		out, err := git.Output(dir, "merge-base", "HEAD", proj.DefaultBranch)
		if err != nil {
			return followTarget{}, fmt.Errorf("follow: merge-base: %w", err)
		}
		base := strings.TrimSpace(out)
		return followTarget{
			Dir:      dir,
			Canvas:   filepath.Join(dir, CloneRunDir, "documents", sess.Doc, "content.md"),
			Base:     base,
			Workflow: md.Workflow,
			Stage:    sess.Doc,
			Project:  md.Project,
			Run:      md.ID,
		}, nil
	}
	if _, err := os.Stat(sess.WorktreePath); err != nil {
		return followTarget{}, nil
	}
	out, err := git.Output(sess.WorktreePath, "merge-base", "HEAD", "main")
	if err != nil {
		return followTarget{}, fmt.Errorf("follow: merge-base: %w", err)
	}
	base := strings.TrimSpace(out)
	return followTarget{
		Dir:      sess.WorktreePath,
		Canvas:   filepath.Join(sess.WorktreePath, run.ContentPath(md.Project, md.ID, sess.Doc)),
		Base:     base,
		Workflow: md.Workflow,
		Stage:    sess.Doc,
		Project:  md.Project,
		Run:      md.ID,
	}, nil
}

func followDocUsesSandbox(docID string) bool {
	switch docID {
	case "code", "test", "push":
		return true
	default:
		return false
	}
}

// buildFollowSummary rolls scanned metadata into the figures the idle
// line needs. "Active" mirrors dash's bucketActiveRuns: in_progress
// or pushed runs from non-idea workflows. The "last" entry is the
// single most-recently-active run by journal activity, with a state
// label suitable for inline display ("awaiting merge" or
// "<workflow>:<stage>"). Idea runs and terminal statuses don't count.
func buildFollowSummary(root string, mds []*run.Metadata, idx *run.JournalIndex) followSummary {
	type row struct {
		md    *run.Metadata
		when  time.Time
		state string
	}
	var rows []row
	for _, md := range mds {
		if md.Workflow == ideaWorkflow {
			continue
		}
		switch md.Status {
		case run.StatusInProgress, run.StatusPushed:
		default:
			continue
		}
		rows = append(rows, row{
			md:    md,
			when:  idx.LastActivity[md.ID],
			state: stateForActive(root, md),
		})
	}
	sum := followSummary{activeCount: len(rows)}
	if len(rows) == 0 {
		return sum
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].when.After(rows[j].when) })
	top := rows[0]
	sum.last = &followLast{project: top.md.Project, run: top.md.ID, state: top.state}
	return sum
}

// stateForActive renders the inline state cell for the idle line's
// "last:" segment. Pushed runs are "awaiting merge"; in_progress runs
// carry their workflow's parked next-stage name. A workflow-lookup or
// Next failure degrades to the bare workflow name rather than dropping
// the row — the operator still sees *which* run was last touched.
func stateForActive(root string, md *run.Metadata) string {
	if md.Status == run.StatusPushed {
		return "awaiting merge"
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return md.Workflow
	}
	next, kind, err := wf.Next(root, md)
	if err != nil || kind != NextKindStage || next == "" {
		return md.Workflow
	}
	return md.Workflow + ":" + next
}

// idleLine renders the one-line idle status that lands on stderr when
// no run is in play:
//
//	(no run in play · 2 active · last: tele fix-it awaiting merge)
//
// With no active runs the trailing "last:" segment drops.
func idleLine(s followSummary) string {
	parts := []string{
		"no run in play",
		fmt.Sprintf("%d active", s.activeCount),
	}
	if s.last != nil {
		parts = append(parts, fmt.Sprintf("last: %s %s %s",
			s.last.project, s.last.run, s.last.state))
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

// printFollowHuman emits the multi-line default form: one labeled
// line per fact, every label padded to the longest (`workflow:`) so
// values line up. All paths are absolute so the operator can copy a
// line and paste it into another shell without thinking about cwd.
func printFollowHuman(w io.Writer, t followTarget) {
	rows := []struct{ label, value string }{
		{"project", t.Project},
		{"run", t.Run},
		{"workflow", t.Workflow},
		{"stage", t.Stage},
		{"canvas", t.Canvas},
		{"base", t.Base},
		{"dir", t.Dir},
	}
	for _, r := range rows {
		moePrintf(w, "%-9s %s\n", r.label+":", r.value)
	}
}

// printFollowShell emits eval-able assignments so a wrapper can
// capture the whole tuple in one shot:
//
//	eval "$(moe follow --shell)" || return $?
//
// Values are POSIX single-quoted; embedded single-quotes are escaped
// via the standard close-quote / backslash-quote / re-open-quote
// dance (see shellQuote), so paths containing spaces or quotes
// round-trip through `eval` cleanly.
func printFollowShell(w io.Writer, t followTarget) {
	pairs := []struct{ k, v string }{
		{"MOE_FOLLOW_PATH", t.Canvas},
		{"MOE_FOLLOW_BASE", t.Base},
		{"MOE_FOLLOW_DIR", t.Dir},
		{"MOE_FOLLOW_PROJECT", t.Project},
		{"MOE_FOLLOW_RUN", t.Run},
		{"MOE_FOLLOW_STAGE", t.Stage},
		{"MOE_FOLLOW_WORKFLOW", t.Workflow},
	}
	for _, p := range pairs {
		fmt.Fprintf(w, "%s=%s\n", p.k, shellQuote(p.v))
	}
}

// shellQuote wraps s in POSIX single quotes. Each embedded
// single-quote is replaced with the four-byte sequence
// close-quote, backslash, quote, open-quote — the standard POSIX
// trick for smuggling an apostrophe through a single-quoted string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

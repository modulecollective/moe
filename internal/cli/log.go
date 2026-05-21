package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

// `moe <workflow> log` renders a past stage's agent transcript
// (thread-claude.jsonl / thread-codex.jsonl) as plain text using the
// same renderer the post-headless auto-tail uses (internal/transcript),
// no `--tail` cap. Six per-workflow wrappers (in idea/sdlc/kb/
// metamoe/hooks_workflow/twin) parse positional args and delegate
// here; this file owns the resolver (project / @latest / workflow /
// run / stage validation, agent disambiguation) and the render to
// stdout.
//
// Shape mirrors `runCat`: namespaced under each workflow group so
// `@latest` and the stage list resolve in workflow context.
// Single-stage workflows (idea, meta-moe, hooks) pass a non-empty
// defaultStage so the operator can omit the stage argument.
//
// A stage with both thread-claude.jsonl and thread-codex.jsonl requires
// `--agent` to disambiguate; a stage with one file picks it implicitly.
// Operator pipes to `less` for paging — no built-in pager keeps the
// command predictable in scripts.

// runLog returns the typed Command.Run for `moe <workflow> log`.
// defaultStage, when non-empty, is the stage used when the operator
// omits a stage argument — picked up automatically by single-stage
// workflows (idea, meta-moe, hooks). Pass "" to force the operator
// to name a stage.
func runLog(workflow, defaultStage string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet(workflow+" log", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agentF := fs.String("agent", "", "render claude or codex (required when the stage has both)")
		fs.Usage = func() {
			if defaultStage == "" {
				moePrintf(stderr, "usage: moe %s log <project>/<run> <stage> [--agent claude|codex]\n", workflow)
			} else {
				moePrintf(stderr, "usage: moe %s log <project>/<run> [<stage>] [--agent claude|codex]\n", workflow)
			}
			moePrintln(stderr, "")
			moePrintln(stderr, "Renders the agent's per-session JSONL transcript for a stage as plain")
			moePrintln(stderr, "text. Pipe to less for paging. Pass @latest in the <run> slot to read")
			moePrintln(stderr, "the most-recent run in this workflow.")
			fs.PrintDefaults()
		}
		if err := fs.Parse(reorderFlags(fs, args)); err != nil {
			return 2
		}
		n := fs.NArg()
		if n < 1 || n > 2 {
			fs.Usage()
			return 2
		}
		if n == 1 && defaultStage == "" {
			fs.Usage()
			return 2
		}
		projectID, runID, err := splitProjectRun(fs.Arg(0))
		if err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 2
		}
		stage := defaultStage
		if n == 2 {
			stage = fs.Arg(1)
		}

		switch *agentF {
		case "", "claude", "codex":
		default:
			moePrintf(stderr, "moe %s log: --agent must be claude or codex, got %q\n", workflow, *agentF)
			return 2
		}

		root, err := findRoot(stderr)
		if err != nil {
			return 1
		}
		if err := requireProject(root, projectID); err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		wf, err := LookupWorkflow(workflow)
		if err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		if runID == latestRunSentinel {
			resolved, err := pickLatestRun(root, workflow, projectID)
			if err != nil {
				moePrintf(stderr, "moe %s log: %v\n", workflow, err)
				return 1
			}
			runID = resolved
		}
		md, err := run.Load(root, projectID, runID)
		if err != nil {
			if errors.Is(err, run.ErrRunNotFound) {
				moePrintf(stderr, "moe %s log: %s %s does not exist\n", workflow, projectID, runID)
				return 1
			}
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		if md.Workflow != workflow {
			moePrintf(stderr, "moe %s log: %s is a %s run, use 'moe %s log'\n", workflow, runID, md.Workflow, md.Workflow)
			return 1
		}
		if !stageRegistered(wf.Stages(), stage) {
			moePrintf(stderr, "moe %s log: no such stage: %s (have: %v)\n", workflow, stage, wf.Stages())
			return 1
		}

		threadPath, agentName, err := pickLogThread(root, projectID, runID, stage, *agentF)
		if err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		if threadPath == "" {
			moePrintf(stderr, "moe %s log: no transcript for stage %q in run %s/%s\n", workflow, stage, projectID, runID)
			return 1
		}

		f, err := os.Open(threadPath)
		if err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		defer f.Close()
		events, err := transcript.Parse(agentName, f)
		if err != nil {
			moePrintf(stderr, "moe %s log: %v\n", workflow, err)
			return 1
		}
		if len(events) == 0 {
			moePrintf(stderr, "moe %s log: %s contains no rendered events\n", workflow, threadPath)
			return 0
		}
		if err := transcript.Render(stdout, events, transcript.RenderOptions{}); err != nil {
			moePrintf(stderr, "moe %s log: render: %v\n", workflow, err)
			return 1
		}
		return 0
	}
}

// pickLogThread chooses the (path, agent) tuple to render for a stage
// the operator named. When agentArg is set, the named thread file must
// exist on disk. When unset, the stage must have exactly one of
// thread-claude.jsonl / thread-codex.jsonl: both present is a refusal
// (disambiguation is the operator's call), neither is the empty case
// the caller surfaces with a friendly message.
func pickLogThread(root, projectID, runID, stage, agentArg string) (string, string, error) {
	if agentArg != "" {
		abs := filepath.Join(root, run.ThreadPathFor(agentArg, projectID, runID, stage))
		if _, err := os.Stat(abs); err != nil {
			if os.IsNotExist(err) {
				return "", "", nil
			}
			return "", "", fmt.Errorf("stat %s: %w", abs, err)
		}
		return abs, agentArg, nil
	}
	type hit struct{ path, agent string }
	var found []hit
	for _, agent := range []string{"claude", "codex"} {
		abs := filepath.Join(root, run.ThreadPathFor(agent, projectID, runID, stage))
		if _, err := os.Stat(abs); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", "", fmt.Errorf("stat %s: %w", abs, err)
		}
		found = append(found, hit{abs, agent})
	}
	if len(found) == 0 {
		return "", "", nil
	}
	if len(found) > 1 {
		return "", "", fmt.Errorf("stage %q has both claude and codex transcripts; pass --agent to pick one", stage)
	}
	return found[0].path, found[0].agent, nil
}

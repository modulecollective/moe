// log.go — `moe log <project> <run> <stage>` renders an agent
// transcript as plain text. The transcript files (thread-claude.jsonl,
// thread-codex.jsonl) already live alongside the canvas — this command
// just turns them into something the operator can scroll. Same renderer
// as the post-headless auto-tail (internal/transcript), no `--tail` cap.
//
// All three positionals are required — no auto-pick, no fallback. The
// operator who wants "what's in play right now" composes with
// `moe follow`, which already owns that resolver:
//
//	eval "$(moe follow --shell)" && \
//	  moe log "$MOE_FOLLOW_PROJECT" "$MOE_FOLLOW_RUN" "$MOE_FOLLOW_STAGE"
//
// A stage with both thread-claude.jsonl and thread-codex.jsonl requires
// `--agent` to disambiguate; a stage with one file picks it implicitly.
// Operator pipes to `less` for paging — no built-in pager keeps the
// command predictable in scripts.
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

func init() {
	Register(&Command{
		Name:    "log",
		Summary: "render an agent transcript for a stage",
		Run:     runLog,
	})
}

func runLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentF := fs.String("agent", "", "render claude or codex (required when the stage has both)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe log <project> <run> <stage> [--agent claude|codex]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Renders the agent's per-session JSONL transcript for a stage as plain")
		moePrintln(stderr, "text. Pipe to less for paging.")
		moePrintln(stderr, "")
		moePrintln(stderr, "To render whatever run/stage is currently in play, compose with")
		moePrintln(stderr, "`moe follow --shell`:")
		moePrintln(stderr, "")
		moePrintln(stderr, `    eval "$(moe follow --shell)" && \`)
		moePrintln(stderr, `      moe log "$MOE_FOLLOW_PROJECT" "$MOE_FOLLOW_RUN" "$MOE_FOLLOW_STAGE"`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)
	runID := fs.Arg(1)
	stage := fs.Arg(2)

	switch *agentF {
	case "", "claude", "codex":
	default:
		moePrintf(stderr, "moe log: --agent must be claude or codex, got %q\n", *agentF)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := run.Load(root, projectID, runID); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "moe log: %s %s does not exist\n", projectID, runID)
			return 1
		}
		moePrintf(stderr, "moe log: %v\n", err)
		return 1
	}

	threadPath, agentName, err := pickLogThread(root, projectID, runID, stage, *agentF)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if threadPath == "" {
		moePrintf(stderr, "moe log: no transcript for stage %q in run %s/%s\n", stage, projectID, runID)
		return 1
	}

	f, err := os.Open(threadPath)
	if err != nil {
		moePrintf(stderr, "moe log: %v\n", err)
		return 1
	}
	defer f.Close()
	events, err := transcript.Parse(agentName, f)
	if err != nil {
		moePrintf(stderr, "moe log: %v\n", err)
		return 1
	}
	if len(events) == 0 {
		moePrintf(stderr, "moe log: %s contains no rendered events\n", threadPath)
		return 0
	}
	if err := transcript.Render(stdout, events, transcript.RenderOptions{}); err != nil {
		moePrintf(stderr, "moe log: render: %v\n", err)
		return 1
	}
	return 0
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
			return "", "", fmt.Errorf("moe log: stat %s: %w", abs, err)
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
			return "", "", fmt.Errorf("moe log: stat %s: %w", abs, err)
		}
		found = append(found, hit{abs, agent})
	}
	if len(found) == 0 {
		return "", "", nil
	}
	if len(found) > 1 {
		return "", "", fmt.Errorf("moe log: stage %q has both claude and codex transcripts; pass --agent to pick one", stage)
	}
	return found[0].path, found[0].agent, nil
}

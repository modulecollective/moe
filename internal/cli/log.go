// log.go — `moe log [<stage>]` renders an agent transcript as plain
// text. The transcript files (thread-claude.jsonl, thread-codex.jsonl)
// already live alongside the canvas — this command just turns them
// into something the operator can scroll. Same renderer as the
// post-headless auto-tail (internal/transcript), no `--tail` cap.
//
// Defaults:
//   - run:   the most recent in-progress run (matching `moe follow`'s
//     picker; --run / --project pin explicitly)
//   - stage: the stage whose thread file was modified most recently
//   - agent: the most-recently-modified thread-<agent>.jsonl for that
//     stage when a stage has more than one
//
// Operator pipes to `less` for paging — no built-in pager keeps the
// command predictable in scripts.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

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
	projectF := fs.String("project", "", "restrict to runs in this project")
	runF := fs.String("run", "", "render this run (default: most recent in-progress)")
	agentF := fs.String("agent", "", "force claude or codex (default: most-recently-modified thread file for the stage)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe log [--project <id>] [--run <id>] [--agent claude|codex] [<stage>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Renders the agent's per-session JSONL transcript for a stage as plain")
		moePrintln(stderr, "text. With no stage arg, picks the stage whose thread-*.jsonl was")
		moePrintln(stderr, "most recently modified. Pipe to less for paging.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	var stageArg string
	if fs.NArg() == 1 {
		stageArg = fs.Arg(0)
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	md, err := pickLogRun(root, *projectF, *runF)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if md == nil {
		moePrintln(stderr, "moe log: no run found (try --run or --project)")
		return 1
	}

	threadPath, agentName, err := pickLogThread(root, md, stageArg, *agentF)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if threadPath == "" {
		if stageArg != "" {
			moePrintf(stderr, "moe log: no transcript for stage %q in run %s\n", stageArg, md.ID)
		} else {
			moePrintf(stderr, "moe log: no transcripts under run %s\n", md.ID)
		}
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

// pickLogRun resolves which run to render. --run wins outright (with
// optional --project narrowing); otherwise the most-recent in-progress
// run by journal activity. Returns (nil, nil) when nothing matches —
// caller renders the friendly empty message.
func pickLogRun(root, projectFilter, runFilter string) (*run.Metadata, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	if runFilter != "" {
		for _, md := range mds {
			if md.ID != runFilter {
				continue
			}
			if projectFilter != "" && md.Project != projectFilter {
				continue
			}
			return md, nil
		}
		return nil, nil
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, err
	}
	type cand struct {
		md   *run.Metadata
		when time.Time
	}
	var cands []cand
	for _, md := range mds {
		if md.Status != run.StatusInProgress {
			continue
		}
		if projectFilter != "" && md.Project != projectFilter {
			continue
		}
		cands = append(cands, cand{md: md, when: idx.LastActivity[md.ID]})
	}
	if len(cands) == 0 {
		return nil, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].when.After(cands[j].when)
	})
	return cands[0].md, nil
}

// pickLogThread chooses the (path, agent) tuple to render. stageArg
// narrows to a single stage's documents/ dir; empty means look under
// every documents/<stage>/. agentArg pins claude or codex; empty
// picks whichever thread-*.jsonl was modified most recently. Returns
// ("", "", nil) when no candidates exist.
func pickLogThread(root string, md *run.Metadata, stageArg, agentArg string) (string, string, error) {
	docsRoot := filepath.Join(root, run.Dir(md.Project, md.ID), "documents")
	entries, err := os.ReadDir(docsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("moe log: read documents dir: %w", err)
	}
	type cand struct {
		path  string
		agent string
		mtime time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if stageArg != "" && e.Name() != stageArg {
			continue
		}
		for _, agent := range []string{"claude", "codex"} {
			if agentArg != "" && agent != agentArg {
				continue
			}
			p := filepath.Join(docsRoot, e.Name(), "thread-"+agent+".jsonl")
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			cands = append(cands, cand{path: p, agent: agent, mtime: info.ModTime()})
		}
	}
	if len(cands) == 0 {
		return "", "", nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].mtime.After(cands[j].mtime)
	})
	return cands[0].path, cands[0].agent, nil
}

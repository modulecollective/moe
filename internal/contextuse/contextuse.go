// Package contextuse answers "does the steering a bureaucracy writes
// actually reach its agents" from the mirrored stage transcripts alone:
// which sessions read the twin, the lore catalog's entries, the knowledge
// index and topics, the open intents and other runs' artifacts, what
// they got back (a truncated result, a not-found error), and which of
// those files nobody opens at all.
//
// Read-side only, same posture as internal/usage: no new state, no
// logging, compute on read. The method and its limits are the moe
// knowledge topic auditing-context-use-from-transcripts.md; this package
// is that topic's scanner made permanent so a 90-day chore can compare
// like with like.
//
// A mention counts as a read only when the command segment carrying the
// path starts with a read or search verb; a path in prose, a patch or a
// listing does not. Claude calls pair with their results by id. Codex
// scripts run several commands per call and the per-command pairing is
// not reliable (commands sit in arrays, promises reject, the script cap
// eats the middle), so a Codex read is judged by its call: truncated if
// the call's result carries the cap warning, missed if an error line in
// it names the path as written.
package contextuse

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/run"
)

// PulseWorkflow is reported apart from every other workflow: its
// fragment mandates prior-run and intent reads, so folding it into the
// stage rates would flatter them.
const PulseWorkflow = "pulse"

// CodexResultCapTokens is the whole-script output cap observed on Codex
// in 2026-11 (the stage prompt's twin section quotes the same figure).
// A twin doc larger than this can't arrive whole in one read.
const CodexResultCapTokens = 10_000

// Filter narrows a scan the way usage.Filter does.
type Filter struct {
	// Project limits the scan to one project's runs; empty scans all.
	Project string
	// Cutoff drops stages whose last work turn predates it. Undated
	// stages are kept, as in `moe usage`.
	Cutoff time.Time
}

// Session is one mirrored stage transcript and the context it reached.
// Every list holds distinct keys, sorted.
type Session struct {
	Project  string    `json:"project"`
	Run      string    `json:"run"`
	Workflow string    `json:"workflow"`
	Stage    string    `json:"stage"`
	Agent    string    `json:"agent"`
	When     time.Time `json:"when,omitzero"`

	// Skill is a load of the moe-context skill: a Skill call on Claude,
	// a read of its SKILL.md on Codex.
	Skill bool `json:"skill"`
	// Index is a successful read of any project's knowledge/index.md.
	Index bool `json:"index"`
	// Topics are "project/topic.md"; Twin "project/doc.md", with "*"
	// standing for a read that named the directory or a glob.
	Topics    []string `json:"topics,omitempty"`
	Lore      []string `json:"lore,omitempty"`
	Twin      []string `json:"twin,omitempty"`
	Intents   []string `json:"intents,omitempty"`
	PriorRuns []string `json:"prior_runs,omitempty"`

	// Reads counts context reads whose result came back; Truncated the
	// ones among them that arrived cut; Unpaired the ones with no result
	// at all (counted as unknown, not failed).
	Reads     int `json:"reads"`
	Truncated int `json:"truncated"`
	Unpaired  int `json:"unpaired"`
	// Misses are paths whose read failed with a not-found error naming
	// the path.
	Misses []string `json:"misses,omitempty"`
}

// Pulse reports whether s is a pulse session.
func (s Session) Pulse() bool { return s.Workflow == PulseWorkflow }

// DeepRead reports whether s read any context beyond its own run.
func (s Session) DeepRead() bool {
	return s.Index || len(s.Topics) > 0 || len(s.Lore) > 0 || len(s.Twin) > 0 ||
		len(s.Intents) > 0 || len(s.PriorRuns) > 0
}

// Gather walks every run's mirrored transcripts under root through f
// and returns one Session per transcript, oldest first. A stage's time
// is its last work turn, falling back to the run's last activity —
// mtimes are checkout times in a session worktree, so the journal is
// the only honest clock (the same rule usage.scan records). Transcripts
// are read disk first, then from the index for paths a sparse session
// leaves out. warn gets one line per transcript that fails to parse;
// what was read before the failure still counts.
func Gather(root string, f Filter, warn io.Writer) ([]Session, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, fmt.Errorf("build journal index: %w", err)
	}
	artifacts := run.NewArtifactReader(root)
	var out []Session
	for _, md := range mds {
		if f.Project != "" && md.Project != f.Project {
			continue
		}
		stages, err := artifacts.DocumentIDs(md.Project, md.ID)
		if err != nil {
			return nil, err
		}
		for _, stage := range stages {
			when := idx.WorkTurnTime[run.WorkTurnKey{Project: md.Project, Run: md.ID, Doc: stage}]
			if when.IsZero() {
				when = idx.LastActivity[md.Project+"/"+md.ID]
			}
			if !when.IsZero() && when.Before(f.Cutoff) {
				continue
			}
			for _, agent := range []string{"claude", "codex"} {
				path := run.ThreadPathFor(agent, md.Project, md.ID, stage)
				rc, err := artifacts.Open(path)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				s := Session{Project: md.Project, Run: md.ID, Workflow: md.Workflow, Stage: stage, Agent: agent, When: when}
				if err := parseTranscript(&s, agent, rc); err != nil {
					cliout.Printf(warn, "moe context-use: %s: %v\n", path, err)
				}
				rc.Close()
				out = append(out, s)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].When.Before(out[j].When) })
	return out, nil
}

// Inventory is what a project's steering offers, read from disk, so the
// report can name what nobody opened. Lore is bureaucracy-wide.
type Inventory struct {
	Lore []string
	// Knowledge maps project → topic file names; IndexLines is 0 when
	// the project has no knowledge index.
	Knowledge  map[string][]string
	IndexLines map[string]int
	// Twin maps project → its docs with sizes.
	Twin map[string][]Doc
}

// Doc is one twin doc's size on disk.
type Doc struct {
	Name  string
	Lines int
	Bytes int
}

// Tokens is the usual four-bytes-per-token estimate — good enough to say
// whether a doc fits under a result cap, which is all it is used for.
func (d Doc) Tokens() int { return d.Bytes / 4 }

// ReadInventory lists the lore entries, and each named project's
// knowledge topics and twin docs, as they stand on disk under root.
func ReadInventory(root string, projects []string) Inventory {
	inv := Inventory{Knowledge: map[string][]string{}, IndexLines: map[string]int{}, Twin: map[string][]Doc{}}
	for _, name := range mdFiles(filepath.Join(root, "lore")) {
		inv.Lore = append(inv.Lore, strings.TrimSuffix(name, ".md"))
	}
	for _, p := range projects {
		pdir := filepath.Join(root, "projects", p)
		inv.Knowledge[p] = mdFiles(filepath.Join(pdir, "knowledge", "topics"))
		if data, err := os.ReadFile(filepath.Join(pdir, "knowledge", "index.md")); err == nil {
			inv.IndexLines[p] = strings.Count(string(data), "\n")
		}
		for _, name := range mdFiles(filepath.Join(pdir, "digital-twin")) {
			data, err := os.ReadFile(filepath.Join(pdir, "digital-twin", name))
			if err != nil {
				continue
			}
			inv.Twin[p] = append(inv.Twin[p], Doc{Name: name, Lines: strings.Count(string(data), "\n"), Bytes: len(data)})
		}
	}
	return inv
}

// mdFiles is the sorted *.md file names directly under dir, nil when
// dir is missing.
func mdFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	return out
}

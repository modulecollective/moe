// Package cli — chain verbs.
//
// `moe chain edit` opens a rebase-style editor over every active sdlc
// run across every project. Saving the file produces a linear chain:
// line i chains-to line i+1. Per Decision 4, the file is authoritative
// for parents in it — any chain-to edge whose parent appears in the
// file is dropped and replaced by the file's ordering; edges whose
// parent isn't in the file are untouched.
//
// `moe chain clear` drops every currently-live chain edge in one
// commit. Confirmation prompt by default; --yes skips.
//
// Both verbs emit MoE-Chained-To / MoE-Chained-To-Removed trailers
// on bureaucracy commits that carry no MoE-Run trailer — one chain
// edit touches several parents, no single canonical run to scope it
// to. BuildJournalIndex's grep widens to pick these up.
package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

func init() {
	g := NewCommandGroup("chain", "manage run chains: edit, clear")
	g.Register(&Command{
		Name:    "edit",
		Summary: "rebase-style editor over active sdlc runs; reorder to chain",
		Run:     runChainEdit,
	})
	g.Register(&Command{
		Name:    "clear",
		Summary: "drop every currently-live chain edge",
		Run:     runChainClear,
	})
	RegisterGroup(g)
}

// chainItem is one line in the editor view: the qualified-slug key the
// operator reorders, plus the annotation that follows it as a comment.
// The annotation is informational only — only the leading key is
// parsed back on save.
type chainItem struct {
	Key  string
	When time.Time
	Anno string
}

func runChainEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain edit")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens $EDITOR on a list of every active sdlc run across every")
		moePrintln(stderr, "project, annotated by chain state. Delete lines you don't want,")
		moePrintln(stderr, "reorder the rest, and save — the remaining lines form a linear")
		moePrintln(stderr, "chain in order (each chains-to the next). Edges whose parent is")
		moePrintln(stderr, "absent from the file are untouched.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "chain edit: %v\n", err)
		return 1
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "chain edit: %v\n", err)
		return 1
	}

	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	items := activeSDLCChainItems(mds, idx, byKey)
	if len(items) == 0 {
		moePrintln(stderr, "chain edit: no active sdlc runs")
		return 0
	}

	tmp, err := os.CreateTemp("", "moe-chain-edit-*.txt")
	if err != nil {
		moePrintf(stderr, "chain edit: create temp: %v\n", err)
		return 1
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(renderChainEditFile(items)); err != nil {
		tmp.Close()
		moePrintf(stderr, "chain edit: write temp: %v\n", err)
		return 1
	}
	if err := tmp.Close(); err != nil {
		moePrintf(stderr, "chain edit: close temp: %v\n", err)
		return 1
	}

	if err := launchEditorOrFail(tmpPath); err != nil {
		moePrintf(stderr, "chain edit: %v\n", err)
		return 1
	}

	raw, err := os.ReadFile(tmpPath)
	if err != nil {
		moePrintf(stderr, "chain edit: read temp: %v\n", err)
		return 1
	}

	desired, err := parseChainEditFile(string(raw))
	if err != nil {
		moePrintf(stderr, "chain edit: %v\n", err)
		return 1
	}

	activeKeys := make(map[string]bool, len(items))
	for _, it := range items {
		activeKeys[it.Key] = true
	}
	for _, k := range desired {
		if !activeKeys[k] {
			moePrintf(stderr, "chain edit: %q is not an active sdlc run\n", k)
			return 1
		}
	}

	adds, removes := diffChainEdit(desired, idx.ChainedChild)
	if len(adds) == 0 && len(removes) == 0 {
		moePrintln(stdout, "chain edit: no changes")
		return 0
	}

	block := trailers.Block{
		ChainedTo:        adds,
		ChainedToRemoved: removes,
	}
	subject := fmt.Sprintf("chain: edit (%d added, %d removed)", len(adds), len(removes))
	msg := subject + "\n\n" + block.String()

	err = repolock.With(root, repolock.Options{Purpose: "chain-edit"}, func() error {
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "chain edit: %v\n", err)
		return 1
	}

	moePrintf(stdout, "chain edit: %d added, %d removed\n", len(adds), len(removes))
	return 0
}

func runChainClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain clear [--yes]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Drops every currently-live chain edge in one commit. The trailers")
		moePrintln(stderr, "stay in history; clearing only resets the live set so the operator")
		moePrintln(stderr, "can rebuild from a blank slate with `moe chain edit`.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "chain clear: %v\n", err)
		return 1
	}

	var removes []string
	for parent, child := range idx.ChainedChild {
		if child == "" {
			continue
		}
		removes = append(removes, parent+" "+child)
	}
	if len(removes) == 0 {
		moePrintln(stdout, "chain clear: no live edges")
		return 0
	}
	sort.Strings(removes)

	if !*yes {
		moePrintf(stdout, "chain clear: drop %d live edge(s)?\n", len(removes))
		for _, e := range removes {
			moePrintf(stdout, "  %s\n", e)
		}
		moePrint(stdout, "confirm? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			moePrintf(stderr, "chain clear: read stdin: %v\n", err)
			return 1
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(answer, "y") {
			moePrintln(stdout, "chain clear: aborted")
			return 0
		}
	}

	block := trailers.Block{ChainedToRemoved: removes}
	subject := fmt.Sprintf("chain: clear (%d removed)", len(removes))
	msg := subject + "\n\n" + block.String()

	err = repolock.With(root, repolock.Options{Purpose: "chain-clear"}, func() error {
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "chain clear: %v\n", err)
		return 1
	}

	moePrintf(stdout, "chain clear: %d removed\n", len(removes))
	return 0
}

// activeSDLCChainItems gathers the active sdlc runs and annotates each
// with its current chain state. Sort order is newest-activity-first so
// the editor opens with recent work at the top — the operator's
// strongest mental anchor when sequencing a backlog.
func activeSDLCChainItems(mds []*run.Metadata, idx *run.JournalIndex, byKey map[string]*run.Metadata) []chainItem {
	chainedFrom := invertEffectiveChain(idx.ChainedChild, byKey)
	out := []chainItem{}
	for _, md := range mds {
		if md.Workflow != "sdlc" || md.Status != run.StatusInProgress {
			continue
		}
		key := md.Project + "/" + md.ID
		out = append(out, chainItem{
			Key:  key,
			When: idx.LastActivity[md.ID],
			Anno: chainAnnotation(key, idx.ChainedChild, chainedFrom, byKey),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].When.Equal(out[j].When) {
			return out[i].Key < out[j].Key
		}
		return out[i].When.After(out[j].When)
	})
	return out
}

// invertEffectiveChain maps child key → list of parent keys for every
// live, unresolved edge. "Effective" filters out edges whose child is
// terminal or no longer on disk, matching Decision 1's read-side rule.
// A child can have multiple parents (cross-parent fan-in is allowed)
// so values are lists, sorted for deterministic output.
func invertEffectiveChain(chainedChild map[string]string, byKey map[string]*run.Metadata) map[string][]string {
	out := map[string][]string{}
	for parent, child := range chainedChild {
		if !chainChildIsLive(child, byKey) {
			continue
		}
		out[child] = append(out[child], parent)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// chainChildIsLive applies Decision 1: a child is "live" only if it
// exists on disk and is not terminal. Empty or missing-from-byKey
// counts as not live.
func chainChildIsLive(childKey string, byKey map[string]*run.Metadata) bool {
	if childKey == "" {
		return false
	}
	md, ok := byKey[childKey]
	if !ok {
		return false
	}
	switch md.Status {
	case run.StatusClosed, run.StatusMerged, run.StatusPromoted, run.StatusPushed:
		return false
	}
	return true
}

// chainAnnotation builds the per-line comment for the editor view.
// One of: "orphan", "chains-to X", "chained-from Y[, Z]",
// "chains-to X, chained-from Y". Cross-references that point at
// terminal or missing runs are suppressed — the editor should not
// advertise stale state.
func chainAnnotation(key string, chainedChild map[string]string, chainedFrom map[string][]string, byKey map[string]*run.Metadata) string {
	var parts []string
	if child, ok := chainedChild[key]; ok && chainChildIsLive(child, byKey) {
		parts = append(parts, "chains-to "+child)
	}
	if parents := chainedFrom[key]; len(parents) > 0 {
		parts = append(parts, "chained-from "+strings.Join(parents, ", "))
	}
	if len(parts) == 0 {
		return "orphan"
	}
	return strings.Join(parts, ", ")
}

// renderChainEditFile produces the editor file body: a few lines of
// instructions then one line per item, key padded to a column so the
// `# annotation` suffixes line up.
func renderChainEditFile(items []chainItem) string {
	width := 0
	for _, it := range items {
		if len(it.Key) > width {
			width = len(it.Key)
		}
	}
	var sb strings.Builder
	sb.WriteString("# moe chain edit\n")
	sb.WriteString("#\n")
	sb.WriteString("# Reorder lines to chain runs head-first. The remaining lines\n")
	sb.WriteString("# become a linear chain: each chains-to the one below it.\n")
	sb.WriteString("# Delete lines to leave them unchained. Lines starting with #\n")
	sb.WriteString("# are ignored. Only the leading <project>/<slug> token is parsed.\n")
	sb.WriteString("#\n")
	sb.WriteString("# Save an empty file (or one with no run lines) for a no-op.\n")
	sb.WriteString("#\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "%-*s  # %s\n", width, it.Key, it.Anno)
	}
	return sb.String()
}

// parseChainEditFile extracts the ordered list of qualified slugs the
// editor file specifies. Lines that are blank or start with `#` are
// skipped; for every other line the first whitespace-separated token
// is taken as the slug. Returns an error on a malformed slug (not
// `<project>/<run>` shape) or a duplicate.
func parseChainEditFile(body string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for lineNo, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		key := fields[0]
		if _, _, err := splitProjectRun(key); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		if seen[key] {
			return nil, fmt.Errorf("line %d: %q appears more than once", lineNo+1, key)
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}

// diffChainEdit computes the trailer batches for one `chain edit`
// save. desired is the ordered list of slugs from the saved file —
// each slug at position i chains-to position i+1; the last slug has
// no successor. live is the journal index's current chain map.
//
// Per Decision 4, parents IN the file are authoritative — their
// desired live child replaces whatever the index says (including
// clearing to no child if the parent is the file's last line).
// Parents NOT in the file are untouched.
//
// Each emitted trailer value is "<parent> <child>". Outputs are
// sorted by parent for deterministic commit bodies and tests.
func diffChainEdit(desired []string, live map[string]string) (adds, removes []string) {
	want := make(map[string]string, len(desired))
	for i, k := range desired {
		if i+1 < len(desired) {
			want[k] = desired[i+1]
		} else {
			want[k] = ""
		}
	}
	parents := make([]string, 0, len(want))
	for p := range want {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		d := want[parent]
		c := live[parent]
		if d == c {
			continue
		}
		if c != "" {
			removes = append(removes, parent+" "+c)
		}
		if d != "" {
			adds = append(adds, parent+" "+d)
		}
	}
	return adds, removes
}

// Package cli — chain verbs.
//
// `moe chain edit` opens a rebase-style editor over every active sdlc
// run across every project, grouped into blocks that mirror the dash's
// chains. A blank line is a chain boundary: each contiguous block of
// run lines becomes one linear chain (line i chains-to line i+1 within
// its block; the block's last line chains-to nothing). The editor is
// WYSIWYG — the blocks you see are the chains you get. The offered runs
// are authoritative: a run's saved position sets its outgoing edge, and
// a run the editor showed but the operator deleted has its edge cleared
// (delete unchains, same as isolating a run in its own block). Runs the
// editor never showed — terminal or suppressed parents — keep their
// edges, so opening the editor and saving unchanged is a no-op for any
// fan-in-free state.
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
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

func init() {
	g := NewCommandGroup("chain", "manage run chains")
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
		moePrintln(stderr, "Opens $EDITOR on every active sdlc run across every project,")
		moePrintln(stderr, "grouped into blocks that mirror the dash's chains. A blank line")
		moePrintln(stderr, "separates chains: each block of run lines becomes one linear chain")
		moePrintln(stderr, "(each chains-to the one below it within the block). Move a line into")
		moePrintln(stderr, "another block to fold it in, or isolate it in its own block")
		moePrintln(stderr, "(or delete it) to unchain it.")
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

	activeKeys := map[string]bool{}
	for _, block := range items {
		for _, it := range block {
			activeKeys[it.Key] = true
		}
	}
	for _, block := range desired {
		for _, k := range block {
			if !activeKeys[k] {
				moePrintf(stderr, "chain edit: %q is not an active sdlc run\n", k)
				return 1
			}
		}
	}

	adds, removes := diffChainEdit(desired, activeKeys, idx.ChainedChild)
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

	err = sync.WithJournalPush(root, repolock.Options{Purpose: "chain-edit"}, stdout, stderr, func() error {
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

	err = sync.WithJournalPush(root, repolock.Options{Purpose: "chain-clear"}, stdout, stderr, func() error {
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
// with its current chain state. Render order matches the dash's ACTIVE
// section — chains render as contiguous head→tail blocks, each block (or
// standalone run) floating by its most-recent member — so the operator
// reads the editor the way they read the dash and can fold a run into an
// existing chain instead of rebuilding from scratch. The grouping is
// shared with the dash via run.OrderChainUnits.
//
// Each returned block is one chain unit (an orphan, or a head→tail
// chain); renderChainEditFile emits a blank line between blocks so the
// editor's blank lines are the chain boundaries.
func activeSDLCChainItems(mds []*run.Metadata, idx *run.JournalIndex, byKey map[string]*run.Metadata) [][]chainItem {
	chainedFrom := invertEffectiveChain(idx.ChainedChild, byKey)
	itemByKey := map[string]chainItem{}
	for _, md := range mds {
		if md.Workflow != "sdlc" || md.Status != run.StatusInProgress {
			continue
		}
		key := md.Project + "/" + md.ID
		itemByKey[key] = chainItem{
			Key:  key,
			When: idx.LastActivity[key],
			Anno: chainAnnotation(key, idx.ChainedChild, chainedFrom, byKey),
		}
	}
	// Feed the shared grouper in newest-first order (Key tiebreak keeps
	// equal-activity runs deterministic); it returns the head→tail units
	// in dash order, one block per unit.
	order := make([]run.ChainOrderItem, 0, len(itemByKey))
	for _, it := range itemByKey {
		order = append(order, run.ChainOrderItem{Key: it.Key, When: it.When})
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].When.Equal(order[j].When) {
			return order[i].Key < order[j].Key
		}
		return order[i].When.After(order[j].When)
	})
	units := run.OrderChainUnits(order, idx, byKey)
	out := make([][]chainItem, 0, len(units))
	for _, u := range units {
		block := make([]chainItem, 0, len(u))
		for _, k := range u {
			block = append(block, itemByKey[k])
		}
		out = append(out, block)
	}
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
		if !run.ChainChildLive(child, byKey) {
			continue
		}
		out[child] = append(out[child], parent)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// chainAnnotation builds the per-line comment for the editor view.
// One of: "orphan", "chains-to X", "chained-from Y[, Z]",
// "chains-to X, chained-from Y". Cross-references that point at
// terminal or missing runs are suppressed — the editor should not
// advertise stale state.
func chainAnnotation(key string, chainedChild map[string]string, chainedFrom map[string][]string, byKey map[string]*run.Metadata) string {
	var parts []string
	if child, ok := chainedChild[key]; ok && run.ChainChildLive(child, byKey) {
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
// instructions then the blocks, one line per item with the key padded
// to a column so the `# annotation` suffixes line up. A blank line
// separates blocks, so the file's blank lines are the chain boundaries
// the parser reads back.
func renderChainEditFile(blocks [][]chainItem) string {
	width := 0
	for _, block := range blocks {
		for _, it := range block {
			if len(it.Key) > width {
				width = len(it.Key)
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("# moe chain edit\n")
	sb.WriteString("#\n")
	sb.WriteString("# A blank line separates chains. Each block of run lines below\n")
	sb.WriteString("# becomes one linear chain: each line chains-to the one below it\n")
	sb.WriteString("# within its block; the block's last line chains-to nothing.\n")
	sb.WriteString("#\n")
	sb.WriteString("# Move a line into another block to fold it into that chain, or\n")
	sb.WriteString("# isolate it (blank lines around it, or delete it) to unchain it.\n")
	sb.WriteString("# Lines starting with # are ignored; only the leading\n")
	sb.WriteString("# <project>/<slug> token is parsed.\n")
	sb.WriteString("#\n")
	sb.WriteString("# Save unchanged for a no-op.\n")
	sb.WriteString("#\n")
	for i, block := range blocks {
		if i > 0 {
			sb.WriteString("\n")
		}
		for _, it := range block {
			fmt.Fprintf(&sb, "%-*s  # %s\n", width, it.Key, it.Anno)
		}
	}
	return sb.String()
}

// parseChainEditFile splits the editor file into blocks of qualified
// slugs — one block per contiguous run of non-blank run lines. A blank
// line flushes the current block (it is a chain boundary); lines that
// start with `#` are transparent (skipped without breaking a block);
// for every run line the first whitespace-separated token is taken as
// the slug. Empty blocks (e.g. the all-comment header) are dropped.
// Returns an error on a malformed slug (not `<project>/<run>` shape) or
// a duplicate. The duplicate check is global: a run can't appear in two
// blocks.
func parseChainEditFile(body string) ([][]string, error) {
	var blocks [][]string
	var cur []string
	seen := map[string]bool{}
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	for lineNo, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "#") {
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
		cur = append(cur, key)
	}
	flush()
	return blocks, nil
}

// diffChainEdit computes the trailer batches for one `chain edit`
// save. blocks are the saved file's chains — within each block, the
// slug at position i chains-to position i+1 and the last slug has no
// successor. Blocks don't chain into one another: a block boundary is a
// chain boundary. offered is the set of runs the editor showed the
// operator (active, non-suppressed); live is the journal index's
// current chain map.
//
// Authoritativeness is scoped to the offered set. A run that appears in
// the file gets the outgoing edge its saved position implies (including
// no edge if it is the last line of its block). A run the editor
// offered but the operator deleted from the file gets its edge cleared
// too — delete unchains, the same as isolating a run in its own block.
// A parent the editor never showed (terminal or suppressed) keeps
// whatever edge the index holds, so a save can never clear an edge the
// operator never saw.
//
// Each emitted trailer value is "<parent> <child>". Outputs are
// sorted by parent for deterministic commit bodies and tests.
func diffChainEdit(blocks [][]string, offered map[string]bool, live map[string]string) (adds, removes []string) {
	want := map[string]string{}
	for _, block := range blocks {
		for i, k := range block {
			if i+1 < len(block) {
				want[k] = block[i+1]
			} else {
				want[k] = ""
			}
		}
	}
	// An offered run absent from the file was deleted: the operator
	// wants it unchained, not left alone. Give it an empty desired edge
	// so a live edge diffs to a clear.
	for k := range offered {
		if _, ok := want[k]; !ok {
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

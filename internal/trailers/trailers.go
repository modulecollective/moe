// Package trailers renders the canonical MoE-* commit-trailer block.
//
// The bureaucracy is branchless: per-run scoping is reconstructed by
// grepping commit trailers (MoE-Run, MoE-Document, MoE-Session, …)
// out of git history. Every harness commit that wants to be findable
// carries a block of these. Block centralises the names, the order,
// and the rendering, so a new emit site cannot silently drift on
// capitalisation or sequence.
//
// Field declaration order matches render order — readers see the
// canonical sequence in one place, callers fill what applies and
// leave the rest zero. Empty fields elide.
package trailers

import "strings"

// Block is the typed shape of the MoE-* trailer block. Each field
// maps to one trailer; zero-value fields elide. String() renders the
// non-empty fields in canonical order, one per line, each terminated
// with '\n', so `subject + "\n\n" + block.String()` yields a commit
// message ending in '\n'.
type Block struct {
	Run           string
	Project       string
	Workflow      string
	Document      string
	Session       string
	PR            string
	Merged        string
	Closed        string
	PromotedTo    string
	FromRun       string
	Idea          string
	IdeaMovedFrom string
	ReopenOf      string
	Chore         string
	// ChoreSkipped carries "<project>/<chore>" on the empty commit
	// written by `moe chore skip`. Its commit time records that the
	// chore is satisfied as of the skip, folding into the value the
	// due reasons compare against exactly as a completed run would.
	ChoreSkipped string
	// ChoreTouched repeats once per chore whose trigger matched the
	// target-repo change landed by this terminal transition. Values are
	// "<project>/<chore>".
	ChoreTouched []string
	// ChainedTo and ChainedToRemoved each repeat once per edge, one
	// trailer line per slice entry, value
	// "<parent-project>/<parent-slug> <child-project>/<child-slug>".
	// A `chain edit` save commit stamps one ChainedTo per new edge
	// plus one ChainedToRemoved per replaced edge; a `chain clear`
	// stamps one ChainedToRemoved per currently-live edge. Empty
	// slices elide.
	ChainedTo        []string
	ChainedToRemoved []string
}

// String renders the block. Empty fields are skipped; field
// declaration order is the canonical wire order.
func (b Block) String() string {
	var sb strings.Builder
	write(&sb, "MoE-Run", b.Run)
	write(&sb, "MoE-Project", b.Project)
	write(&sb, "MoE-Workflow", b.Workflow)
	write(&sb, "MoE-Document", b.Document)
	write(&sb, "MoE-Session", b.Session)
	write(&sb, "MoE-PR", b.PR)
	write(&sb, "MoE-Merged", b.Merged)
	write(&sb, "MoE-Closed", b.Closed)
	write(&sb, "MoE-Promoted-To", b.PromotedTo)
	write(&sb, "MoE-From-Run", b.FromRun)
	write(&sb, "MoE-Idea", b.Idea)
	write(&sb, "MoE-Idea-Moved-From", b.IdeaMovedFrom)
	write(&sb, "MoE-Reopen-Of", b.ReopenOf)
	write(&sb, "MoE-Chore", b.Chore)
	write(&sb, "MoE-Chore-Skipped", b.ChoreSkipped)
	for _, v := range b.ChoreTouched {
		write(&sb, "MoE-Chore-Touched", v)
	}
	for _, v := range b.ChainedTo {
		write(&sb, "MoE-Chained-To", v)
	}
	for _, v := range b.ChainedToRemoved {
		write(&sb, "MoE-Chained-To-Removed", v)
	}
	return sb.String()
}

func write(sb *strings.Builder, key, val string) {
	if val == "" {
		return
	}
	sb.WriteString(key)
	sb.WriteString(": ")
	sb.WriteString(val)
	sb.WriteByte('\n')
}

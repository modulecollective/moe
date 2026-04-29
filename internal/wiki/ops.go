package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpsStashName is the dotfile under ContentDir the agent appends
// `[wiki-op]` tags to as it applies schema-evolution primitives. The
// engine seeds it at session open and reads + truncates it at finalize.
// excludeManaged keeps it out of the diff that drives log.md so the
// scratchpad never appears in changelog entries.
const OpsStashName = ".wiki-ops"

// OpsStashPath returns the absolute path to the `.wiki-ops` stash file
// given a ContentDir.
func OpsStashPath(contentDir string) string {
	return filepath.Join(contentDir, OpsStashName)
}

// WikiOpKind enumerates the schema-evolution primitives the agent may
// apply during an open-schema ingest. The closed-schema (twin) config
// will refuse all of these — the engine exposes the same vocabulary so
// both modes can talk about the same operations.
type WikiOpKind int

const (
	// OpSplit — one topic doc fanned out into multiple new docs.
	OpSplit WikiOpKind = iota
	// OpMerge — content from one (or more) docs absorbed into another.
	OpMerge
	// OpRename — title/framing shifted; the underlying doc is the
	// same content under a new filename.
	OpRename
	// OpRetire — doc removed because nothing references it any more
	// and its content is either fully absorbed elsewhere or no longer
	// load-bearing.
	OpRetire
)

// String returns the lowercase label used in `[wiki-op]` tags and
// rendered log entries.
func (k WikiOpKind) String() string {
	switch k {
	case OpSplit:
		return "split"
	case OpMerge:
		return "merge"
	case OpRename:
		return "rename"
	case OpRetire:
		return "retire"
	default:
		return "unknown"
	}
}

// WikiOp is one parsed entry from `.wiki-ops`. Sources / Targets are
// the filenames the agent named on either side of the operation;
// retire has a single source and no targets, rename has one of each,
// split has one source and N targets, merge has N sources and one
// target.
type WikiOp struct {
	Kind    WikiOpKind
	Sources []string
	Targets []string
}

// EnsureOpsStash truncates (or creates) the `.wiki-ops` stash file under
// contentDir so the agent starts the session with an empty scratchpad.
// Called at session open. Creates contentDir if it doesn't yet exist —
// a fresh wiki may have no on-disk files at all on its first ingest.
//
// Any I/O failure surfaces; callers should treat a failure here as
// non-fatal and degrade to today's behavior (no operations group in the
// log entry) rather than blocking the session.
func EnsureOpsStash(contentDir string) error {
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return fmt.Errorf("wiki: mkdir %s: %w", contentDir, err)
	}
	path := OpsStashPath(contentDir)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wiki: seed %s: %w", path, err)
	}
	return f.Close()
}

// readAndTruncateOpsStash reads the stash, parses it into ops, and
// truncates the file in one pass. A missing or unreadable stash is not
// an error — it produces an empty op list and the log entry degrades
// to today's shape. The truncation rides along in the per-turn commit
// commitTurn assembles after FinalizeIngest.
func readAndTruncateOpsStash(contentDir string) []WikiOp {
	path := OpsStashPath(contentDir)
	body, err := os.ReadFile(path)
	if err != nil {
		// Missing or unreadable — degrade silently. The diff still
		// records what changed.
		return nil
	}
	ops := parseOps(string(body))
	// Best-effort truncation. If we can't truncate, the next session's
	// EnsureOpsStash will reset it; not worth blocking finalize over.
	_ = os.Truncate(path, 0)
	return ops
}

// parseOps walks `.wiki-ops` line by line and returns the recognised
// `[wiki-op]` entries in document order. Lines that aren't tags, or
// tags whose body doesn't match a known primitive shape, are silently
// skipped — the agent's commentary or a malformed line shouldn't blow
// up the parse.
func parseOps(body string) []WikiOp {
	var ops []WikiOp
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "[wiki-op]")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		// First whitespace-delimited token is the primitive name; the
		// remainder is the per-primitive body (sources / targets).
		var kind, args string
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			kind, args = rest[:i], strings.TrimSpace(rest[i+1:])
		} else {
			kind, args = rest, ""
		}
		op, ok := parseOpBody(kind, args)
		if !ok {
			continue
		}
		ops = append(ops, op)
	}
	return ops
}

// parseOpBody dispatches on the primitive name and parses the rest of
// the line into a WikiOp. Returns false for unknown primitives or
// malformed bodies.
func parseOpBody(kind, body string) (WikiOp, bool) {
	switch kind {
	case "split":
		// "<src> → <dst1>, <dst2>, ..."
		left, right, ok := splitArrow(body)
		if !ok {
			return WikiOp{}, false
		}
		src := strings.TrimSpace(left)
		targets := splitCommaList(right)
		if src == "" || len(targets) == 0 {
			return WikiOp{}, false
		}
		return WikiOp{Kind: OpSplit, Sources: []string{src}, Targets: targets}, true
	case "merge":
		// "<src> into <dst>" — also accept "<srcs...> → <dst>" so the
		// rendered log entry style ("a → b") parses if the agent ever
		// echoes their own log format back.
		if left, right, ok := splitInto(body); ok {
			sources := splitCommaList(left)
			tgt := strings.TrimSpace(right)
			if len(sources) == 0 || tgt == "" {
				return WikiOp{}, false
			}
			return WikiOp{Kind: OpMerge, Sources: sources, Targets: []string{tgt}}, true
		}
		if left, right, ok := splitArrow(body); ok {
			sources := splitCommaList(left)
			tgt := strings.TrimSpace(right)
			if len(sources) == 0 || tgt == "" {
				return WikiOp{}, false
			}
			return WikiOp{Kind: OpMerge, Sources: sources, Targets: []string{tgt}}, true
		}
		return WikiOp{}, false
	case "rename":
		left, right, ok := splitArrow(body)
		if !ok {
			return WikiOp{}, false
		}
		src := strings.TrimSpace(left)
		tgt := strings.TrimSpace(right)
		if src == "" || tgt == "" {
			return WikiOp{}, false
		}
		return WikiOp{Kind: OpRename, Sources: []string{src}, Targets: []string{tgt}}, true
	case "retire":
		src := strings.TrimSpace(body)
		if src == "" {
			return WikiOp{}, false
		}
		return WikiOp{Kind: OpRetire, Sources: []string{src}}, true
	default:
		return WikiOp{}, false
	}
}

// splitArrow splits on the first occurrence of either "→" (U+2192) or
// the ASCII "->". The Unicode arrow is the canonical form in the
// design; ASCII is a graceful fallback for keyboards without it.
func splitArrow(s string) (left, right string, ok bool) {
	if i := strings.Index(s, "→"); i >= 0 {
		return s[:i], s[i+len("→"):], true
	}
	if i := strings.Index(s, "->"); i >= 0 {
		return s[:i], s[i+len("->"):], true
	}
	return "", "", false
}

// splitInto splits on the first " into " (whitespace-bracketed). The
// surrounding spaces avoid clipping a filename that happens to contain
// the substring.
func splitInto(s string) (left, right string, ok bool) {
	const sep = " into "
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return "", "", false
}

// splitCommaList breaks a comma-separated list into trimmed tokens,
// dropping empties.
func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// formatOpLine renders a single WikiOp as it appears in log.md under
// the operations group. The arrow form is canonical regardless of how
// the agent originally phrased the tag, so log entries read uniformly.
func formatOpLine(op WikiOp) string {
	switch op.Kind {
	case OpSplit:
		return fmt.Sprintf("split: %s → %s",
			strings.Join(op.Sources, ", "),
			strings.Join(op.Targets, ", "))
	case OpMerge:
		return fmt.Sprintf("merge: %s → %s",
			strings.Join(op.Sources, ", "),
			strings.Join(op.Targets, ", "))
	case OpRename:
		return fmt.Sprintf("rename: %s → %s",
			strings.Join(op.Sources, ", "),
			strings.Join(op.Targets, ", "))
	case OpRetire:
		return fmt.Sprintf("retire: %s", strings.Join(op.Sources, ", "))
	default:
		return ""
	}
}

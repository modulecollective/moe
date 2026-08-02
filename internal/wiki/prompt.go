package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IngestPromptSection is the wiki-specific block that gets appended to
// the system prompt for an ingest session. It carries the per-instance
// framing, the on-disk shape contract, and the schema rules so the agent
// knows what it may and may not restructure during this turn.
//
// The section is one cohesive markdown block — the caller layers it
// into the prompt alongside soul.md, the stage fragment, and the
// operational core via the same `\n---\n\n` separator buildSystemPrompt
// uses for the rest of the prompt.
func IngestPromptSection(cfg Config) string {
	var b strings.Builder
	b.WriteString(wikiPreamble(cfg))
	b.WriteString(`Schema rules:
The doc set is fixed. Do not create, rename, or delete topic docs unless
the operator has explicitly authorized that change in this session.
Edits land inside the existing docs.
`)
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// wikiPreamble is the shared "what is this wiki" header that opens the
// ingest prompt section: the per-instance body, the `## Wiki: <name>`
// header, the absolute content-dir path, and the on-disk shape contract.
func wikiPreamble(cfg Config) string {
	var b strings.Builder
	if body := strings.TrimSpace(cfg.IngestPrompt); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "## Wiki: %s\n\n", cfg.Name)
	fmt.Fprintf(&b, "Wiki content directory:\n  %s\n\n", cfg.ContentDir)
	b.WriteString("On-disk shape:\n")
	b.WriteString("- log.md — append-only changelog. Engine-managed; do not edit.\n")
	b.WriteString("- checkpoint.json — last-ran SHAs. Engine-managed; do not edit.\n")
	for _, d := range cfg.ManagedDocs {
		fmt.Fprintf(&b, "- %s — %s.", d.Filename, d.Title)
		if purpose := strings.TrimSpace(d.Purpose); purpose != "" {
			fmt.Fprintf(&b, " %s", purpose)
		}
		fmt.Fprintf(&b, "%s\n", managedDocSizeAnnotation(cfg.ContentDir, d))
	}
	b.WriteString("\nNo index.md, no topics/. The doc set is fixed; cross-links\n")
	b.WriteString("between managed docs are flat sibling refs (e.g.\n")
	b.WriteString("[architecture](architecture.md)).\n\n")
	return b.String()
}

// managedDocSizeAnnotation renders a doc's current on-disk size for the
// preamble's shape list. A measured size is a fact and travels to any
// project; a declared budget is a per-corpus invention that goes stale
// the pass after it's set. The size is the whole lever — the only thing
// a prose corpus responds to is telling the writer how big it has
// gotten, and the ingest prompt carries the judgement about what to do
// with the number.
//
// A doc that isn't on disk yet gets no annotation: the missing-doc
// finding already says so, and "0.0 KB" would read as a measurement.
func managedDocSizeAnnotation(contentDir string, d ManagedDoc) string {
	info, err := os.Stat(filepath.Join(contentDir, d.Filename))
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (%.1f KB)", float64(info.Size())/1024)
}

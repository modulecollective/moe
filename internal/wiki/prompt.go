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
		fmt.Fprintf(&b, "%s\n", managedDocBudgetAnnotation(cfg.ContentDir, d))
	}
	b.WriteString("\nNo index.md, no topics/. The doc set is fixed; cross-links\n")
	b.WriteString("between managed docs are flat sibling refs (e.g.\n")
	b.WriteString("[architecture](architecture.md)).\n\n")
	return b.String()
}

func managedDocBudgetAnnotation(contentDir string, d ManagedDoc) string {
	if d.SoftBudgetKB <= 0 {
		return ""
	}
	info, err := os.Stat(filepath.Join(contentDir, d.Filename))
	if err != nil {
		return fmt.Sprintf(" (size unavailable; soft budget %d KB)", d.SoftBudgetKB)
	}
	sizeKB := float64(info.Size()) / 1024
	annotation := fmt.Sprintf(" (%.1f KB; soft budget %d KB)", sizeKB, d.SoftBudgetKB)
	if info.Size() > int64(d.SoftBudgetKB)*1024 {
		annotation += " ⚠ over budget — compress this pass"
	}
	return annotation
}

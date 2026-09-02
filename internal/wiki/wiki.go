// Package wiki holds the structural checks and path helpers for the
// project digital twin — a closed-schema doc set under
// projects/<p>/digital-twin/ — plus the lore catalog that shares its
// reference-section shape.
//
// It was once an engine: it owned an ingest loop, a checkpoint, an
// events window, a session-end finalize that appended to log.md, and a
// schema mode generic over two wikis. Both wikis are gone as workflows.
// A project's knowledge/ tree is a plain doc tree checked by
// internal/knowledge; the twin is a plain doc tree any sdlc stage may
// write, checked by Scan here and gated at stage exit. What's left is
// the scan, the doc-set declaration it scans against, and the prompt
// sections that point an agent at both trees.
package wiki

// Config is the input to Scan: which directory holds the docs, and
// which docs the schema declares. ContentDir is absolute.
//
// It is a struct rather than two arguments because the finding types
// name docs relative to ContentDir, and pairing the two at the call
// site keeps a caller from scanning one twin's directory against
// another's doc list.
type Config struct {
	// ContentDir is the absolute path to the doc set's on-disk dir
	// (<root>/projects/<p>/digital-twin).
	ContentDir string
	// ManagedDocs is the hard-fixed set of docs the schema declares.
	// Required. Order is the order findings render in.
	ManagedDocs []ManagedDoc
}

// ManagedDoc names one of the closed schema's hard-fixed docs. Twin's
// five (vision / architecture / patterns / operations / glossary) live
// in internal/cli/twin.go; the scan treats them as opaque
// (filename, title, purpose).
type ManagedDoc struct {
	// Filename is the path under ContentDir (e.g. "vision.md").
	// Flat — closed-schema has no topics/ subfolder.
	Filename string
	// Title is the human-readable heading rendered into hygiene
	// reports.
	Title string
	// Purpose is a one-line "what this doc is for", carried so a
	// caller that needs to describe the doc set has it in one place.
	Purpose string
}

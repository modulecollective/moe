package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanCleanTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Knowledge\n\n- [DNS basics](topics/dns-basics.md)\n- [TCP](topics/tcp.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns-basics.md"),
		"# DNS basics\n\nSee also [TCP](tcp.md).\n")
	writeFile(t, filepath.Join(dir, "topics", "tcp.md"),
		"# TCP\n\nThree-way handshake is described elsewhere.\n")

	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !f.IsEmpty() {
		t.Fatalf("expected clean tree, got %+v", f)
	}
}

func TestScanFlagsOrphanBrokenAndEmpty(t *testing.T) {
	dir := t.TempDir()
	// index.md links to one missing file and one real file; one
	// topic doc is on disk but unreferenced (orphan); one topic doc
	// is empty; one topic doc has a broken cross-link.
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Knowledge\n\n- [DNS](topics/dns.md)\n- [Phantom](topics/missing.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\nSee [TCP handshake](tcp-handshake.md) for context.\n")
	writeFile(t, filepath.Join(dir, "topics", "orphan.md"),
		"# Orphan\n\nNobody links here.\n")
	writeFile(t, filepath.Join(dir, "topics", "stub.md"), "# Stub\n")

	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got, want := f.Orphans, []string{"topics/orphan.md", "topics/stub.md"}; !equalStrings(got, want) {
		t.Errorf("orphans: got %v want %v", got, want)
	}
	if got, want := f.MissingFromIndex, []string{"topics/missing.md"}; !equalStrings(got, want) {
		t.Errorf("missing-from-index: got %v want %v", got, want)
	}
	if got, want := f.EmptyDocs, []string{"topics/stub.md"}; !equalStrings(got, want) {
		t.Errorf("empty: got %v want %v", got, want)
	}
	if len(f.BrokenLinks) != 1 ||
		f.BrokenLinks[0].From != "topics/dns.md" ||
		f.BrokenLinks[0].Target != "topics/tcp-handshake.md" {
		t.Errorf("broken links: %+v", f.BrokenLinks)
	}
	if got, want := f.Count(), 5; got != want {
		t.Errorf("Count()=%d want %d", got, want)
	}
}

func TestScanIgnoresExternalLinksAndAnchors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"),
		"# Knowledge\n\n- [DNS](topics/dns.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\n"+
			"See [RFC 1035](https://example.com/rfc1035) and "+
			"the [intro section](#intro) for context. "+
			"Cross-link to [self](dns.md#section-2).\n")
	f, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.BrokenLinks) != 0 {
		t.Errorf("external + anchor links should not flag as broken: %+v", f.BrokenLinks)
	}
}

// A topic doc linking back up to ../index.md is the documented shape and
// must not read as a broken link.
func TestScanAllowsUpLinkToIndex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Knowledge\n\n- [DNS](topics/dns.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\nBack to the [index](../index.md).\n")
	f, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsEmpty() {
		t.Errorf("up-link to index should be clean, got %+v", f)
	}
}

func TestScanMissingDirIsCleanFindings(t *testing.T) {
	// A project with no knowledge tree has nothing to get wrong.
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan on missing dir: %v", err)
	}
	if !f.IsEmpty() {
		t.Errorf("expected empty findings on missing dir, got %+v", f)
	}
}

// The half-built case: index.md exists at the top but topics/ has not
// been created yet. Same as a missing dir — empty findings, no error —
// so a first knowledge write doesn't trip a phantom failure.
func TestScanMissingTopicsDirIsCleanFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Knowledge\n\nNothing yet.\n")
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan with missing topics dir: %v", err)
	}
	if !f.IsEmpty() {
		t.Errorf("expected empty findings with no topics dir, got %+v", f)
	}
}

// Without an index.md every topic doc would read as an orphan, which
// says nothing useful — the orphan check waits for a catalog to exist.
func TestScanWithoutIndexSkipsOrphans(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "topics", "dns.md"), "# DNS\n\nbody\n")
	f, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Orphans) != 0 {
		t.Errorf("no index means no orphan findings, got %+v", f.Orphans)
	}
}

func TestRenderGroupsByCategory(t *testing.T) {
	f := Findings{
		Orphans:          []string{"topics/orphan.md"},
		MissingFromIndex: []string{"topics/phantom.md"},
		BrokenLinks:      []BrokenLink{{From: "topics/a.md", Target: "topics/b.md"}},
		EmptyDocs:        []string{"topics/stub.md"},
	}
	got := Render(f)
	for _, want := range []string{
		"## Knowledge tree findings",
		"**Topic docs missing from index.md**",
		"- topics/orphan.md",
		"**Index entries pointing at missing files**",
		"- topics/phantom.md",
		"**Broken cross-links**",
		"- topics/a.md → topics/b.md",
		"**Empty or stub docs**",
		"- topics/stub.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderEmptyReturnsEmpty(t *testing.T) {
	if got := Render(Findings{}); got != "" {
		t.Errorf("expected empty string for clean findings, got %q", got)
	}
}

// equalStrings compares two string slices element-wise, treating an
// empty slice and nil as equal.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

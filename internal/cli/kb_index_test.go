package cli

import "testing"

// applyIndexPatch is the load-bearing surgery for kb shelve. These
// tests cover the cases the stage fragment's re-shelve rules call out:
// first-ever shelve into an empty index, first bullet in a new
// category, bullet appended to an existing category, re-shelve under
// the same category (updates the hook), and re-shelve that moves to a
// different category (bullet moves, old file would be rm'd by the
// caller).
func TestApplyIndexPatchEmptyIndex(t *testing.T) {
	got := applyIndexPatch("", "dns-basics", "Networking",
		"- [DNS basics](networking/dns-basics.md) — how resolvers actually work")
	want := "# knowledge\n\n## Networking\n\n- [DNS basics](networking/dns-basics.md) — how resolvers actually work\n"
	if got != want {
		t.Fatalf("empty-index patch mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestApplyIndexPatchNewCategoryOnPopulatedIndex(t *testing.T) {
	body := "# knowledge\n\n## Databases\n\n- [Postgres WAL](databases/postgres-wal.md) — write-ahead log mechanics\n"
	got := applyIndexPatch(body, "dns-basics", "Networking",
		"- [DNS basics](networking/dns-basics.md) — how resolvers actually work")
	want := "# knowledge\n\n## Databases\n\n- [Postgres WAL](databases/postgres-wal.md) — write-ahead log mechanics\n\n## Networking\n\n- [DNS basics](networking/dns-basics.md) — how resolvers actually work\n"
	if got != want {
		t.Fatalf("new-category patch mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestApplyIndexPatchAppendsToExistingCategory(t *testing.T) {
	body := "# knowledge\n\n## Networking\n\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n"
	got := applyIndexPatch(body, "dns-basics", "Networking",
		"- [DNS basics](networking/dns-basics.md) — how resolvers actually work")
	want := "# knowledge\n\n## Networking\n\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n- [DNS basics](networking/dns-basics.md) — how resolvers actually work\n"
	if got != want {
		t.Fatalf("append-to-existing patch mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestApplyIndexPatchReshelveSameCategoryUpdatesHook(t *testing.T) {
	body := "# knowledge\n\n## Networking\n\n- [DNS basics](networking/dns-basics.md) — old hook\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n"
	got := applyIndexPatch(body, "dns-basics", "Networking",
		"- [DNS basics](networking/dns-basics.md) — refreshed hook")
	// The topic's bullet is removed from its original position and
	// appended to the end of the category — insertion-order
	// preserved for the remaining entries.
	want := "# knowledge\n\n## Networking\n\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n- [DNS basics](networking/dns-basics.md) — refreshed hook\n"
	if got != want {
		t.Fatalf("reshelve-same patch mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestApplyIndexPatchReshelveDifferentCategoryMovesBullet(t *testing.T) {
	body := "# knowledge\n\n## Networking\n\n- [DNS basics](networking/dns-basics.md) — old hook\n\n## Databases\n\n- [Postgres WAL](databases/postgres-wal.md) — write-ahead log mechanics\n"
	got := applyIndexPatch(body, "dns-basics", "Databases",
		"- [DNS basics](databases/dns-basics.md) — refreshed hook about storage")
	// Old bullet removed from Networking; new bullet appended to
	// Databases. Empty-ish Networking section is left alone (out of
	// scope to rebalance the shelf as a whole).
	want := "# knowledge\n\n## Networking\n\n\n## Databases\n\n- [Postgres WAL](databases/postgres-wal.md) — write-ahead log mechanics\n- [DNS basics](databases/dns-basics.md) — refreshed hook about storage\n"
	if got != want {
		t.Fatalf("reshelve-different patch mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestApplyIndexPatchCaseInsensitiveCategoryMatch(t *testing.T) {
	// Model said "networking" (lowercase); existing section is
	// "Networking" (title case). We reuse the existing section
	// rather than creating a second one.
	body := "# knowledge\n\n## Networking\n\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n"
	got := applyIndexPatch(body, "dns-basics", "networking",
		"- [DNS basics](networking/dns-basics.md) — how resolvers actually work")
	want := "# knowledge\n\n## Networking\n\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n- [DNS basics](networking/dns-basics.md) — how resolvers actually work\n"
	if got != want {
		t.Fatalf("case-insensitive category match mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestFindExistingBulletPath(t *testing.T) {
	body := "# knowledge\n\n## Networking\n\n- [DNS basics](networking/dns-basics.md) — old hook\n- [TCP handshake](networking/tcp-handshake.md) — SYN/ACK step by step\n"
	if got := findExistingBulletPath(body, "dns-basics"); got != "networking/dns-basics.md" {
		t.Fatalf("findExistingBulletPath=%q, want %q", got, "networking/dns-basics.md")
	}
	if got := findExistingBulletPath(body, "no-such-topic"); got != "" {
		t.Fatalf("findExistingBulletPath for missing topic=%q, want %q", got, "")
	}
}

func TestSlugifyCategory(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Networking", "networking"},
		{"CI/CD", "ci-cd"},
		{"Operating Systems", "operating-systems"},
		{"  Whitespace  ", "whitespace"},
		{"Foo -- Bar", "foo-bar"},
		{"", ""},
	}
	for _, c := range cases {
		got := slugifyCategory(c.in)
		if got != c.want {
			t.Errorf("slugifyCategory(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseShelveDecisionAcceptsPlainJSON(t *testing.T) {
	d, err := parseShelveDecision([]byte(`{"category": "Networking", "hook": "how resolvers actually work"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Category != "Networking" || d.Hook != "how resolvers actually work" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseShelveDecisionToleratesLeadingWhitespace(t *testing.T) {
	d, err := parseShelveDecision([]byte("\n\n   {\"category\":\"A\",\"hook\":\"b\"}\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Category != "A" || d.Hook != "b" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseShelveDecisionToleratesCodeFence(t *testing.T) {
	d, err := parseShelveDecision([]byte("```json\n{\"category\":\"A\",\"hook\":\"b\"}\n```\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Category != "A" || d.Hook != "b" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseShelveDecisionRejectsEmpty(t *testing.T) {
	if _, err := parseShelveDecision([]byte("   \n")); err == nil {
		t.Fatal("expected error for empty output")
	}
}

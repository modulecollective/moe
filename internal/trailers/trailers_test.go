package trailers

import "testing"

func TestBlockEmpty(t *testing.T) {
	if got := (Block{}).String(); got != "" {
		t.Fatalf("zero-value Block: want empty string, got %q", got)
	}
}

func TestBlockCanonicalOrderAllFields(t *testing.T) {
	b := Block{
		Run:        "r",
		Project:    "p",
		Workflow:   "w",
		Document:   "d",
		Session:    "s",
		PR:         "pr",
		Merged:     "m",
		Closed:     "c",
		PromotedTo: "pt",
		FromRun:    "fr",
		Idea:       "i",
		ReopenOf:   "ro",
	}
	want := "MoE-Run: r\n" +
		"MoE-Project: p\n" +
		"MoE-Workflow: w\n" +
		"MoE-Document: d\n" +
		"MoE-Session: s\n" +
		"MoE-PR: pr\n" +
		"MoE-Merged: m\n" +
		"MoE-Closed: c\n" +
		"MoE-Promoted-To: pt\n" +
		"MoE-From-Run: fr\n" +
		"MoE-Idea: i\n" +
		"MoE-Reopen-Of: ro\n"
	if got := b.String(); got != want {
		t.Fatalf("String() mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestBlockElidesEmptyFields(t *testing.T) {
	b := Block{Run: "r", Project: "p", Workflow: "w"}
	want := "MoE-Run: r\nMoE-Project: p\nMoE-Workflow: w\n"
	if got := b.String(); got != want {
		t.Fatalf("partial Block: got %q want %q", got, want)
	}
}

func TestBlockTrailingNewline(t *testing.T) {
	// `subject + "\n\n" + block.String()` must end in '\n'. A missing
	// terminator on the last trailer would silently break callers that
	// concatenate block to a subject and pass to git commit -m.
	got := Block{Run: "r"}.String()
	if got == "" || got[len(got)-1] != '\n' {
		t.Fatalf("last line missing trailing newline: %q", got)
	}
}

func TestBlockOrderIndependentOfFieldAssignment(t *testing.T) {
	// Renders should not depend on which fields the caller populated
	// first — the struct field order is what determines wire order.
	a := Block{Idea: "i", Run: "r", Project: "p"}
	b := Block{Project: "p", Run: "r", Idea: "i"}
	if a.String() != b.String() {
		t.Fatalf("output depends on assignment order:\n a: %q\n b: %q", a.String(), b.String())
	}
	want := "MoE-Run: r\nMoE-Project: p\nMoE-Idea: i\n"
	if a.String() != want {
		t.Fatalf("got %q want %q", a.String(), want)
	}
}

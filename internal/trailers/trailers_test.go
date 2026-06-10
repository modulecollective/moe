package trailers

import "testing"

func TestBlockEmpty(t *testing.T) {
	if got := (Block{}).String(); got != "" {
		t.Fatalf("zero-value Block: want empty string, got %q", got)
	}
}

func TestBlockCanonicalOrderAllFields(t *testing.T) {
	b := Block{
		Run:           "r",
		Project:       "p",
		Workflow:      "w",
		Document:      "d",
		Session:       "s",
		PR:            "pr",
		Merged:        "m",
		Closed:        "c",
		PromotedTo:    "pt",
		FromRun:       "fr",
		Idea:          "i",
		IdeaMovedFrom: "imf",
		ReopenOf:      "ro",
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
		"MoE-Idea-Moved-From: imf\n" +
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

func TestBlockChainedTrailersRepeatInOrder(t *testing.T) {
	// `chain edit` saves a single commit that batches one MoE-Chained-To
	// per new edge and one MoE-Chained-To-Removed per replaced edge.
	// The slice fields preserve emit order so a reviewer reading the
	// commit body sees adds then removes in the order the editor
	// produced them.
	b := Block{
		Run: "r",
		ChainedTo: []string{
			"projA/parent1 projB/child1",
			"projA/parent2 projA/child2",
		},
		ChainedToRemoved: []string{
			"projA/parent1 projA/old1",
		},
	}
	want := "MoE-Run: r\n" +
		"MoE-Chained-To: projA/parent1 projB/child1\n" +
		"MoE-Chained-To: projA/parent2 projA/child2\n" +
		"MoE-Chained-To-Removed: projA/parent1 projA/old1\n"
	if got := b.String(); got != want {
		t.Fatalf("chained trailers:\n got: %q\nwant: %q", got, want)
	}
}

func TestBlockChainedTrailersElideWhenEmpty(t *testing.T) {
	// Nil and empty slices both elide — same contract as empty
	// strings on other fields.
	b := Block{Run: "r", ChainedTo: nil, ChainedToRemoved: []string{}}
	if got := b.String(); got != "MoE-Run: r\n" {
		t.Fatalf("empty chained trailers should elide: got %q", got)
	}
}

func TestBlockChoreTrailers(t *testing.T) {
	got := (Block{
		Run:          "readme-refresh",
		Project:      "moe",
		Chore:        "moe/readme-refresh",
		ChoreTouched: []string{"moe/twin-reflect", "moe/readme-refresh"},
	}).String()
	want := "MoE-Run: readme-refresh\n" +
		"MoE-Project: moe\n" +
		"MoE-Chore: moe/readme-refresh\n" +
		"MoE-Chore-Touched: moe/twin-reflect\n" +
		"MoE-Chore-Touched: moe/readme-refresh\n"
	if got != want {
		t.Fatalf("trailers:\n%s\nwant:\n%s", got, want)
	}
}

func TestBlockChoreSkippedTrailer(t *testing.T) {
	// `moe chore skip` writes a trailer-only commit carrying exactly one
	// MoE-Chore-Skipped, rendered adjacent to (just after) MoE-Chore.
	got := (Block{ChoreSkipped: "moe/readme-refresh"}).String()
	want := "MoE-Chore-Skipped: moe/readme-refresh\n"
	if got != want {
		t.Fatalf("trailers:\n%s\nwant:\n%s", got, want)
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

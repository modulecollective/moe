package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// The gate's `runs` entries have two spellings — an object that mints, a
// string that names a run already parked — and a tagged idea is a parked
// run, so a survey naming one reaches for the string. Every test here is
// about that spelling reaching the promotion seam the object one already
// had, and about the three shapes it must *not* change.

// seedTaggedIdea parks an in-progress idea carrying an sdlc workflow tag
// — what `moe idea tag` and a filer's `(sdlc)` stamp both write.
func seedTaggedIdea(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, projectID, run.Options{
		ID:        slug,
		Workflow:  "idea",
		PromoteTo: "sdlc",
		SeedDocs:  map[string]string{"idea": "# " + slug + "\n\nThe canvas that becomes the design seed.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// seedDesignOnlyIdea parks a tagged idea whose licence is the narrower
// one: promote it, but ride one design turn and hold. Written the way
// TagIdea writes it — both fields, one claim.
func seedDesignOnlyIdea(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	seedTaggedIdea(t, root, projectID, slug)
	// Through the real writer, so the fixture can't drift from what the
	// operator's `moe idea tag --design-only` actually lands — and so it
	// leaves the tree clean, which the promote below requires.
	if err := runopen.TagIdea(root, projectID, slug, "sdlc", true); err != nil {
		t.Fatal(err)
	}
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// TestPromoteCarriesTheDesignOnlyTag is the whole carry: the operator
// stamped the licence on the idea from their phone, and the run the
// pulse mints has to come out byte-identical in shape to a design-only
// spawn — SpawnedBy plus DesignOnly — because every mechanism
// downstream (the kick floor's hold, the one-stage ride, the stage
// prompt) reads only those two and knows nothing about where the run
// came from.
func TestPromoteCarriesTheDesignOnlyTag(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "worth-a-think", Why: "the operator tagged it"}}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the idea's licence carried onto the run")
	}
	if dest.SpawnedBy == "" {
		t.Errorf("spawned_by = %q, want the run to still mark itself machine-minted", dest.SpawnedBy)
	}
	if !strings.Contains(errb.String(), "to sdlc, design only run") {
		t.Errorf("stderr = %q, want the promote line to say the ride is short", errb.String())
	}
}

// TestPlainTaggedIdeaPromotesWithoutTheBit is the regression guard on
// the carry: the ordinary tag is still a licence to ship, so the run it
// mints must not hold at design.
func TestPlainTaggedIdeaPromotesWithoutTheBit(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "just-ship-it")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "just-ship-it", Why: "the operator tagged it"}}, io.Discard, &errb)
	dest, err := run.Load(root, "moe", minted["just-ship-it"])
	if err != nil {
		t.Fatal(err)
	}
	if dest.DesignOnly {
		t.Errorf("run.json design_only = true, want a plain tag to stay a licence to ship")
	}
}

// TestDesignOnlySpecOnADesignOnlyTaggedIdeaIsIgnored: the survey reads
// the open backlog, where a design-only tag now renders as "design
// only" — so it will echo the marker back as `design_only: true` on the
// spec. Skipping for that would make the operator's own licence
// unusable the moment the board is quoted, and the spec and the tag
// agree anyway. Warn and promote.
func TestDesignOnlySpecOnADesignOnlyTaggedIdeaIsIgnored(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	spec := designOnlySpec("worth-a-think")
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{spec}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "ignoring design_only for tagged idea moe/worth-a-think; the tag carries it") {
		t.Errorf("stderr = %q, want the redundant flag named and ignored", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the tag's bit on the run")
	}
	// The idea canvas is the seed, not the spec's brief — unchanged by
	// this run, and the reason the tag can mean "the seed is a brief".
	if !strings.Contains(errb.String(), "ignoring design body for tagged idea") {
		t.Errorf("stderr = %q, want the spec's design body still ignored", errb.String())
	}
}

// TestDesignOnlySpecWithNoBodyOnADesignOnlyTaggedIdeaPromotes is the
// same quote with the survey's other half followed: the guidance says
// omit `design` for a tagged idea, and the board says "design only", so
// the honest spec is a bare slug plus the marker. The no-body refusal
// exists because a fresh mint would have nothing but a title and a why
// for a seed — an idea's canvas *is* that brief, so there is nothing
// missing here to refuse over.
func TestDesignOnlySpecWithNoBodyOnADesignOnlyTaggedIdeaPromotes(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	spec := designOnlySpec("worth-a-think")
	spec.Design = ""
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{spec}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	if strings.Contains(errb.String(), "no design body") {
		t.Errorf("stderr = %q, want no no-body refusal — the idea canvas is the brief", errb.String())
	}
	if !strings.Contains(errb.String(), "ignoring design_only for tagged idea moe/worth-a-think; the tag carries it") {
		t.Errorf("stderr = %q, want the redundant flag named and ignored", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the tag's bit on the run")
	}
}

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// tagState reads back the pair the tag writes. They are one claim, so
// every assertion below reads them together.
func tagState(t *testing.T, root, projectID, slug string) (string, bool) {
	t.Helper()
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return md.PromoteTo, md.DesignOnly
}

// TestIdeaTagDesignOnlyStampsBothFields is the rung this run exists to
// add: from the phone the operator's choice was "ship it" or "wait for
// a terminal", and --design-only is the middle. The bit lands beside
// the tag because it is the same claim the run field already makes —
// the seed is a brief — travelling with the canvas that carries it.
func TestIdeaTagDesignOnlyStampsBothFields(t *testing.T) {
	root := tagFixture(t, "moe", "worth-a-think")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/worth-a-think", "--design-only"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "tagged idea moe/worth-a-think → sdlc (design only)") {
		t.Fatalf("success line should name the narrower licence: %q", out.String())
	}
	if wf, only := tagState(t, root, "moe", "worth-a-think"); wf != "sdlc" || !only {
		t.Fatalf("promote_to=%q design_only=%v, want sdlc/true", wf, only)
	}
	if head := gitLog(t, root, "-1", "--format=%s"); !strings.Contains(head, "Tag idea moe/worth-a-think (sdlc, design only)") {
		t.Fatalf("commit subject wrong: %q", head)
	}
}

// TestIdeaTagDesignOnlyFlagOrderDoesNotMatter: the operator types this
// on a phone. reorderFlags is why `moe idea tag moe/x --design-only`
// parses at all — flag's own parser stops at the first non-flag.
func TestIdeaTagDesignOnlyFlagOrderDoesNotMatter(t *testing.T) {
	for _, args := range [][]string{
		{"idea", "tag", "moe/either-way", "--design-only"},
		{"idea", "tag", "--design-only", "moe/either-way"},
		{"idea", "tag", "moe/either-way", "sdlc", "--design-only"},
	} {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			root := tagFixture(t, "moe", "either-way")
			var out, errb bytes.Buffer
			if code := Run(args, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if wf, only := tagState(t, root, "moe", "either-way"); wf != "sdlc" || !only {
				t.Fatalf("promote_to=%q design_only=%v, want sdlc/true", wf, only)
			}
		})
	}
}

// TestIdeaTagPlainRetagClearsDesignOnly: the tag is one claim — "ship"
// or "design, then I look" — never both. So the plain verb is how the
// operator promotes their own idea from the narrow rung to the wide
// one, and untag clears the pair.
func TestIdeaTagPlainRetagClearsDesignOnly(t *testing.T) {
	root := tagFixture(t, "moe", "changed-my-mind")
	if code := Run([]string{"idea", "tag", "moe/changed-my-mind", "--design-only"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup design-only tag failed")
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/changed-my-mind"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if wf, only := tagState(t, root, "moe", "changed-my-mind"); wf != "sdlc" || only {
		t.Fatalf("promote_to=%q design_only=%v, want the narrowing cleared", wf, only)
	}

	if code := Run([]string{"idea", "tag", "moe/changed-my-mind", "--design-only"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("re-narrow failed")
	}
	if code := Run([]string{"idea", "untag", "moe/changed-my-mind"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("untag failed")
	}
	if wf, only := tagState(t, root, "moe", "changed-my-mind"); wf != "" || only {
		t.Fatalf("promote_to=%q design_only=%v after untag, want both cleared", wf, only)
	}
}

// TestIdeaTagDesignOnlyRepeatIsANoOp: the same-state re-tap stays a
// clean no-op with no empty commit, and the notice names the state it
// found rather than the workflow alone — otherwise a double-tap on the
// narrow rung reads back as if it were the wide one.
func TestIdeaTagDesignOnlyRepeatIsANoOp(t *testing.T) {
	root := tagFixture(t, "moe", "double-tapped")
	if code := Run([]string{"idea", "tag", "moe/double-tapped", "--design-only"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup tag failed")
	}
	before := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/double-tapped", "--design-only"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already tagged → sdlc (design only)") {
		t.Fatalf("expected the found state named, got: %q", out.String())
	}
	if after := gitLog(t, root, "-1", "--format=%H"); after != before {
		t.Fatalf("no-op tag minted a commit: %q → %q", before, after)
	}
}

// TestTagIdeaRefusesDesignOnlyWithoutAWorkflow: design-only narrows a
// licence, it is not one. No CLI spelling can produce the pair — untag
// passes false — so the refusal guards the seam against a future caller
// that would otherwise write "design-only and fenced", a state nothing
// downstream can read.
func TestTagIdeaRefusesDesignOnlyWithoutAWorkflow(t *testing.T) {
	root := tagFixture(t, "moe", "no-such-licence")

	err := runopen.TagIdea(root, "moe", "no-such-licence", "", true)
	if !errors.Is(err, runopen.ErrNotTaggableIdea) {
		t.Fatalf("err = %v, want ErrNotTaggableIdea", err)
	}
	if wf, only := tagState(t, root, "moe", "no-such-licence"); wf != "" || only {
		t.Fatalf("promote_to=%q design_only=%v, want the refusal to write nothing", wf, only)
	}
}

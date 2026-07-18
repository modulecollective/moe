package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestFollowupClaimSlugsGenericRule: a past-tense filing claim with a
// backticked slug is a claim; instruction prose that merely names the
// mechanism is not. The verb gate is the whole precision story here —
// the word "followup" appears in every stage fragment.
func TestFollowupClaimSlugsGenericRule(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{"past-tense claim", "Filed as followup `pulse-tail-stale-binary`.", []string{"pulse-tail-stale-binary"}},
		{"hyphenated spelling", "Left a follow-up `fix-the-thing` for this.", []string{"fix-the-thing"}},
		{"cross-project claim", "Filed followup `claudia/inherit-nginx`.", []string{"claudia/inherit-nginx"}},
		{"two on a line", "Filed followups `one-thing` and `two-thing`.", []string{"one-thing", "two-thing"}},
		{"instruction prose", "Leave a followup via the `moe-bureaucracy` skill.", nil},
		{"no slug", "Filed a followup for the stale-binary case.", nil},
		{"verb without followup", "Filed the `some-run` design.", nil},
		{"single-word backtick", "Filed a followup after running `vet`.", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := followupClaimSlugs([]byte("# Doc\n\n" + tc.line + "\n"))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFollowupClaimSlugsNewFilingsSection: a pulse's report lines don't
// contain the word "followup", so the generic rule misses them
// entirely. The section is the claim.
func TestFollowupClaimSlugsNewFilingsSection(t *testing.T) {
	canvas := []byte(`# Pulse

## What landed

- ` + "`not-a-filing`" + ` — this is prose under a different heading

## New filings

- ` + "`stale-binary-refile`" + ` — chains refile settled observations
- ` + "`other-thing`" + ` — something else

## Pull next

- ` + "`ranked-idea`" + ` — already open, not a filing
`)
	got := followupClaimSlugs(canvas)
	want := []string{"stale-binary-refile", "other-thing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestUnverifiedFollowupClaims is the end-to-end predicate: a claim is
// clean when the slug is in followups.md (either box state) or names a
// run that exists; only a slug found nowhere is reported.
func TestUnverifiedFollowupClaims(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "existing-run-2026-07-18", "sdlc", run.StatusClosed, time.Now().Local(), nil)
	seedRun(t, root, "moe", "the-run", "sdlc", run.StatusInProgress, time.Now().Local(), map[string]string{
		"design": "# Design\n\nFiled as followup `never-filed-thing`.\n" +
			"Filed as followup `in-followups-file`.\n" +
			"Filed as followup `promoted-already`.\n" +
			"Filed as followup `existing-run`.\n",
		"code": "# Code\n\nnothing claimed here\n",
	})
	writeFollowups(t, root, "moe", "the-run", "# Follow-ups\n\n"+
		"- [ ] `in-followups-file` — something\n"+
		"- [x] `promoted-already` — already harvested\n")

	md, err := run.Load(root, "moe", "the-run")
	if err != nil {
		t.Fatal(err)
	}
	got := unverifiedFollowupClaims(root, md)
	if want := []string{"never-filed-thing"}; strings.Join(got["design"], ",") != strings.Join(want, ",") {
		t.Errorf("design claims: got %v, want %v", got["design"], want)
	}
	if len(got["code"]) != 0 {
		t.Errorf("code doc reported %v, want nothing", got["code"])
	}
}

// TestWarnUnverifiedFollowupClaimsIsAdvisory pins the output shape and
// the warn-only posture: the message names the doc and the slug, and it
// goes to stderr rather than becoming an error.
func TestWarnUnverifiedFollowupClaimsIsAdvisory(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "the-run", "sdlc", run.StatusInProgress, time.Now().Local(), map[string]string{
		"design": "# Design\n\nFiled as followup `never-filed-thing`.\n",
	})

	md, err := run.Load(root, "moe", "the-run")
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	warnUnverifiedFollowupClaims(root, md, &errb)
	if !strings.Contains(errb.String(), "canvas design claims followup `never-filed-thing` that was never filed") {
		t.Errorf("stderr=%q, want the claim warning", errb.String())
	}
}

// TestFiledFollowupSlugsToleratesMalformedFile: parseFollowups is total
// and rejects a malformed file, but an advisory check must not turn one
// into noise — a malformed entry simply doesn't count as filed, and the
// well-formed lines around it still do.
func TestFiledFollowupSlugsToleratesMalformedFile(t *testing.T) {
	root := newTestBureaucracy(t)
	writeFollowups(t, root, "moe", "the-run", "# Follow-ups\n\n"+
		"- [ ] no-backticks-here — malformed\n"+
		"- [ ] `good-one` — fine\n")

	filed := filedFollowupSlugs(root, "moe", "the-run")
	if !filed["good-one"] {
		t.Error("well-formed entry not recognised as filed")
	}
	if len(filed) != 1 {
		t.Errorf("filed=%v, want only the well-formed entry", filed)
	}
}

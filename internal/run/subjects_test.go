package run

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestSubjectMatchersAcceptBothForms pins the read-back headlines in
// both the current grammar and the legacy form the journal's older
// commits carry, plus the near-misses that share trailers with them and
// must stay out.
func TestSubjectMatchersAcceptBothForms(t *testing.T) {
	const p, r = "moe", "loop"
	for _, tc := range []struct {
		subject               string
		turn, advance, push   bool
		diagnostic, turnDocOK bool
		turnDoc               string
	}{
		{subject: "Update design for moe/loop", turn: true, turnDocOK: true, turnDoc: "design"},
		{subject: "work: update design", turn: true, turnDocOK: true, turnDoc: "design"},
		{subject: "Advance design for moe/loop", advance: true},
		{subject: "advance: design", advance: true},
		{subject: "Archive design diagnostic for moe/loop — canvas unchanged", diagnostic: true},
		{subject: "diagnostic: moe/loop design canvas unchanged", diagnostic: true},
		{subject: "Push moe/loop", push: true},
		{subject: "Push moe/loop — merged", push: true},
		{subject: "Sync moe/loop — merged", push: true},
		{subject: "push: moe/loop merged", push: true},
		{subject: "sync: moe/loop closed", push: true},
		// Near-misses.
		{subject: "Start design for moe/loop"},
		{subject: "work: start session for design"},
		{subject: "Update model-stylesheet.css"},
		{subject: "Update design for moe/other"},
		{subject: "Update design for moe/loop — extra"},
		{subject: "Update design for lightpool/loop"},
		{subject: "Advance design for moe/other"},
		{subject: "Archive build diagnostic for moe/loop — canvas unchanged"},
		{subject: "Update push for moe/loop", turnDocOK: true, turnDoc: "push"},
		{subject: "Start push for moe/loop"},
		{subject: "Bump project pointers"},
	} {
		if got := IsWorkTurnSubject(tc.subject, p, r, "design"); got != tc.turn {
			t.Errorf("IsWorkTurnSubject(%q) = %v, want %v", tc.subject, got, tc.turn)
		}
		if got := IsAdvanceSubject(tc.subject, p, r, "design"); got != tc.advance {
			t.Errorf("IsAdvanceSubject(%q) = %v, want %v", tc.subject, got, tc.advance)
		}
		if got := isPushRecordSubject(tc.subject); got != tc.push {
			t.Errorf("isPushRecordSubject(%q) = %v, want %v", tc.subject, got, tc.push)
		}
		if got := IsDiagnosticSubject(tc.subject, p, r, "design"); got != tc.diagnostic {
			t.Errorf("IsDiagnosticSubject(%q) = %v, want %v", tc.subject, got, tc.diagnostic)
		}
		doc, ok := workTurnDoc(tc.subject, p, r)
		if ok != tc.turnDocOK || doc != tc.turnDoc {
			t.Errorf("workTurnDoc(%q) = %q, %v; want %q, %v", tc.subject, doc, ok, tc.turnDoc, tc.turnDocOK)
		}
	}
}

// TestBuildersRoundTripThroughMatchers keeps each writer and its reader
// on one definition.
func TestBuildersRoundTripThroughMatchers(t *testing.T) {
	if s := WorkTurnSubject("moe", "loop", "build"); !IsWorkTurnSubject(s, "moe", "loop", "build") {
		t.Errorf("WorkTurnSubject %q not matched", s)
	}
	if doc, ok := workTurnDoc(WorkTurnSubject("moe", "loop", "build"), "moe", "loop"); !ok || doc != "build" {
		t.Errorf("workTurnDoc(WorkTurnSubject) = %q, %v", doc, ok)
	}
	if s := AdvanceSubject("moe", "loop", "build"); !IsAdvanceSubject(s, "moe", "loop", "build") {
		t.Errorf("AdvanceSubject %q not matched", s)
	}
	if s := DiagnosticSubject("moe", "loop", "build", "unchanged"); !IsDiagnosticSubject(s, "moe", "loop", "build") {
		t.Errorf("DiagnosticSubject %q not matched", s)
	}
	for _, s := range []string{PushSubject("moe", "loop", ""), PushSubject("moe", "loop", "merged"), SyncSubject("moe", "loop", "MERGED")} {
		if !isPushRecordSubject(s) {
			t.Errorf("push record %q not matched", s)
		}
	}
	if got := SyncSubject("moe", "loop", "MERGED"); got != "Sync moe/loop — merged" {
		t.Errorf("SyncSubject = %q", got)
	}
}

// TestReadersSeeMixedHistory is the in-flight run at install time: its
// journal holds legacy headlines from the old binary and current ones
// from the new. Every reader must see both, and the newest wins.
func TestReadersSeeMixedHistory(t *testing.T) {
	root := newTestRoot(t)
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	i := 0
	commit := func(subject, docID string) string {
		t.Helper()
		i++
		stamp := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		gittest.RunWithEnv(t, root, []string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp},
			"commit", "--allow-empty", "-m", subject+"\n\n"+
				"MoE-Run: loop\nMoE-Project: moe\nMoE-Workflow: sdlc\nMoE-Document: "+docID+"\n")
		return gittest.HeadSHA(t, root)
	}
	legacyDesign := commit("work: update design", "design")
	legacyAdvance := commit("advance: review", "review")
	newBuild := commit("Update build for moe/loop", "build")
	newAdvance := commit("Advance build for moe/loop", "build")
	commit("Start review for moe/loop", "review")

	for _, tc := range []struct {
		name, doc, want string
		latest          func(root, projectID, runID, docID string) (string, time.Time, error)
	}{
		{"work turn, legacy", "design", legacyDesign, LatestWorkTurnSHA},
		{"work turn, current", "build", newBuild, LatestWorkTurnSHA},
		{"work turn, none", "review", "", LatestWorkTurnSHA},
		{"advance, legacy", "review", legacyAdvance, LatestAdvanceSHA},
		{"advance, current", "build", newAdvance, LatestAdvanceSHA},
	} {
		got, _, err := tc.latest(root, "moe", "loop", tc.doc)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	docs, err := WorkTurnDocs(root, "moe", "loop")
	if err != nil || strings.Join(docs, ",") != "build,design" {
		t.Errorf("WorkTurnDocs = %v, %v; want [build design]", docs, err)
	}

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []WorkTurnKey{{"moe", "loop", "design"}, {"moe", "loop", "build"}} {
		if idx.WorkTurnTime[k].IsZero() {
			t.Errorf("WorkTurnTime[%v] missing", k)
		}
	}
	if _, ok := idx.WorkTurnTime[WorkTurnKey{"moe", "loop", "review"}]; ok {
		t.Errorf("session start registered as a work turn")
	}
	for _, k := range []WorkTurnKey{{"moe", "loop", "review"}, {"moe", "loop", "build"}} {
		if idx.AdvanceTime[k].IsZero() {
			t.Errorf("AdvanceTime[%v] missing", k)
		}
	}
}

package run

import "strings"

// Commit headlines moe reads back. Every bureaucracy headline follows
// one grammar — `<Verb> <object> [for] <project>/<run>[ — <qualifier>]`
// — and most are written inline at their call sites because nothing
// reads them. The few below are load-bearing: a reader decides stage
// satisfaction, turn counts or ship provenance from them. Each has its
// builder and its matcher side by side, so writer and reader cannot
// drift. The journal keeps every headline it was ever written with, so
// each matcher also accepts the legacy lowercase `area: detail` form
// those commits carry.

// WorkTurnSubject is the headline of a committed stage turn.
func WorkTurnSubject(projectID, runID, docID string) string {
	return "Update " + docID + " for " + projectID + "/" + runID
}

// AdvanceSubject is the headline of the click-forward marker that
// records a stage done without a turn.
func AdvanceSubject(projectID, runID, docID string) string {
	return "Advance " + docID + " for " + projectID + "/" + runID
}

// PushSubject is the headline of a push record; qualifier is "" for the
// PR-opened record and "merged" for the fast-forward ship.
func PushSubject(projectID, runID, qualifier string) string {
	return withQualifier("Push "+projectID+"/"+runID, qualifier)
}

// SyncSubject is the headline `moe sync` writes when it reconciles an
// upstream merge or close of the run's PR.
func SyncSubject(projectID, runID, status string) string {
	return withQualifier("Sync "+projectID+"/"+runID, strings.ToLower(status))
}

// DiagnosticSubject is the headline of an archived stage refusal.
func DiagnosticSubject(projectID, runID, docID, reason string) string {
	return "Archive " + docID + " diagnostic for " + projectID + "/" + runID + " — canvas " + reason
}

func withQualifier(s, qualifier string) string {
	if qualifier == "" {
		return s
	}
	return s + " — " + qualifier
}

// IsWorkTurnSubject reports whether subject is a work-turn headline for
// the run's document. Session starts (`Start <doc> for …`), merges and
// push records carry the same trailers and are kept out here.
func IsWorkTurnSubject(subject, projectID, runID, docID string) bool {
	return subject == WorkTurnSubject(projectID, runID, docID) || subject == "work: update "+docID
}

// IsAdvanceSubject is IsWorkTurnSubject for the advance marker.
func IsAdvanceSubject(subject, projectID, runID, docID string) bool {
	return subject == AdvanceSubject(projectID, runID, docID) || subject == "advance: "+docID
}

// IsDiagnosticSubject reports whether subject is an archived refusal of
// the run's document.
func IsDiagnosticSubject(subject, projectID, runID, docID string) bool {
	return strings.HasPrefix(subject, "Archive "+docID+" diagnostic for "+projectID+"/"+runID+" — canvas ") ||
		strings.HasPrefix(subject, "diagnostic: "+projectID+"/"+runID+" "+docID+" canvas ")
}

// workTurnDoc returns the document a work-turn headline names, for a
// reader that knows the run but not the document. ok is false for any
// other headline.
func workTurnDoc(subject, projectID, runID string) (string, bool) {
	doc, ok := strings.CutPrefix(subject, "work: update ")
	if !ok {
		doc, ok = strings.CutPrefix(subject, "Update ")
		if ok {
			doc, ok = strings.CutSuffix(doc, " for "+projectID+"/"+runID)
		}
	}
	if !ok || !isDocID(doc) {
		return "", false
	}
	return doc, true
}

// isDocID is the `[a-z0-9-]+` shape the legacy reader's grep pinned.
func isDocID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// isPushRecordSubject reports whether subject is one of the terminal
// records that mean a run actually shipped: the push records push.go
// writes and the sync record sync.go writes when it reconciles an
// upstream merge. Everything else carrying Document: push is stage
// traffic on the way there (`Start push for …`, `Update push for …`).
func isPushRecordSubject(subject string) bool {
	for _, prefix := range []string{"Push ", "Sync ", "push: ", "sync: "} {
		if strings.HasPrefix(subject, prefix) {
			return true
		}
	}
	return false
}

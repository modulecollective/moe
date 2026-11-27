package run

// Commit headlines with more than one writer. Every bureaucracy headline
// follows one grammar — `<Verb> <object> [for] <project>/<run>[ —
// <qualifier>]` — and is for the eye only: moe reads a commit through
// its trailers and never parses a headline or body text (MoE-Kind says
// what kind of commit it is). Single-site headlines are written inline;
// these are shared so their writers agree.

// WorkTurnSubject is the headline of a committed stage turn.
func WorkTurnSubject(projectID, runID, docID string) string {
	return "Update " + docID + " for " + projectID + "/" + runID
}

// PushSubject is the headline of a push record; qualifier is "" for the
// PR-opened record and "merged" for the fast-forward ship.
func PushSubject(projectID, runID, qualifier string) string {
	s := "Push " + projectID + "/" + runID
	if qualifier != "" {
		s += " — " + qualifier
	}
	return s
}

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

// selectBackendSession keeps metadata dirtiness separate from launch freshness.
// Legacy IDs are evidence candidates, never a projection of the active backend.
func selectBackendSession(a agent.Agent, doc *run.Document, backend, cwd, mirrorPath, workRoot string, stderr io.Writer) (id string, fresh, changed bool, err error) {
	id, keyed := doc.Sessions[backend]
	if !keyed {
		if run.ValidSessionID(doc.Session) {
			found, probeErr := a.TranscriptExists(doc.Session, cwd)
			if probeErr != nil {
				return "", false, false, fmt.Errorf("session: stat transcript: %w", probeErr)
			}
			if !found {
				outcome, restoreErr := a.RestoreTranscript(doc.Session, cwd, "")
				if restoreErr != nil {
					return "", false, false, fmt.Errorf("session: restore transcript: %w", restoreErr)
				}
				found = outcome.Result != agent.RestoreMissing
				if found {
					reportSessionRestore(stderr, workRoot, doc.Session, outcome)
				}
			}
			if found {
				id = doc.Session
			}
		}
		if id == "" {
			id, err = a.TranscriptSessionID(mirrorPath)
			if err != nil {
				return "", false, false, fmt.Errorf("session: read mirror identity: %w", err)
			}
		}
		if id != "" {
			doc.SetSession(backend, id)
			changed = true
		}
	}
	if !run.ValidSessionID(id) {
		// Even when healing an invalid keyed ID, reject corrupt mirror evidence
		// before the executor can overwrite it with a new conversation.
		if _, err := a.TranscriptSessionID(mirrorPath); err != nil {
			return "", false, false, fmt.Errorf("session: read mirror identity: %w", err)
		}
		id, err = run.NewSessionID()
		if err != nil {
			return "", false, false, err
		}
		if doc.Session != "" && !keyed {
			moePrintf(stderr, "legacy session %s has no recoverable %s identity; starting fresh as %s (prior chat history not recoverable)\n", doc.Session, backend, id)
		}
		doc.SetSession(backend, id)
		return id, true, true, nil
	}
	found, err := a.TranscriptExists(id, cwd)
	if err != nil {
		return "", false, false, fmt.Errorf("session: stat transcript: %w", err)
	}
	if found {
		return id, false, changed, nil
	}

	// A mirror is eligible only for its own identity. Native cache recovery
	// can still succeed when the mirror is absent or stale.
	mirrorID, err := a.TranscriptSessionID(mirrorPath)
	if err != nil {
		return "", false, false, fmt.Errorf("session: read mirror identity: %w", err)
	}
	if mirrorID != id {
		mirrorPath = ""
	}
	outcome, err := a.RestoreTranscript(id, cwd, mirrorPath)
	if err != nil {
		return "", false, false, fmt.Errorf("session: restore transcript: %w", err)
	}
	if outcome.Result != agent.RestoreMissing {
		reportSessionRestore(stderr, workRoot, id, outcome)
		return id, false, changed, nil
	}
	next, err := run.NewSessionID()
	if err != nil {
		return "", false, false, err
	}
	moePrintf(stderr, "session %s not found anywhere; starting fresh as %s (prior chat history not recoverable)\n", id, next)
	doc.SetSession(backend, next)
	return next, true, true, nil
}

func reportSessionRestore(stderr io.Writer, workRoot, id string, outcome agent.RestoreOutcome) {
	switch outcome.Result {
	case agent.RestoreFromCache:
		moePrintf(stderr, "session %s recovered from cache (%s)\n", id, outcome.Source)
	case agent.RestoreFromMirror:
		src := outcome.Source
		if rel, err := filepath.Rel(workRoot, src); err == nil && !strings.HasPrefix(rel, "..") {
			src = rel
		}
		moePrintf(stderr, "session %s restored from %s\n", id, src)
	}
}

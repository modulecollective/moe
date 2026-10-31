package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/modulecollective/moe/internal/run"
)

// TranscriptSessionID streams native records so large tool output does not
// impose a scanner token limit. Every identity-bearing record must agree.
func (Agent) TranscriptSessionID(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("codex: open transcript %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var sessionID string
	for {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		err := dec.Decode(&record)
		if errors.Is(err, io.EOF) {
			return sessionID, nil
		}
		if err != nil {
			return "", fmt.Errorf("codex: decode transcript %s: %w", path, err)
		}
		if record.Type != "session_meta" {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if len(record.Payload) != 0 {
			if err := json.Unmarshal(record.Payload, &payload); err != nil {
				return "", fmt.Errorf("codex: decode session metadata %s: %w", path, err)
			}
		}
		id := payload.ID
		if id == "" {
			continue
		}
		if !run.ValidSessionID(id) {
			return "", fmt.Errorf("codex: invalid session ID %q in %s", id, path)
		}
		if sessionID != "" && sessionID != id {
			return "", fmt.Errorf("codex: conflicting session IDs %q and %q in %s", sessionID, id, path)
		}
		sessionID = id
	}
}
